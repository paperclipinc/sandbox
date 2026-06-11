import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { AddressInfo } from "node:net";

import { HttpClient, validSandboxId } from "../src/http.js";
import { AgentRunError, redact } from "../src/errors.js";

type Handler = (req: IncomingMessage, body: string, res: ServerResponse) => void;

let server: Server;
let baseUrl: string;
let handler: Handler;

beforeEach(async () => {
  server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => handler(req, body, res));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address() as AddressInfo;
  baseUrl = `http://127.0.0.1:${addr.port}`;
});

afterEach(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

describe("validSandboxId", () => {
  it("accepts allowlisted ids", () => {
    expect(validSandboxId("sbx-123")).toBe(true);
    expect(validSandboxId("A_b-9")).toBe(true);
  });

  it("rejects empty, traversal, and slash ids", () => {
    expect(validSandboxId("")).toBe(false);
    expect(validSandboxId("..")).toBe(false);
    expect(validSandboxId("a/b")).toBe(false);
    expect(validSandboxId("-leading")).toBe(false);
    expect(validSandboxId("x".repeat(65))).toBe(false);
  });
});

describe("redact", () => {
  it("replaces a non-empty token", () => {
    expect(redact("secret=tok-abc here", "tok-abc")).toBe("secret=[REDACTED] here");
  });

  it("is a no-op for an empty token", () => {
    expect(redact("nothing to hide", "")).toBe("nothing to hide");
    expect(redact("nothing to hide", undefined)).toBe("nothing to hide");
  });
});

describe("HttpClient", () => {
  it("sends the bearer header only when a token is set and posts JSON", async () => {
    let seenAuth: string | undefined;
    let seenContentType: string | undefined;
    let seenBody = "";
    handler = (req, body, res) => {
      seenAuth = req.headers["authorization"];
      seenContentType = req.headers["content-type"];
      seenBody = body;
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ ok: true }));
    };

    const client = new HttpClient(baseUrl, "tok-xyz");
    const out = await client.post<{ ok: boolean }>("/v1/thing", { a: 1 });
    expect(out.ok).toBe(true);
    expect(seenAuth).toBe("Bearer tok-xyz");
    expect(seenContentType).toBe("application/json");
    expect(JSON.parse(seenBody)).toEqual({ a: 1 });
  });

  it("omits the bearer header when no token is set", async () => {
    let seenAuth: string | undefined = "unset";
    handler = (req, _body, res) => {
      seenAuth = req.headers["authorization"];
      res.end(JSON.stringify({}));
    };
    const client = new HttpClient(baseUrl);
    await client.post("/v1/thing", {});
    expect(seenAuth).toBeUndefined();
  });

  it("throws an AgentRunError on non-2xx with the token redacted from the body", async () => {
    const token = "super-secret-token";
    handler = (_req, _body, res) => {
      res.writeHead(500, { "content-type": "application/json" });
      // A misconfigured server echoes the token into its error body.
      res.end(JSON.stringify({ error: `boom with bearer ${token}` }));
    };
    const client = new HttpClient(baseUrl, token);

    await expect(client.post("/v1/thing", {})).rejects.toMatchObject({
      name: "AgentRunError",
    });

    let caught: AgentRunError | undefined;
    try {
      await client.post("/v1/thing", {});
    } catch (e) {
      caught = e as AgentRunError;
    }
    expect(caught).toBeInstanceOf(AgentRunError);
    const serialized = JSON.stringify({
      message: caught!.message,
      cause: caught!.errorCause,
      remediation: caught!.remediation,
      code: caught!.code,
    });
    expect(serialized).not.toContain(token);
    expect(serialized).toContain("[REDACTED]");
    expect(caught!.code).toBe("internal_error");
    expect(caught!.errorCause).toBeTruthy();
    expect(caught!.remediation).toBeTruthy();
  });

  it("del issues a DELETE and resolves on 2xx", async () => {
    let method: string | undefined;
    handler = (req, _body, res) => {
      method = req.method;
      res.end(JSON.stringify({ status: "terminated" }));
    };
    const client = new HttpClient(baseUrl);
    await client.del("/v1/sandboxes/sbx-1");
    expect(method).toBe("DELETE");
  });
});
