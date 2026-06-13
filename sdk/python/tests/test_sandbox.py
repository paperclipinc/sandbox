import base64
import json
from unittest.mock import MagicMock, patch

import httpx
import pytest

from mitos.sandbox import Sandbox, SandboxFiles
from mitos.types import SandboxPhase


TEST_TOKEN = "ab" * 32  # 64 hex chars, like the controller mints


def _token_secret_mock(token: str = TEST_TOKEN, endpoint: str = "10.0.0.5:9091") -> MagicMock:
    """Mock of CoreV1Api.read_namespaced_secret's V1Secret (base64 data)."""
    secret = MagicMock()
    secret.data = {
        "token": base64.b64encode(token.encode()).decode(),
        "endpoint": base64.b64encode(endpoint.encode()).decode(),
    }
    return secret


@pytest.fixture
def mock_api():
    return MagicMock()


@pytest.fixture
def mock_core_api():
    core_api = MagicMock()
    core_api.read_namespaced_secret.return_value = _token_secret_mock()
    return core_api


@pytest.fixture
def ready_sandbox(mock_api, mock_core_api):
    return Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=mock_core_api,
        _endpoint="127.0.0.1:8080",
        _phase=SandboxPhase.READY,
    )


@pytest.fixture
def pending_sandbox(mock_api, mock_core_api):
    return Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=mock_core_api,
    )


def test_sandbox_repr(ready_sandbox):
    r = repr(ready_sandbox)
    assert "test-sandbox" in r
    assert "Ready" in r


def test_sandbox_endpoint(ready_sandbox):
    assert ready_sandbox.endpoint == "127.0.0.1:8080"


def test_sandbox_phase(ready_sandbox):
    assert ready_sandbox.phase == SandboxPhase.READY


def test_pending_sandbox_phase(pending_sandbox):
    assert pending_sandbox.phase == SandboxPhase.PENDING


def test_sandbox_context_manager(mock_api, mock_core_api):
    sandbox = Sandbox(
        name="ctx-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=mock_core_api,
        _endpoint="127.0.0.1:8080",
        _phase=SandboxPhase.READY,
    )

    with sandbox as s:
        assert s.name == "ctx-sandbox"

    mock_api.delete_namespaced_custom_object.assert_called_once()


def test_sandbox_terminate(ready_sandbox, mock_api):
    ready_sandbox.terminate()

    mock_api.delete_namespaced_custom_object.assert_called_once_with(
        group="mitos.run",
        version="v1alpha1",
        namespace="default",
        plural="sandboxclaims",
        name="test-sandbox",
    )
    assert ready_sandbox.phase == SandboxPhase.TERMINATING


def test_sandbox_fork_creates_cr(ready_sandbox, mock_api):
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {
            "readyForks": 2,
            "forks": [
                {"name": "fork-1", "endpoint": "127.0.0.1:9001", "phase": "Ready", "sandboxID": "f1", "node": "n1"},
                {"name": "fork-2", "endpoint": "127.0.0.1:9002", "phase": "Ready", "sandboxID": "f2", "node": "n1"},
            ],
        }
    }

    forks = ready_sandbox.fork(2)

    mock_api.create_namespaced_custom_object.assert_called_once()
    call_kwargs = mock_api.create_namespaced_custom_object.call_args
    body = call_kwargs.kwargs.get("body") or call_kwargs[1].get("body")
    assert body["kind"] == "SandboxFork"
    assert body["spec"]["replicas"] == 2
    assert body["spec"]["sourceRef"]["name"] == "test-sandbox"

    assert len(forks) == 2
    assert forks[0].phase == SandboxPhase.READY
    assert forks[1].phase == SandboxPhase.READY
    assert forks[0]._sandbox_id == "f1"
    assert forks[1]._sandbox_id == "f2"


def test_sandbox_wait_ready_polls(pending_sandbox, mock_api):
    mock_api.get_namespaced_custom_object.side_effect = [
        {"status": {"phase": "Pending"}},
        {"status": {"phase": "Restoring"}},
        {"status": {"phase": "Ready", "endpoint": "10.0.0.5:8080"}},
    ]

    pending_sandbox._wait_ready(timeout=5.0)

    assert pending_sandbox.phase == SandboxPhase.READY
    assert pending_sandbox._endpoint == "10.0.0.5:8080"
    assert mock_api.get_namespaced_custom_object.call_count == 3


def test_sandbox_wait_ready_failed(pending_sandbox, mock_api):
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {"phase": "Failed"}
    }

    with pytest.raises(RuntimeError, match="failed"):
        pending_sandbox._wait_ready(timeout=1.0)


