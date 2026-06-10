from __future__ import annotations

import base64
import time
import uuid
from typing import Optional

import httpx
from kubernetes import client as k8s_client
from kubernetes.client.rest import ApiException

from agent_run.types import ExecResult, FileInfo, ForkInfo, SandboxInfo, SandboxPhase


API_GROUP = "agentrun.dev"
API_VERSION = "v1alpha1"
POLL_INTERVAL = 0.05


class SandboxFiles:
    """File operations on a sandbox."""

    def __init__(self, sandbox: Sandbox):
        self._sandbox = sandbox

    def read(self, path: str) -> str:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/read",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path},
            headers=self._sandbox._auth_headers(),
        )
        resp.raise_for_status()
        return resp.json()["content"]

    def read_bytes(self, path: str) -> bytes:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/read",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path, "binary": True},
            headers=self._sandbox._auth_headers(),
        )
        resp.raise_for_status()
        return bytes.fromhex(resp.json()["content"])

    def write(self, path: str, content: str | bytes, mode: int = 0o644) -> None:
        data: dict = {"sandbox": self._sandbox._sandbox_ref, "path": path, "mode": mode}
        if isinstance(content, bytes):
            data["content"] = content.hex()
            data["binary"] = True
        else:
            data["content"] = content

        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/write",
            json=data,
            headers=self._sandbox._auth_headers(),
        )
        resp.raise_for_status()

    def list(self, path: str = "/") -> list[FileInfo]:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/list",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path},
            headers=self._sandbox._auth_headers(),
        )
        resp.raise_for_status()
        return [
            FileInfo(
                name=f["name"],
                is_dir=f["is_dir"],
                size=f["size"],
                mode=f.get("mode", 0),
                modified_at=f.get("modified_at"),
            )
            for f in resp.json()["entries"]
        ]

    def exists(self, path: str) -> bool:
        try:
            self.list(path)
            return True
        except httpx.HTTPStatusError:
            return False

    def remove(self, path: str) -> None:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/remove",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path},
            headers=self._sandbox._auth_headers(),
        )
        resp.raise_for_status()

    def mkdir(self, path: str) -> None:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/mkdir",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path},
            headers=self._sandbox._auth_headers(),
        )
        resp.raise_for_status()


