import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { AddressInfo } from "node:net";

import { Sandbox } from "../src/sandbox.js";
import { AgentRunError } from "../src/errors.js";

interface Recorded {
  method?: string;
  url?: string;
  auth?: string;
  body: unknown;
}

let server: Server;
let baseUrl: string;
let recorded: Recorded[];
let responder: (req: IncomingMessage, body: string, res: ServerResponse) => void;

beforeEach(async () => {
  recorded = [];
  server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      recorded.push({
        method: req.method,
        url: req.url,
        auth: req.headers["authorization"],
        body: body ? JSON.parse(body) : undefined,
      });
      responder(req, body, res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address() as AddressInfo;
  baseUrl = `127.0.0.1:${addr.port}`;
});

afterEach(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

describe("Sandbox.exec", () => {
  it("sends {sandbox, command, timeout} with the bearer header and parses ExecResult", async () => {
    responder = (_req, _body, res) => {
      res.setHeader("content-type", "application/json");
      res.end(
        JSON.stringify({ exit_code: 7, stdout: "hi", stderr: "err", exec_time_ms: 12.5 }),
      );
    };
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl, token: "tok-1" });
    const result = await sandbox.exec("echo hi", { timeoutSeconds: 9 });

    expect(result).toEqual({
      exitCode: 7,
      stdout: "hi",
      stderr: "err",
      execTimeMs: 12.5,
    });
    const call = recorded[0];
    expect(call.url).toBe("/v1/exec");
    expect(call.auth).toBe("Bearer tok-1");
    expect(call.body).toEqual({ sandbox: "sbx-1", command: "echo hi", timeout: 9 });
  });

  it("omits timeout when not provided", async () => {
    responder = (_req, _body, res) => res.end(JSON.stringify({ exit_code: 0 }));
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    const result = await sandbox.exec("ls");
    expect(result.exitCode).toBe(0);
    expect(result.stdout).toBe("");
    expect(recorded[0].body).toEqual({ sandbox: "sbx-1", command: "ls" });
  });
});

describe("Sandbox.files", () => {
  it("read posts {sandbox, path} and returns content", async () => {
    responder = (_req, _body, res) =>
      res.end(JSON.stringify({ content: "file body", size: 9 }));
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    const out = await sandbox.files.read("/etc/hosts");
    expect(out).toBe("file body");
    expect(recorded[0].url).toBe("/v1/files/read");
    expect(recorded[0].body).toEqual({ sandbox: "sbx-1", path: "/etc/hosts" });
  });

  it("write posts content and an explicit mode", async () => {
    responder = (_req, _body, res) => res.end(JSON.stringify({ status: "ok" }));
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    await sandbox.files.write("/tmp/x", "data", { mode: 0o600 });
    expect(recorded[0].url).toBe("/v1/files/write");
    expect(recorded[0].body).toEqual({
      sandbox: "sbx-1",
      path: "/tmp/x",
      content: "data",
      mode: 0o600,
    });
  });

  it("write omits mode when not given", async () => {
    responder = (_req, _body, res) => res.end(JSON.stringify({ status: "ok" }));
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    await sandbox.files.write("/tmp/x", "data");
    expect(recorded[0].body).toEqual({ sandbox: "sbx-1", path: "/tmp/x", content: "data" });
  });

  it("list posts {sandbox, path} and maps entries to FileInfo", async () => {
    responder = (_req, _body, res) =>
      res.end(
        JSON.stringify({
          entries: [
            { name: "a", is_dir: false, size: 3, mode: 420, modified_at: "2026-06-11T00:00:00Z" },
            { name: "sub", is_dir: true, size: 0 },
          ],
        }),
      );
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    const entries = await sandbox.files.list("/workspace");
    expect(recorded[0].url).toBe("/v1/files/list");
    expect(recorded[0].body).toEqual({ sandbox: "sbx-1", path: "/workspace" });
    expect(entries).toEqual([
      { name: "a", isDir: false, size: 3, mode: 420, modifiedAt: "2026-06-11T00:00:00Z" },
      { name: "sub", isDir: true, size: 0, mode: 0, modifiedAt: undefined },
    ]);
  });

  it("list defaults the path to /", async () => {
    responder = (_req, _body, res) => res.end(JSON.stringify({ entries: [] }));
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl });
    await sandbox.files.list();
    expect(recorded[0].body).toEqual({ sandbox: "sbx-1", path: "/" });
  });
});

describe("Sandbox errors and validation", () => {
  it("rejects an unsafe sandbox id before any request", () => {
    expect(() => new Sandbox({ id: "../etc", endpoint: baseUrl })).toThrow(AgentRunError);
    expect(() => new Sandbox({ id: "a/b", endpoint: baseUrl })).toThrow(AgentRunError);
    // No request should have reached the server.
    expect(recorded.length).toBe(0);
  });

  it("surfaces a server error as an AgentRunError without the token", async () => {
    const token = "leaky-token-value";
    responder = (_req, _body, res) => {
      res.writeHead(500);
      res.end(JSON.stringify({ error: `failure ${token}` }));
    };
    const sandbox = new Sandbox({ id: "sbx-1", endpoint: baseUrl, token });
    let caught: AgentRunError | undefined;
    try {
      await sandbox.exec("boom");
    } catch (e) {
      caught = e as AgentRunError;
    }
    expect(caught).toBeInstanceOf(AgentRunError);
    expect(JSON.stringify(caught)).not.toContain(token);
    expect(caught!.code).toBe("internal_error");
  });
});

describe("Sandbox.terminate", () => {
  it("invokes the injected terminator", async () => {
    responder = (_req, _body, res) => res.end("{}");
    let called = false;
    const sandbox = new Sandbox({
      id: "sbx-1",
      endpoint: baseUrl,
      terminator: async () => {
        called = true;
      },
    });
    await sandbox.terminate();
    expect(called).toBe(true);
  });
});
