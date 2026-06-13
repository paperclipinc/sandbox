from unittest import mock

import pytest

from mitos.errors import AgentRunError
from mitos.sandbox import Sandbox
from mitos.types import SandboxPhase


def _sandbox(api):
    core_api = mock.MagicMock()
    # Stay tokenless on the reconnect: return an empty Secret data map so
    # _load_token does not try to base64-decode a MagicMock.
    core_api.read_namespaced_secret.return_value = mock.MagicMock(data={})
    return Sandbox(name="sb-1", namespace="default", pool="p", api=api,
                   core_api=core_api)


def test_wait_until_ready_returns_when_ready():
    api = mock.MagicMock()
    api.get_namespaced_custom_object.return_value = {
        "status": {"phase": "Ready", "endpoint": "10.0.0.1:8443", "sandboxID": "sb-1"}
    }
    sb = _sandbox(api)
    assert sb.wait_until_ready(timeout=1.0) is sb
    assert sb.phase == SandboxPhase.READY
    assert sb.endpoint == "10.0.0.1:8443"


def test_wait_until_ready_raises_on_failed():
    api = mock.MagicMock()
    api.get_namespaced_custom_object.return_value = {"status": {"phase": "Failed"}}
    sb = _sandbox(api)
    with pytest.raises(AgentRunError) as ei:
        sb.wait_until_ready(timeout=1.0)
    assert ei.value.code == "sandbox_failed"


def test_wait_until_ready_times_out():
    api = mock.MagicMock()
    api.get_namespaced_custom_object.return_value = {"status": {"phase": "Pending"}}
    sb = _sandbox(api)
    with pytest.raises(AgentRunError) as ei:
        sb.wait_until_ready(timeout=0.2)
    assert ei.value.code == "ready_timeout"