def test_wait_ready_reads_token_secret(mock_api):
    core_api = MagicMock()
    core_api.read_namespaced_secret.return_value = _token_secret_mock()
    sandbox = Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=core_api,
    )
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {"phase": "Ready", "endpoint": "10.0.0.5:9091", "sandboxID": "sb-1"}
    }

    sandbox._wait_ready(timeout=5.0)

    core_api.read_namespaced_secret.assert_called_once_with(
        name="test-sandbox-sandbox-token", namespace="default"
    )
    assert sandbox._token == TEST_TOKEN


def test_wait_ready_tolerates_missing_token_secret(mock_api):
    from kubernetes.client.rest import ApiException

    core_api = MagicMock()
    core_api.read_namespaced_secret.side_effect = ApiException(status=404)
    sandbox = Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=core_api,
    )
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {"phase": "Ready", "endpoint": "10.0.0.5:9091"}
    }

    sandbox._wait_ready(timeout=5.0)

    assert sandbox._token is None


def _ready_http_sandbox(transport: httpx.MockTransport, token: str | None = TEST_TOKEN) -> Sandbox:
    sandbox = Sandbox(
        name="claim-1",
        namespace="default",
        pool="pool-1",
        api=MagicMock(),  # k8s API unused when endpoint/phase/sandbox_id pre-seeded
        core_api=MagicMock(),
        _endpoint="10.0.3.7:9091",
        _phase=SandboxPhase.READY,
    )
    sandbox._sandbox_id = "sb-claim-1"
    sandbox._token = token
    sandbox._http = httpx.Client(transport=transport)
    return sandbox


def test_exec_targets_v1_and_sends_sandbox_id():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["json"] = json.loads(request.content)
        return httpx.Response(
            200,
            json={"exit_code": 0, "stdout": "hi\n", "stderr": "", "exec_time_ms": 1.0},
        )

    result = _ready_http_sandbox(httpx.MockTransport(handler)).exec("echo hi")

    assert result.stdout == "hi\n"
    assert seen["url"] == "http://10.0.3.7:9091/v1/exec"
    assert seen["json"]["sandbox"] == "sb-claim-1"


def test_files_read_targets_v1_and_sends_sandbox_id():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["json"] = json.loads(request.content)
        return httpx.Response(200, json={"content": "data", "size": 4})

    content = _ready_http_sandbox(httpx.MockTransport(handler)).files.read("/workspace/x")
    assert content == "data"
    assert seen["url"].endswith("/v1/files/read")
    assert seen["json"]["sandbox"] == "sb-claim-1"


def test_exec_sends_bearer_token():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers.get("authorization")
        return httpx.Response(
            200,
            json={"exit_code": 0, "stdout": "", "stderr": "", "exec_time_ms": 1.0},
        )

    _ready_http_sandbox(httpx.MockTransport(handler)).exec("true")

    assert seen["auth"] == f"Bearer {TEST_TOKEN}"


def test_all_file_calls_send_bearer_token():
    auths = []

    def handler(request: httpx.Request) -> httpx.Response:
        auths.append(request.headers.get("authorization"))
        return httpx.Response(
            200,
            json={"content": "", "size": 0, "entries": [], "status": "ok"},
        )

    files = _ready_http_sandbox(httpx.MockTransport(handler)).files
    files.read("/x")
    files.write("/x", "data")
    files.list("/")
    files.mkdir("/d")
    files.remove("/x")

    assert auths == [f"Bearer {TEST_TOKEN}"] * 5


def test_no_token_sends_no_auth_header():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers.get("authorization")
        return httpx.Response(
            200,
            json={"exit_code": 0, "stdout": "", "stderr": "", "exec_time_ms": 1.0},
        )

    _ready_http_sandbox(httpx.MockTransport(handler), token=None).exec("true")

    assert seen["auth"] is None


def test_wait_forks_loads_each_fork_token(mock_api):
    core_api = MagicMock()
    core_api.read_namespaced_secret.return_value = _token_secret_mock()
    sandbox = Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=core_api,
        _endpoint="127.0.0.1:8080",
        _phase=SandboxPhase.READY,
    )
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {
            "readyForks": 1,
            "forks": [
                {"name": "fork-1", "endpoint": "127.0.0.1:9001", "phase": "Ready", "sandboxID": "f1", "node": "n1"},
            ],
        }
    }

    forks = sandbox.fork(1)

    assert forks[0]._token == TEST_TOKEN
    core_api.read_namespaced_secret.assert_called_once_with(
        name="fork-1-sandbox-token", namespace="default"
    )
