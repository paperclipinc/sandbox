import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { AddressInfo } from "node:net";

import { SandboxServer } from "../src/server.js";

interface Recorded {
  method?: string;
  url?: string;
  body: unknown;
}

let server: Server;
let baseUrl: string;
let recorded: Recorded[];
let sandboxIds: Set<string>;

beforeEach(async () => {
  recorded = [];
  sandboxIds = new Set();
  server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      const parsed = body ? JSON.parse(body) : undefined;
      recorded.push({ method: req.method, url: req.url, body: parsed });
      route(req, parsed, res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address() as AddressInfo;
  baseUrl = `http://127.0.0.1:${addr.port}`;
});

afterEach(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

function json(res: ServerResponse, v: unknown, code = 200) {
  res.writeHead(code, { "content-type": "application/json" });
  res.end(JSON.stringify(v));
}

// route reproduces the sandbox-server shapes (cmd/sandbox-server/main.go).
function route(req: IncomingMessage, body: any, res: ServerResponse) {
  const url = req.url ?? "";
  if (req.method === "GET" && url === "/v1/templates") {
    return json(res, [
      { id: "python", ready: true, created_at: "2026-06-11T00:00:00Z", creation_time_ms: 120 },
    ]);
  }
  if (req.method === "POST" && url === "/v1/templates") {
    return json(res, {
      id: body.id,
      ready: true,
      created_at: "2026-06-11T00:00:00Z",
      creation_time_ms: 100,
    });
  }
  if (req.method === "POST" && url === "/v1/fork") {
    sandboxIds.add(body.id);
    return json(res, {
      id: body.id,
      template_id: body.template,
      endpoint: "http://localhost:8080",
      fork_time_ms: 0.8,
    });
  }
  if (req.method === "GET" && url === "/v1/sandboxes") {
    return json(
      res,
      Array.from(sandboxIds, (id) => ({
        id,
        template_id: "python",
        endpoint: "http://localhost:8080",
        created_at: "2026-06-11T00:00:00Z",
        fork_time_ms: 0.8,
      })),
    );
  }
  if (req.method === "POST" && url === "/v1/exec") {
    if (!sandboxIds.has(body.sandbox)) {
      return json(res, { error: "sandbox not found" }, 404);
    }
    return json(res, { exit_code: 0, stdout: "2\n", stderr: "", exec_time_ms: 5 });
  }
  if (req.method === "DELETE" && url.startsWith("/v1/sandboxes/")) {
    const id = decodeURIComponent(url.slice("/v1/sandboxes/".length));
    sandboxIds.delete(id);
    return json(res, { status: "terminated", id });
  }
  return json(res, { error: "not found" }, 404);
}

describe("SandboxServer", () => {
  it("lists templates", async () => {
    const s = new SandboxServer(baseUrl);
    const templates = await s.listTemplates();
    expect(templates).toEqual([
      { id: "python", ready: true, createdAt: "2026-06-11T00:00:00Z", creationTimeMs: 120 },
    ]);
    expect(recorded[0].method).toBe("GET");
  });

  it("creates a template with an init wait", async () => {
    const s = new SandboxServer(baseUrl);
    const t = await s.createTemplate("node", { initWaitSeconds: 3 });
    expect(t.id).toBe("node");
    expect(recorded[0].method).toBe("POST");
    expect(recorded[0].body).toEqual({ id: "node", init_wait_seconds: 3 });
  });

  it("forks a usable Sandbox that round-trips an exec", async () => {
    const s = new SandboxServer(baseUrl);
    const sandbox = await s.fork("python", "sbx-direct");
    expect(sandbox.id).toBe("sbx-direct");

    const forkCall = recorded.find((r) => r.url === "/v1/fork");
    expect(forkCall?.body).toEqual({ template: "python", id: "sbx-direct" });

    const result = await sandbox.exec("print(1 + 1)");
    expect(result.exitCode).toBe(0);
    expect(result.stdout).toBe("2\n");

    const execCall = recorded.find((r) => r.url === "/v1/exec");
    expect(execCall?.body).toEqual({ sandbox: "sbx-direct", command: "print(1 + 1)" });
  });

  it("forks with a generated id when none is given", async () => {
    const s = new SandboxServer(baseUrl);
    const sandbox = await s.fork("python");
    expect(sandbox.id).toMatch(/^sandbox-[0-9a-f]{8}$/);
  });

  it("terminate deletes the sandbox via the server", async () => {
    const s = new SandboxServer(baseUrl);
    const sandbox = await s.fork("python", "sbx-term");
    await sandbox.terminate();
    const del = recorded.find((r) => r.method === "DELETE");
    expect(del?.url).toBe("/v1/sandboxes/sbx-term");
    const remaining = await s.listSandboxes();
    expect(remaining.find((x) => x.id === "sbx-term")).toBeUndefined();
  });
});
