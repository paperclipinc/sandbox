from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Optional


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
