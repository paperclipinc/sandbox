from __future__ import annotations

import base64
import json
import threading
import time
import uuid
from typing import Callable, Iterable, Optional

import httpx
from kubernetes import client as k8s_client
from kubernetes.client.rest import ApiException

from mitos._envelope import raise_for_status, raise_for_status_stream
from mitos.errors import AgentRunError
from mitos.pty import PtyHandle
from mitos.types import (
    BackgroundProcess,
    ExecResult,
    Execution,
    ExecutionError,
    FileInfo,
    ForkInfo,
    Result,
    SandboxInfo,
    SandboxPhase,
)


def _decode_stream_bytes(value) -> str:
    """Go marshals a []byte JSON field as base64; decode it back to text. A
    plain string (some kernels send text directly) is returned unchanged."""
    if value is None:
        return ""
    if isinstance(value, str):
        try:
            return base64.b64decode(value).decode("utf-8", "replace")
        except Exception:
            return value
    return str(value)


def _parse_run_code_stream(
    lines: Iterable[bytes],
    on_stdout: Optional[Callable[[str], None]],
    on_stderr: Optional[Callable[[str], None]],
    on_result: Optional[Callable[[Result], None]],
) -> Execution:
    """Folds an NDJSON ExecStreamFrame stream into an Execution, firing the
    callbacks live as frames arrive. Result and error payloads are tenant code
    output and are never logged here."""
    ex = Execution()
    saw_exit = False
    for raw in lines:
        if not raw.strip():
            continue
        frame = json.loads(raw)
        kind = frame.get("kind")
        if kind == "stdout":
            text = _decode_stream_bytes(frame.get("stdout"))
            ex.logs["stdout"].append(text)
            if on_stdout:
                on_stdout(text)
        elif kind == "stderr":
            text = _decode_stream_bytes(frame.get("stderr"))
            ex.logs["stderr"].append(text)
            if on_stderr:
                on_stderr(text)
        elif kind == "result":
            payload = frame.get("result") or {}
            data = payload.get("data") or {}
            text = payload.get("text") or ""
            is_main = bool(text)
            result = Result(data=data, is_main_result=is_main)
            ex.results.append(result)
            if is_main and text:
                ex.text = text
            if on_result:
                on_result(result)
        elif kind == "error":
            payload = frame.get("error") or {}
            ex.error = ExecutionError(
                name=payload.get("name", ""),
                value=payload.get("value", ""),
                traceback=payload.get("traceback", []) or [],
            )
        elif kind == "exit":
            saw_exit = True
            break
    if not saw_exit:
        # The body ended before the terminal exit frame: the stream was
        # truncated or dropped. Surface it as an error rather than a misleading
        # clean Execution success.
        raise RuntimeError(
            "run_code stream ended before the terminal exit frame: "
            "the connection was truncated or dropped; the result is unknown"
        )
    return ex


API_GROUP = "mitos.run"
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
        raise_for_status(resp, token=self._sandbox._token)
        return resp.json()["content"]

    def read_bytes(self, path: str) -> bytes:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/read",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path, "binary": True},
            headers=self._sandbox._auth_headers(),
        )
        raise_for_status(resp, token=self._sandbox._token)
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
        raise_for_status(resp, token=self._sandbox._token)

    def list(self, path: str = "/") -> list[FileInfo]:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/list",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path},
            headers=self._sandbox._auth_headers(),
        )
        raise_for_status(resp, token=self._sandbox._token)
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
        except AgentRunError as exc:
            if exc.status == 404:
                return False
            raise

    def remove(self, path: str) -> None:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/remove",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path},
            headers=self._sandbox._auth_headers(),
        )
        raise_for_status(resp, token=self._sandbox._token)

    def mkdir(self, path: str) -> None:
        resp = self._sandbox._http.post(
            f"{self._sandbox._base_url}/files/mkdir",
            json={"sandbox": self._sandbox._sandbox_ref, "path": path},
            headers=self._sandbox._auth_headers(),
        )
        raise_for_status(resp, token=self._sandbox._token)


