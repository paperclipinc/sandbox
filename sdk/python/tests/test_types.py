from mitos.types import ExecResult, FileInfo, ForkPolicy, SandboxPhase, PoolStatus


def test_fork_policy_values():
    assert ForkPolicy.FRESH == "Fresh"
    assert ForkPolicy.SHARE == "Share"
    assert ForkPolicy.CLONE == "Clone"
    assert ForkPolicy.SNAPSHOT == "Snapshot"


def test_sandbox_phase_values():
    assert SandboxPhase.PENDING == "Pending"
    assert SandboxPhase.RESTORING == "Restoring"
    assert SandboxPhase.READY == "Ready"
    assert SandboxPhase.TERMINATING == "Terminating"
    assert SandboxPhase.FAILED == "Failed"


def test_exec_result():
    result = ExecResult(exit_code=0, stdout="hello\n", stderr="")
    assert result.exit_code == 0
    assert result.stdout == "hello\n"
    assert result.stderr == ""


def test_exec_result_with_timing():
    result = ExecResult(exit_code=0, stdout="", stderr="", exec_time_ms=1.5)
    assert result.exec_time_ms == 1.5


def test_file_info():
    f = FileInfo(name="test.py", is_dir=False, size=1024)
    assert f.name == "test.py"
    assert not f.is_dir
    assert f.size == 1024


def test_file_info_directory():
    d = FileInfo(name="src", is_dir=True, size=0)
    assert d.is_dir


def test_pool_status():
    status = PoolStatus(
        name="python-pool",
        ready_snapshots=8,
        desired=10,
        node_distribution={"node-1": 4, "node-2": 4},
    )
    assert status.ready_snapshots == 8
    assert status.desired == 10
    assert len(status.node_distribution) == 2
