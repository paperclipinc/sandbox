import { describe, it, expect } from "vitest";
import type { Execution, Result, ExecutionError } from "../src/types.js";
import { parseRunCodeStream } from "../src/sandbox.js";

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
