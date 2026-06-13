"""Integration test: Python SDK ↔ sandbox-server (mock mode).

Requires sandbox-server running on localhost:8080 with --mock.
Skip if server is not running.
"""

import os
import subprocess
import time

import pytest

from mitos.direct import SandboxServer


SERVER_URL = "http://localhost:18080"
server_process = None


@pytest.fixture(scope="module", autouse=True)
def start_server():
    """Start sandbox-server in mock mode for the test suite."""
    global server_process

    # Build the server
    result = subprocess.run(
        ["go", "build", "-o", "/tmp/sandbox-server-test", "./cmd/sandbox-server/"],
        cwd=os.path.join(os.path.dirname(__file__), "..", "..", ".."),
        capture_output=True,
    )
    if result.returncode != 0:
        pytest.skip(f"Could not build sandbox-server: {result.stderr.decode()}")

    # Start it
    server_process = subprocess.Popen(
        ["/tmp/sandbox-server-test", "--mock", "--addr", ":18080"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    time.sleep(1)

    yield

    server_process.terminate()
    server_process.wait(timeout=5)


def test_health():
    server = SandboxServer(SERVER_URL)
    health = server.health()
    assert health["status"] == "ok"
    assert health["mock"] is True


def test_create_template():
    server = SandboxServer(SERVER_URL)
    result = server.create_template("test-python", init_wait_seconds=1)
    assert result["id"] == "test-python"
    assert result["ready"] is True


def test_list_templates():
    server = SandboxServer(SERVER_URL)
    templates = server.list_templates()
    assert len(templates) >= 1
    names = [t["id"] for t in templates]
    assert "test-python" in names


def test_fork():
    server = SandboxServer(SERVER_URL)
    sandbox = server.fork("test-python", "test-sandbox-1")
    assert sandbox.id == "test-sandbox-1"
    assert sandbox.template == "test-python"
    assert sandbox.fork_time_ms > 0


def test_fork_auto_id():
    server = SandboxServer(SERVER_URL)
    sandbox = server.fork("test-python")
    assert sandbox.id.startswith("sandbox-")
    assert len(sandbox.id) > 10


def test_fork_unknown_template():
    server = SandboxServer(SERVER_URL)
    import httpx
    with pytest.raises(httpx.HTTPStatusError):
        server.fork("nonexistent")


def test_list_sandboxes():
    server = SandboxServer(SERVER_URL)
    sandboxes = server.list_sandboxes()
    assert len(sandboxes) >= 1


def test_terminate():
    server = SandboxServer(SERVER_URL)
    sandbox = server.fork("test-python", "to-terminate")
    sandbox.terminate()

    # Should be gone
    sandboxes = server.list_sandboxes()
    ids = [s["id"] for s in sandboxes]
    assert "to-terminate" not in ids


def test_context_manager():
    server = SandboxServer(SERVER_URL)
    with server.fork("test-python", "ctx-sandbox") as sandbox:
        assert sandbox.id == "ctx-sandbox"

    sandboxes = server.list_sandboxes()
    ids = [s["id"] for s in sandboxes]
    assert "ctx-sandbox" not in ids


def test_repr():
    server = SandboxServer(SERVER_URL)
    sandbox = server.fork("test-python", "repr-test")
    r = repr(sandbox)
    assert "repr-test" in r
    sandbox.terminate()
