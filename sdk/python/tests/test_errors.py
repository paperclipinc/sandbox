import httpx
import pytest

from mitos.errors import AgentRunError
from mitos._envelope import raise_for_status


def _response(status, json_body=None, text=None):
    request = httpx.Request("POST", "http://sb/v1/exec")
    if json_body is not None:
        return httpx.Response(status, json=json_body, request=request)
    return httpx.Response(status, text=text or "", request=request)


def test_parses_server_envelope():
    resp = _response(404, {"error": {
        "code": "not_found",
        "message": "no such sandbox",
        "cause": "no sandbox registered for id sb-1",
        "remediation": "Confirm the sandbox id exists and is Ready before calling.",
    }})
    with pytest.raises(AgentRunError) as ei:
        raise_for_status(resp)
    err = ei.value
    assert err.code == "not_found"
    assert err.status == 404
    assert err.remediation.startswith("Confirm")
    assert "no sandbox registered" in err.cause
    # The string form is LLM-legible: code + remediation are visible.
    assert "not_found" in str(err)
    assert "Confirm" in str(err)


def test_falls_back_on_non_envelope_body():
    resp = _response(503, text="upstream gateway error")
    with pytest.raises(AgentRunError) as ei:
        raise_for_status(resp)
    err = ei.value
    assert err.status == 503
    assert err.code == "unavailable"
    assert err.remediation  # never empty
    assert "upstream gateway error" in err.cause


def test_legacy_bare_error_string_body():
    # A server still on the old {"error": "msg"} shape must not crash the client.
    resp = _response(500, {"error": "boom"})
    with pytest.raises(AgentRunError) as ei:
        raise_for_status(resp)
    assert ei.value.cause == "boom" or "boom" in ei.value.cause


def test_success_passes_through():
    raise_for_status(_response(200, {"ok": True}))  # no raise


def test_redacts_token_from_cause():
    token = "supersecrettoken"
    resp = _response(500, text=f"error with leaked {token} in body")
    with pytest.raises(AgentRunError) as ei:
        raise_for_status(resp, token=token)
    assert token not in ei.value.cause
    assert "[REDACTED]" in ei.value.cause