class Sandbox:
    """A running sandbox instance."""

    def __init__(
        self,
        name: str,
        namespace: str,
        pool: str,
        api: k8s_client.CustomObjectsApi,
        core_api: Optional[k8s_client.CoreV1Api] = None,
        _endpoint: Optional[str] = None,
        _phase: SandboxPhase = SandboxPhase.PENDING,
    ):
        self.name = name
        self.namespace = namespace
        self.pool = pool
        self._api = api
        # Reads the <name>-sandbox-token Secret; defaults to a CoreV1Api on
        # the same loaded kube config the CustomObjectsApi came from.
        self._core_api = core_api if core_api is not None else k8s_client.CoreV1Api()
        self._endpoint = _endpoint
        self._phase = _phase
        self._sandbox_id: Optional[str] = None
        self._token: Optional[str] = None
        self._http = httpx.Client(timeout=30.0)
        self.files = SandboxFiles(self)

    @property
    def _base_url(self) -> str:
        if not self._endpoint:
            self._wait_ready()
        return f"http://{self._endpoint}/v1"

    @property
    def _sandbox_ref(self) -> str:
        if self._sandbox_id is None:
            self._wait_ready()
        # Fall back to the claim name when the cluster never reported a
        # sandboxID; forkd registers sandboxes under the claim name in
        # that case.
        return self._sandbox_id or self.name

    @property
    def endpoint(self) -> str:
        if not self._endpoint:
            self._wait_ready()
        return self._endpoint

    @property
    def phase(self) -> SandboxPhase:
        return self._phase

    def _auth_headers(self) -> dict[str, str]:
        """Bearer auth for the sandbox API; empty when no token is known."""
        if self._token:
            return {"Authorization": f"Bearer {self._token}"}
        return {}

    def _load_token(self) -> None:
        """Read the sandbox API bearer token from <name>-sandbox-token.

        The controller creates the Secret alongside the Ready claim (or
        fork). A missing Secret is tolerated: the sandbox stays tokenless
        and the API will answer 401 to every call, which surfaces the
        misconfiguration without crashing here. The token value is held in
        memory only.
        """
        try:
            secret = self._core_api.read_namespaced_secret(
                name=f"{self.name}-sandbox-token", namespace=self.namespace
            )
        except ApiException:
            return
        data = secret.data or {}
        token_b64 = data.get("token")
        if token_b64:
            self._token = base64.b64decode(token_b64).decode()

    def _wait_ready(self, timeout: float = 30.0) -> None:
        deadline = time.time() + timeout
        while time.time() < deadline:
            obj = self._api.get_namespaced_custom_object(
                group=API_GROUP,
                version=API_VERSION,
                namespace=self.namespace,
                plural="sandboxclaims",
                name=self.name,
            )
            status = obj.get("status", {})
            self._phase = SandboxPhase(status.get("phase", "Pending"))
            self._endpoint = status.get("endpoint")
            self._sandbox_id = status.get("sandboxID")

            if self._phase == SandboxPhase.READY and self._endpoint:
                self._load_token()
                return
            if self._phase == SandboxPhase.FAILED:
                raise RuntimeError(f"sandbox {self.name} failed")

            time.sleep(POLL_INTERVAL)

        raise TimeoutError(f"sandbox {self.name} not ready after {timeout}s")

    def exec(
        self,
        command: str,
        timeout: int = 30,
        working_dir: str = "/workspace",
        env: Optional[dict[str, str]] = None,
    ) -> ExecResult:
        """Execute a command in the sandbox."""
        payload: dict = {
            "sandbox": self._sandbox_ref,
            "command": command,
            "timeout": timeout,
            "working_dir": working_dir,
        }
        if env:
            payload["env"] = env

        resp = self._http.post(
            f"{self._base_url}/exec",
            json=payload,
            timeout=timeout + 5,
            headers=self._auth_headers(),
        )
        resp.raise_for_status()
        data = resp.json()

        return ExecResult(
            exit_code=data["exit_code"],
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
            exec_time_ms=data.get("exec_time_ms", 0),
        )

    def fork(self, n: int = 1, pause_source: bool = False) -> list[Sandbox]:
        """Fork this sandbox into n independent copies."""
        fork_name = f"{self.name}-fork-{uuid.uuid4().hex[:6]}"

        fork_obj = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}",
            "kind": "SandboxFork",
            "metadata": {
                "name": fork_name,
                "namespace": self.namespace,
            },
            "spec": {
                "sourceRef": {"name": self.name},
                "replicas": n,
                "pauseSource": pause_source,
            },
        }

        self._api.create_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self.namespace,
            plural="sandboxforks",
            body=fork_obj,
        )

        return self._wait_forks(fork_name, n)

    def _wait_forks(self, fork_name: str, expected: int, timeout: float = 30.0) -> list[Sandbox]:
        deadline = time.time() + timeout
        while time.time() < deadline:
            obj = self._api.get_namespaced_custom_object(
                group=API_GROUP,
                version=API_VERSION,
                namespace=self.namespace,
                plural="sandboxforks",
                name=fork_name,
            )
            status = obj.get("status", {})
            forks = status.get("forks", [])

            ready = [f for f in forks if f.get("phase") == "Ready"]
            if len(ready) >= expected:
                result = []
                for f in ready:
                    sandbox = Sandbox(
                        name=f["name"],
                        namespace=self.namespace,
                        pool=self.pool,
                        api=self._api,
                        core_api=self._core_api,
                        _endpoint=f.get("endpoint"),
                        _phase=SandboxPhase.READY,
                    )
                    sandbox._sandbox_id = f.get("sandboxID")
                    # Each fork has its own token Secret (<forkID>-sandbox-token);
                    # the source's token does not open the fork.
                    sandbox._load_token()
                    result.append(sandbox)
                return result

            time.sleep(POLL_INTERVAL)

        raise TimeoutError(f"forks not ready after {timeout}s")

    def info(self) -> SandboxInfo:
        """Get current sandbox info from the cluster."""
        obj = self._api.get_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self.namespace,
            plural="sandboxclaims",
            name=self.name,
        )
        status = obj.get("status", {})
        return SandboxInfo(
            name=self.name,
            phase=SandboxPhase(status.get("phase", "Pending")),
            endpoint=status.get("endpoint", ""),
            node=status.get("node", ""),
            sandbox_id=status.get("sandboxID", ""),
            fork_time_ms=status.get("forkTimeMs", 0),
            pool=self.pool,
        )

    def terminate(self) -> None:
        """Terminate this sandbox."""
        self._api.delete_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self.namespace,
            plural="sandboxclaims",
            name=self.name,
        )
        self._phase = SandboxPhase.TERMINATING
        self._http.close()

    def __enter__(self) -> Sandbox:
        return self

    def __exit__(self, *args) -> None:
        self.terminate()

    def __repr__(self) -> str:
        return f"Sandbox(name={self.name!r}, phase={self._phase.value!r}, endpoint={self._endpoint!r})"
