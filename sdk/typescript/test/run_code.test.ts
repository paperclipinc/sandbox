import { describe, it, expect } from "vitest";
import type { Execution, Result, ExecutionError } from "../src/types.js";
import { parseRunCodeStream } from "../src/sandbox.js";
import { AgentRunError } from "../src/errors.js";

function b64(s: string): string {
  return Buffer.from(s, "utf-8").toString("base64");
}

async function* lines(...objs: unknown[]): AsyncIterable<string> {
  for (const o of objs) {
    yield JSON.stringify(o);
  }
}

describe("parseRunCodeStream", () => {
  it("accumulates frames and fires callbacks", async () => {
    const seenStdout: string[] = [];
    const seenResults: Result[] = [];
    const ex = await parseRunCodeStream(
      lines(
        { kind: "stdout", stdout: b64("hi\n") },
        { kind: "result", result: { text: "42", data: { "text/plain": "42", "image/png": "aGVsbG8=" } } },
        { kind: "exit", exit_code: 0 },
      ),
      { onStdout: (s) => seenStdout.push(s), onResult: (r) => seenResults.push(r) },
    );
    expect(ex.text).toBe("42");
    expect(ex.logs.stdout).toEqual(["hi\n"]);
    expect(ex.results[0].data["image/png"]).toBe("aGVsbG8=");
    expect(seenStdout).toEqual(["hi\n"]);
    expect(seenResults.length).toBe(1);
    expect(ex.error).toBeNull();
  });

  it("captures a structured error", async () => {
    const ex = await parseRunCodeStream(
      lines(
        { kind: "error", error: { name: "ValueError", value: "bad", traceback: ["ValueError: bad"] } },
        { kind: "exit", exit_code: 1 },
      ),
      {},
    );
    expect(ex.error?.name).toBe("ValueError");
    expect(ex.text).toBeNull();
  });

  it("throws when the stream ends without a terminal exit frame", async () => {
    // A truncated or dropped connection: frames arrive but no exit frame. This
    // must throw rather than resolve to a misleading clean Execution.
    const promise = parseRunCodeStream(
      lines(
        { kind: "stdout", stdout: b64("partial\n") },
        { kind: "result", result: { text: "7", data: { "text/plain": "7" } } },
      ),
      {},
    );
    await expect(promise).rejects.toBeInstanceOf(AgentRunError);
    await expect(promise).rejects.toMatchObject({ code: "run_code_stream_truncated" });
  });
});

describe("run_code types", () => {
  it("Execution holds the E2B shape", () => {
    const result: Result = {
      data: { "image/png": "aGVsbG8=", "text/plain": "42" },
      isMainResult: true,
    };
    const err: ExecutionError = { name: "ValueError", value: "bad", traceback: ["ValueError: bad"] };
    const ex: Execution = {
      text: "42",
      logs: { stdout: ["hi\n"], stderr: [] },
      results: [result],
      error: null,
    };
    expect(ex.results[0].data["image/png"]).toBe("aGVsbG8=");
    expect(err.name).toBe("ValueError");
    expect(ex.text).toBe("42");
  });
});
