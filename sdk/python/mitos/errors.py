from __future__ import annotations

from typing import Any, Optional


class AgentRunError(Exception):
    """An LLM-legible error from the mitos SDK.

    Mirrors the server envelope {error:{code, message, cause, remediation}} and
    the TypeScript AgentRunError. ``code`` is a stable machine identifier;
    ``cause`` is the underlying detail (with any bearer token redacted);
    ``remediation`` is a short actionable hint. ``status`` is the HTTP status
    when the error came from a response. No token or secret value appears in any
    field.
    """

    def __init__(
        self,
        message: str,
        code: str,
        cause: str = "",
        remediation: str = "",
        status: Optional[int] = None,
        context: Optional[dict[str, Any]] = None,
    ):
        super().__init__(message)
        self.code = code
        self.cause = cause
        self.remediation = remediation
        self.status = status
        self.context = context or {}

    def __str__(self) -> str:
        parts = [f"[{self.code}] {super().__str__()}"]
        if self.cause:
            parts.append(f"cause: {self.cause}")
        if self.remediation:
            parts.append(f"remediation: {self.remediation}")
        return " | ".join(parts)
