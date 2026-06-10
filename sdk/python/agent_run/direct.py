"""Direct client for sandbox-server (no Kubernetes required).

Tokenless by design: the standalone sandbox-server has no token-minting
control plane and runs its sandbox API with AllowTokenless. The k8s-mode
client (sandbox.py) sends per-sandbox bearer tokens instead.

Usage:
    from agent_run.direct import SandboxServer

    server = SandboxServer("http://localhost:8080")
    server.create_template("python")

    sandbox = server.fork("python")
    result = sandbox.exec("print(1 + 1)")
    print(result.stdout)

    sandbox.terminate()
"""
from __future__ import annotations

import uuid
from typing import Optional

import httpx

from agent_run.types import ExecResult


class DirectSandbox:
    """A sandbox connected directly to sandbox-server."""

    def __init__(self, id: str, template: str, endpoint: str, server_url: str, fork_time_ms: float):
        self.id = id
        self.template = template
        self.endpoint = endpoint
        self.fork_time_ms = fork_time_ms
        self._server_url = server_url
        self._http = httpx.Client(timeout=30.0)

    def exec(self, command: str, timeout: int = 30) -> ExecResult:
        resp = self._http.post(
            f"{self._server_url}/v1/exec",
            json={"sandbox": self.id, "command": command, "timeout": timeout},
        )
        resp.raise_for_status()
        data = resp.json()
        return ExecResult(
            exit_code=data["exit_code"],
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
            exec_time_ms=data.get("exec_time_ms", 0),
        )

    def terminate(self) -> None:
        self._http.delete(f"{self._server_url}/v1/sandboxes/{self.id}")
        self._http.close()

    def __enter__(self) -> DirectSandbox:
        return self

    def __exit__(self, *args) -> None:
        self.terminate()

    def __repr__(self) -> str:
        return f"DirectSandbox(id={self.id!r}, fork_time_ms={self.fork_time_ms:.2f})"


class SandboxServer:
    """Client for sandbox-server REST API (standalone mode, no k8s)."""

    def __init__(self, url: str = "http://localhost:8080"):
        self.url = url.rstrip("/")
        self._http = httpx.Client(timeout=60.0)

    def health(self) -> dict:
        resp = self._http.get(f"{self.url}/v1/health")
        resp.raise_for_status()
        return resp.json()

    def list_templates(self) -> list[dict]:
        resp = self._http.get(f"{self.url}/v1/templates")
        resp.raise_for_status()
        return resp.json()

    def create_template(self, id: str, init_wait_seconds: int = 5) -> dict:
        resp = self._http.post(
            f"{self.url}/v1/templates",
            json={"id": id, "init_wait_seconds": init_wait_seconds},
        )
        resp.raise_for_status()
        return resp.json()

    def fork(self, template: str, id: Optional[str] = None) -> DirectSandbox:
        if id is None:
            id = f"sandbox-{uuid.uuid4().hex[:8]}"
        resp = self._http.post(
            f"{self.url}/v1/fork",
            json={"template": template, "id": id},
        )
        resp.raise_for_status()
        data = resp.json()
        return DirectSandbox(
            id=data["id"],
            template=data["template_id"],
            endpoint=data["endpoint"],
            server_url=self.url,
            fork_time_ms=data["fork_time_ms"],
        )

    def list_sandboxes(self) -> list[dict]:
        resp = self._http.get(f"{self.url}/v1/sandboxes")
        resp.raise_for_status()
        return resp.json()
