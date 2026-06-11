// Errors for the agent-run TypeScript SDK. AgentRunError carries an
// LLM-legible code, cause, and remediation so a failure can be acted on
// programmatically. fromResponse builds one from a non-2xx HTTP response and
// redacts any echo of the bearer token from the body first, so a token a
// hostile or misconfigured server reflects into its error body never surfaces.

export interface AgentRunErrorOptions {
  code: string;
  cause?: string;
  remediation?: string;
}

/**
 * An error from the SDK. `code` is a stable machine-readable identifier;
 * `cause` is the underlying detail (server body, redacted); `remediation` is a
 * short actionable hint. The token never appears in any of these fields.
 */
export class AgentRunError extends Error {
  readonly code: string;
  readonly errorCause?: string;
  readonly remediation?: string;

  constructor(message: string, opts: AgentRunErrorOptions) {
    super(message);
    this.name = "AgentRunError";
    this.code = opts.code;
    this.errorCause = opts.cause;
    this.remediation = opts.remediation;
  }

  /**
   * Builds an AgentRunError from a non-2xx HTTP response. The body is redacted
   * for the token before it becomes the error `cause`, so a reflected token is
   * never surfaced in a message, log, or thrown value.
   */
  static fromResponse(
    status: number,
    bodyText: string,
    token?: string,
  ): AgentRunError {
    const safeBody = redact(bodyText, token).trim();
    const code = codeForStatus(status);
    const cause = safeBody === "" ? `HTTP ${status}` : safeBody;
    const remediation = remediationForStatus(status);
    return new AgentRunError(
      `sandbox API request failed: HTTP ${status} (${code})`,
      { code, cause, remediation },
    );
  }
}

/**
 * Replaces every occurrence of a non-empty token in `text` with "[REDACTED]".
 * An empty or undefined token is a no-op. Mirrors internal/mcp redact.
 */
export function redact(text: string, token?: string): string {
  if (!token) {
    return text;
  }
  return text.split(token).join("[REDACTED]");
}

function codeForStatus(status: number): string {
  switch (status) {
    case 400:
      return "bad_request";
    case 401:
      return "unauthorized";
    case 403:
      return "forbidden";
    case 404:
      return "not_found";
    case 409:
      return "conflict";
    case 413:
      return "request_too_large";
    case 429:
      return "rate_limited";
    case 500:
      return "internal_error";
    case 503:
      return "unavailable";
    default:
      if (status >= 500) {
        return "server_error";
      }
      return "request_failed";
  }
}

function remediationForStatus(status: number): string {
  switch (status) {
    case 401:
    case 403:
      return "Check the sandbox bearer token is set and authorizes this sandbox.";
    case 404:
      return "Confirm the sandbox id exists and is Ready before calling.";
    case 413:
      return "Reduce the request payload size (file content is hex-encoded and bounded by the server).";
    case 429:
      return "Back off and retry the request after a short delay.";
    default:
      if (status >= 500) {
        return "Retry the request; if it persists, inspect the forkd or sandbox-server logs.";
      }
      return "Inspect the request fields against the sandbox API contract.";
  }
}
