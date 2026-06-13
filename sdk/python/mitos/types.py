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
class Result:
    """One rich display artifact from run_code (mirrors E2B's Result).

    data maps a MIME type to its payload (base64 for image/png, raw text for
    text/html, image/svg+xml, text/markdown, text/latex, application/json,
    text/plain). The typed properties are convenience accessors that return None
    when that MIME type is absent. is_main_result is True for the cell's
    last-expression value (an execute_result), False for a side display_data.
    """

    data: dict[str, str] = field(default_factory=dict)
    is_main_result: bool = False

    @property
    def text(self) -> Optional[str]:
        return self.data.get("text/plain")

    @property
    def png(self) -> Optional[str]:
        return self.data.get("image/png")

    @property
    def svg(self) -> Optional[str]:
        return self.data.get("image/svg+xml")

    @property
    def html(self) -> Optional[str]:
        return self.data.get("text/html")

    @property
    def markdown(self) -> Optional[str]:
        return self.data.get("text/markdown")

    @property
    def latex(self) -> Optional[str]:
        return self.data.get("text/latex")

    @property
    def json(self) -> Optional[str]:
        return self.data.get("application/json")

    @property
    def chart(self) -> Optional[str]:
        # Structured chart data, when a kernel emits it under this MIME type.
        return self.data.get("application/vnd.vegalite.v5+json") or self.data.get(
            "application/vnd.vega.v5+json"
        )


@dataclass
class ExecutionError:
    """A structured exception from run_code (mirrors E2B's error)."""

    name: str
    value: str
    traceback: list[str] = field(default_factory=list)


@dataclass
class Execution:
    """The full result of a run_code call (mirrors E2B's Execution).

    text is the REPL last-expression value (the text/plain of the main result);
    logs holds buffered stdout/stderr lines; results is every rich display
    artifact in order; error is the structured exception, or None.
    """

    text: Optional[str] = None
    logs: dict[str, list[str]] = field(default_factory=lambda: {"stdout": [], "stderr": []})
    results: list[Result] = field(default_factory=list)
    error: Optional[ExecutionError] = None


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