class SandboxPty:
    """PTY operations on a sandbox: create an interactive terminal."""

    def __init__(self, sandbox: "Sandbox"):
        self._sandbox = sandbox

    def create(
        self,
        on_data: Callable[[bytes], None],
        cols: int = 80,
        rows: int = 24,
    ) -> PtyHandle:
        """Open an interactive PTY (a shell) in the sandbox. Output bytes are
        delivered to on_data on a background thread. Returns a PtyHandle with
        send_input(bytes), resize(cols, rows), kill(), and wait() -> exit_code.

        The transport is a WebSocket to the sandbox API's /v1/pty, gated by the
        per-sandbox bearer token (sent in the Authorization header, never
        logged)."""
        base = self._sandbox._base_url  # http://<endpoint>/v1
        ws_base = base.replace("http://", "ws://", 1).replace("https://", "wss://", 1)
        ref = self._sandbox._sandbox_ref
        url = f"{ws_base}/pty?sandbox={ref}&cols={cols}&rows={rows}"
        return PtyHandle(url=url, token=self._sandbox._token, on_data=on_data)


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
        self.pty = SandboxPty(self)

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

    def wait_until_ready(self, timeout: float = 30.0) -> "Sandbox":
        """Block until the sandbox is Ready (Modal-style), then return self so it
        chains: sb = c.sandbox("python").wait_until_ready(). Raises AgentRunError
        with code sandbox_failed or ready_timeout otherwise. Idempotent: returns
        immediately if already Ready with an endpoint."""
        if self._phase == SandboxPhase.READY and self._endpoint:
            return self
        self._wait_ready(timeout=timeout)
        return self

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
                raise AgentRunError(
                    f"sandbox {self.name} failed",
                    code="sandbox_failed",
                    cause=f"claim {self.name} reached the Failed phase",
                    remediation="Inspect the SandboxClaim status conditions and the pool capacity.",
                )

            time.sleep(POLL_INTERVAL)

        raise AgentRunError(
            f"sandbox {self.name} not ready after {timeout}s",
            code="ready_timeout",
            cause=f"claim {self.name} did not reach Ready within {timeout}s",
            remediation="Raise the timeout, or check the controller is reconciling and the pool has capacity.",
        )

    def exec(
        self,
        command: str,
        timeout: int = 30,
        working_dir: str = "/workspace",
        env: Optional[dict[str, str]] = None,
        on_stdout: Optional[Callable[[bytes], None]] = None,
        on_stderr: Optional[Callable[[bytes], None]] = None,
    ) -> ExecResult:
        """Execute a command in the sandbox.

        When on_stdout or on_stderr is given, output is streamed over
        /v1/exec/stream (NDJSON) and the callbacks fire per chunk as bytes
        arrive; the returned ExecResult still carries the full aggregate. With
        no callbacks the blocking /v1/exec path is used unchanged.
        """
        if on_stdout is None and on_stderr is None:
            return self._exec_blocking(command, timeout, working_dir, env)
        out_parts: list[bytes] = []
        err_parts: list[bytes] = []
        exit_code, exec_time_ms = self._stream(
            command, timeout, working_dir, env,
            lambda b: (out_parts.append(b), on_stdout(b) if on_stdout else None),
            lambda b: (err_parts.append(b), on_stderr(b) if on_stderr else None),
        )
        return ExecResult(
            exit_code=exit_code,
            stdout=b"".join(out_parts).decode("utf-8", "replace"),
            stderr=b"".join(err_parts).decode("utf-8", "replace"),
            exec_time_ms=exec_time_ms,
        )

    def _exec_blocking(self, command, timeout, working_dir, env) -> ExecResult:
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
        raise_for_status(resp, token=self._token)
        data = resp.json()
        return ExecResult(
            exit_code=data["exit_code"],
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
            exec_time_ms=data.get("exec_time_ms", 0),
        )

    def run_code(
        self,
        code: str,
        language: str = "python",
        timeout: int = 60,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
        on_result: Optional[Callable[[Result], None]] = None,
    ) -> Execution:
        """Run a code snippet in the sandbox's stateful kernel.

        State persists across run_code calls for the sandbox lifetime. Returns an
        Execution with text (REPL last value), logs (stdout/stderr), results
        (rich display artifacts), and error. Streams via the callbacks as frames
        arrive. Requires a base image with the code-interpreter kernel; without
        it the Execution carries a KernelUnavailable error.
        """
        payload: dict = {
            "sandbox": self._sandbox_ref,
            "code": code,
            "language": language,
            "timeout": timeout,
        }
        with self._http.stream(
            "POST",
            f"{self._base_url}/run_code/stream",
            json=payload,
            timeout=timeout + 10,
            headers=self._auth_headers(),
        ) as resp:
            raise_for_status_stream(resp, token=self._token)
            return _parse_run_code_stream(
                resp.iter_lines(),
                on_stdout,
                on_stderr,
                on_result,
            )

    def _stream(
        self, command, timeout, working_dir, env, on_out, on_err,
        client=None, on_response=None,
    ):
        """Opens /v1/exec/stream and feeds chunks to on_out/on_err. Returns
        (exit_code, exec_time_ms). Raises on transport error frames and on a
        stream that ends before the terminal exit frame.

        Streams on `client` when given (a dedicated per-stream httpx client so a
        kill() can tear down only that connection), otherwise on the shared
        Sandbox client. When on_response is given it is called with the live
        streaming Response so a kill() can close that exact connection and
        unblock the in-flight read deterministically, independent of how the
        installed httpx version handles Client.close()."""
        http = client if client is not None else self._http
        payload: dict = {
            "sandbox": self._sandbox_ref,
            "command": command,
            "timeout": timeout,
            "working_dir": working_dir,
        }
        if env:
            payload["env"] = env
        exit_code = 0
        exec_time_ms = 0.0
        saw_exit = False
        with http.stream(
            "POST",
            f"{self._base_url}/exec/stream",
            json=payload,
            timeout=None,
            headers=self._auth_headers(),
        ) as resp:
            if on_response is not None:
                on_response(resp)
            raise_for_status_stream(resp, token=self._token)
            for line in resp.iter_lines():
                if not line:
                    continue
                frame = json.loads(line)
                if "exit_code" in frame and "stream" not in frame:
                    exit_code = frame["exit_code"]
                    exec_time_ms = frame.get("exec_time_ms", 0.0)
                    saw_exit = True
                    if frame.get("error"):
                        raise RuntimeError(f"exec stream error: {frame['error']}")
                    continue
                data = base64.b64decode(frame["data"]) if frame.get("data") else b""
                if frame.get("stream") == "stderr":
                    on_err(data)
                else:
                    on_out(data)
        if not saw_exit:
            # The body ended before the terminal exit frame: the stream was
            # truncated or dropped. Surface it as an error rather than a
            # misleading exit_code=0 success.
            raise RuntimeError(
                "exec stream ended before the terminal exit frame: "
                "the connection was truncated or dropped; the exit code is unknown"
            )
        return exit_code, exec_time_ms

    def exec_background(
        self,
        command: str,
        timeout: int = 86400,
        working_dir: str = "/workspace",
        env: Optional[dict[str, str]] = None,
        on_stdout: Optional[Callable[[bytes], None]] = None,
        on_stderr: Optional[Callable[[bytes], None]] = None,
    ) -> "BackgroundProcess":
        """Start a long-running command and return a handle. The command starts
        running immediately on a background thread; wait() blocks for the
        aggregate result. kill() closes only this stream's own client so forkd
        cancels the guest process group, leaving the shared Sandbox client (and
        every other exec/file call) untouched. Default timeout is one day so a
        background server is not reaped by the per-exec timeout."""
        out_parts: list[bytes] = []
        err_parts: list[bytes] = []
        # Resolve the endpoint and token on the calling thread so a failure to
        # become ready surfaces here, not silently inside the drain thread.
        base_url = self._base_url
        self._sandbox_ref  # noqa: B018  force readiness/id resolution
        # A dedicated client so kill() tears down only this stream, never the
        # shared Sandbox client that other exec/file calls ride on.
        stream_http = httpx.Client(timeout=30.0)

        state: dict = {"result": None, "error": None, "response": None}
        done = threading.Event()
        resp_lock = threading.Lock()

        def capture_response(resp) -> None:
            with resp_lock:
                state["response"] = resp

        def drain_thread() -> None:
            try:
                exit_code, exec_time_ms = self._stream(
                    command, timeout, working_dir, env,
                    lambda b: (out_parts.append(b), on_stdout(b) if on_stdout else None),
                    lambda b: (err_parts.append(b), on_stderr(b) if on_stderr else None),
                    client=stream_http,
                    on_response=capture_response,
                )
                state["result"] = ExecResult(
                    exit_code=exit_code,
                    stdout=b"".join(out_parts).decode("utf-8", "replace"),
                    stderr=b"".join(err_parts).decode("utf-8", "replace"),
                    exec_time_ms=exec_time_ms,
                )
            except BaseException as exc:  # noqa: BLE001
                state["error"] = exc
            finally:
                stream_http.close()
                done.set()

        # _base_url is read inside _stream; the thread relies on it being
        # resolved above so no k8s call happens off the calling thread.
        assert base_url
        thread = threading.Thread(target=drain_thread, daemon=True)
        thread.start()

        def wait_for() -> ExecResult:
            done.wait()
            if state["error"] is not None:
                raise state["error"]
            return state["result"]

        def kill() -> None:
            # Close the exact in-flight streaming response first so the read the
            # drain thread is blocked on aborts deterministically; relying on
            # Client.close() alone is not portable across httpx versions. Then
            # close the per-stream client (never the shared Sandbox client) and
            # join the drain thread so kill() returns only once the thread has
            # observed the teardown and set _done.
            with resp_lock:
                resp = state["response"]
            if resp is not None:
                try:
                    resp.close()
                except Exception:  # noqa: BLE001
                    pass
            stream_http.close()
            thread.join(timeout=5.0)

        return BackgroundProcess(
            _drain=wait_for,
            _close=kill,
            _done=done,
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

        raise AgentRunError(
            f"forks not ready after {timeout}s",
            code="ready_timeout",
            cause=f"fork {fork_name} did not produce {expected} Ready children within {timeout}s",
            remediation="Raise the timeout, or check the source is Ready and the pool/node has capacity.",
        )

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
