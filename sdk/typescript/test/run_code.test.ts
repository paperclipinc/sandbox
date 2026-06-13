import { describe, it, expect } from "vitest";
import type { Execution, Result, ExecutionError } from "../src/types.js";

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
