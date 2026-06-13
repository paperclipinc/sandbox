from __future__ import annotations

import threading
from dataclasses import dataclass, field
from enum import Enum
from typing import Callable, Optional


class ForkPolicy(str, Enum):
    FRESH = "Fresh"
    SHARE = "Share"
    CLONE = "Clone"
    SNAPSHOT = "Snapshot"


class SandboxPhase(str, Enum):
    PENDING = "Pending"
    RESTORING = "Restoring"
    READY = "Ready"
    TERMINATING = "Terminating"
    FAILED = "Failed"


@dataclass
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str
    exec_time_ms: float = 0.0


@dataclass
class FileInfo:
    name: str
    is_dir: bool
    size: int
    mode: int = 0
    modified_at: Optional[str] = None


@dataclass
class SandboxInfo:
    name: str
    phase: SandboxPhase
    endpoint: str
    node: str
    sandbox_id: str
    fork_time_ms: float
    pool: str


@dataclass
class PoolStatus:
    name: str
    ready_snapshots: int
    desired: int
    node_distribution: dict[str, int] = field(default_factory=dict)


@dataclass
class ForkInfo:
    name: str
    sandbox_id: str
    endpoint: str
    node: str
    phase: SandboxPhase
    fork_time_ms: float = 0.0


@dataclass
class BackgroundProcess:
    """A handle to a streaming exec started in the background.

    The command begins running on a background thread the moment the handle is
    created. wait() blocks for that thread and returns the aggregate
    ExecResult. kill() stops the process by closing only this stream's own HTTP
    client, which forkd turns into a context cancel that kills the guest
    process group; it never touches the shared Sandbox client. running() reports
    whether the drain thread is still going.
    """

    _drain: Callable[[], ExecResult]
    _close: Callable[[], None]
    _done: Optional[threading.Event] = None
    _result: Optional[ExecResult] = None

    def running(self) -> bool:
        """Whether the background command is still draining."""
        if self._done is None:
            return False
        return not self._done.is_set()

    def wait(self) -> ExecResult:
        if self._result is None:
            self._result = self._drain()
        return self._result

    def kill(self) -> None:
        self._close()
