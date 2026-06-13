from unittest import mock

from mitos.client import AgentRun
from mitos.types import SandboxPhase


def _client():
    c = AgentRun.__new__(AgentRun)
    c._api = mock.MagicMock()
    c._core_api = mock.MagicMock()
    c._namespace = "default"
    c._allow_default_pool = True
    return c


def test_from_name_reconnects_ready_sandbox():
    c = _client()
    # The reconnected handle reads its token Secret; return an empty data map so
    # _load_token stays tokenless rather than base64-decoding a MagicMock.
    c._core_api.read_namespaced_secret.return_value = mock.MagicMock(data={})
    c._api.get_namespaced_custom_object.return_value = {
        "spec": {"poolRef": {"name": "p"}},
        "status": {"phase": "Ready", "endpoint": "10.0.0.2:8443", "sandboxID": "sb-x"},
    }
    sb = c.from_name("agent-session-1")
    assert sb.name == "agent-session-1"
    assert sb.phase == SandboxPhase.READY
    assert sb.endpoint == "10.0.0.2:8443"
    assert sb.pool == "p"
    # The token Secret was read for the reconnected handle.
    c._core_api.read_namespaced_secret.assert_called()
