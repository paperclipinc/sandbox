import { describe, it, expect, vi } from "vitest";
import { Sandbox } from "../src/sandbox.js";

function ndjsonResponse(lines: string[]): Response {
  const body = lines.map((l) => l + "\n").join("");
  return new Response(body, {
    status: 200,
    headers: { "Content-Type": "application/x-ndjson" },
  });
}

function b64(s: string): string {
  return Buffer.from(s, "utf8").toString("base64");
}

describe("streaming exec", () => {
  it("invokes onStdout/onStderr per chunk and returns aggregate", async () => {
    const lines = [
      JSON.stringify({ stream: "stdout", data: b64("out1") }),
      JSON.stringify({ stream: "stderr", data: b64("err1") }),
      JSON.stringify({ stream: "stdout", data: b64("out2") }),
      JSON.stringify({ exit_code: 7, exec_time_ms: 2 }),
    ];
    const fetchMock = vi.fn().mockResolvedValue(ndjsonResponse(lines));
    vi.stubGlobal("fetch", fetchMock);

    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const out: string[] = [];
    const err: string[] = [];
    const result = await sb.exec("echo hi", {
      onStdout: (b) => out.push(new TextDecoder().decode(b)),
      onStderr: (b) => err.push(new TextDecoder().decode(b)),
    });

    expect(out.join("")).toBe("out1out2");
    expect(err.join("")).toBe("err1");
    expect(result.exitCode).toBe(7);
    expect(result.stdout).toBe("out1out2");
    vi.unstubAllGlobals();
  });

  it("execBackground returns a handle whose wait() drains the stream", async () => {
    const lines = [
      JSON.stringify({ stream: "stdout", data: b64("ready") }),
      JSON.stringify({ exit_code: 0, exec_time_ms: 1 }),
    ];
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ndjsonResponse(lines)));
    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const proc = await sb.execBackground("sleep 1");
    const result = await proc.wait();
    expect(result.exitCode).toBe(0);
    expect(result.stdout).toBe("ready");
    vi.unstubAllGlobals();
  });
});
