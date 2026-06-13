from __future__ import annotations

from typing import Optional

import httpx

from mitos.errors import AgentRunError


def _redact(text: str, token: Optional[str]) -> str:
    """Replaces every occurrence of a non-empty token with [REDACTED]. Mirrors
    the TypeScript redact helper and internal/mcp redaction."""
    if not token:
        return text
    return text.replace(token, "[REDACTED]")


# Default code and remediation per HTTP status, used when the body is not the
# structured server envelope (an older server, a proxy 502, a transport layer).
_STATUS_CODE = {
    400: "bad_request",
    401: "unauthorized",
    403: "forbidden",
    404: "not_found",
    409: "conflict",
    413: "request_too_large",
    429: "rate_limited",
    500: "internal_error",
    503: "unavailable",
}

_STATUS_REMEDIATION = {
    401: "Check the sandbox bearer token is set and authorizes this sandbox.",
    403: "Check the sandbox bearer token is set and authorizes this sandbox.",
    404: "Confirm the sandbox id exists and is Ready before calling.",
    413: "Reduce the request payload size (file content is hex-encoded and bounded by the server).",
    429: "Back off and retry the request after a short delay.",
}


def _status_code(status: int) -> str:
    if status in _STATUS_CODE:
        return _STATUS_CODE[status]
    return "server_error" if status >= 500 else "request_failed"


def _status_remediation(status: int) -> str:
    if status in _STATUS_REMEDIATION:
        return _STATUS_REMEDIATION[status]
    if status >= 500:
        return "Retry the request; if it persists, inspect the forkd or sandbox-server logs."
    return "Inspect the request fields against the sandbox API contract."


def error_from_response(resp: httpx.Response, token: Optional[str] = None) -> AgentRunError:
    """Builds an AgentRunError from a non-2xx response. Prefers the structured
    server envelope {error:{code,message,cause,remediation}}; falls back to
    status-derived defaults for an older or non-mitos server. Any bearer token
    echoed in the body is redacted before it becomes the cause."""
    status = resp.status_code
    body_text = _redact(resp.text, token)

    code = _status_code(status)
    message = f"sandbox API request failed: HTTP {status} ({code})"
    cause = body_text.strip() or f"HTTP {status}"
    remediation = _status_remediation(status)
    context: dict = {}

    try:
        parsed = resp.json()
    except Exception:  # noqa: BLE001  not JSON; keep the text fallback
        parsed = None

    if isinstance(parsed, dict):
        err = parsed.get("error")
        if isinstance(err, dict):
            # New structured envelope.
            code = err.get("code") or code
            message = err.get("message") or message
            cause = _redact(err.get("cause", ""), token) or cause
            remediation = err.get("remediation") or remediation
            ctx = err.get("context")
            if isinstance(ctx, dict):
                context = ctx
        elif isinstance(err, str):
            # Legacy bare {"error": "msg"} shape.
            cause = _redact(err, token) or cause

    return AgentRunError(
        message=message,
        code=code,
        cause=cause,
        remediation=remediation,
        status=status,
        context=context,
    )


def raise_for_status(resp: httpx.Response, token: Optional[str] = None) -> None:
    """Raises AgentRunError on a non-2xx response, leaving 2xx untouched. Drop-in
    for httpx Response.raise_for_status() but yields the structured type."""
    if resp.is_success:
        return
    raise error_from_response(resp, token=token)


def raise_for_status_stream(resp: httpx.Response, token: Optional[str] = None) -> None:
    """Like raise_for_status, for a streaming Response whose body has not been
    read. On a non-2xx status it reads the (small) error body so the structured
    envelope can be parsed; on success it leaves the stream unread so the caller
    iterates it normally."""
    if resp.is_success:
        return
    resp.read()
    raise error_from_response(resp, token=token)
