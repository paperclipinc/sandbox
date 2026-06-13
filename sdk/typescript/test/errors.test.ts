import { describe, expect, it } from "vitest";
import { AgentRunError } from "../src/errors.js";

describe("AgentRunError.fromResponse server envelope", () => {
  it("prefers the server envelope code/cause/remediation over status defaults", () => {
    // Use a server code/remediation that DIFFERS from the status-derived
    // defaults for 500, so the test fails unless the envelope is parsed.
    const body = JSON.stringify({
      error: {
        code: "exec_failed",
        message: "the command could not be executed in the sandbox",
        cause: "exec failed: agent not connected",
        remediation: "Inspect the cause and check the forkd logs for the guest agent state.",
      },
    });
    const err = AgentRunError.fromResponse(500, body);
    expect(err.code).toBe("exec_failed");
    // The cause is the inner field only, not the whole JSON envelope.
    expect(err.errorCause).toBe("exec failed: agent not connected");
    expect(err.remediation).toContain("guest agent state");
  });

  it("falls back to status-derived defaults for a non-envelope body", () => {
    const err = AgentRunError.fromResponse(503, "upstream gateway error");
    expect(err.code).toBe("unavailable");
    expect(err.errorCause).toContain("upstream gateway error");
    expect(err.remediation).not.toBe("");
  });

  it("handles the legacy bare {error: string} body", () => {
    const err = AgentRunError.fromResponse(500, JSON.stringify({ error: "boom" }));
    // The legacy string becomes the cause verbatim, not the wrapping JSON.
    expect(err.errorCause).toBe("boom");
    expect(err.remediation).not.toBe("");
  });

  it("redacts a token echoed in an envelope cause", () => {
    const token = "supersecrettoken";
    const body = JSON.stringify({
      error: { code: "internal", message: "x", cause: `leaked ${token}`, remediation: "y" },
    });
    const err = AgentRunError.fromResponse(500, body, token);
    expect(err.errorCause).toBe("leaked [REDACTED]");
    expect(err.errorCause).not.toContain(token);
  });
});
