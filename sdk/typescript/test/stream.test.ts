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

  // Issue A: a body that ends before the terminal exit frame is a truncated
  // stream and must surface as an error, not exitCode=0 success.
  it("errors when the stream ends before the exit frame", async () => {
    const lines = [
      JSON.stringify({ stream: "stdout", data: b64("out1") }),
      JSON.stringify({ stream: "stdout", data: b64("out2") }),
      // No exit frame: the body simply ends.
    ];
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ndjsonResponse(lines)));
    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });

    await expect(
      sb.exec("echo hi", { onStdout: () => {} }),
    ).rejects.toMatchObject({
      name: "AgentRunError",
      code: "exec_stream_truncated",
    });
    vi.unstubAllGlobals();
  });

  // Issue B: kill() aborts the underlying fetch. The signal must be threaded
  // into fetch so a quiet background process is torn down immediately, not only
  // at the next chunk.
  it("threads the AbortSignal into fetch so kill() aborts promptly", async () => {
    let seenSignal: AbortSignal | undefined;
    // A body that never delivers an exit frame and whose reader.read() hangs
    // until the signal aborts. The fetch receives the signal and rejects the
    // read when aborted, mirroring the platform fetch behavior.
    const fetchMock = vi.fn().mockImplementation((_url, init: RequestInit) => {
      seenSignal = init.signal ?? undefined;
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          // Emit one early chunk, then block; aborting cancels the stream.
          controller.enqueue(
            new TextEncoder().encode(
              JSON.stringify({ stream: "stdout", data: b64("ready") }) + "\n",
            ),
          );
          const onAbort = () => {
            controller.error(
              Object.assign(new Error("aborted"), { name: "AbortError" }),
            );
          };
          if (init.signal?.aborted) {
            onAbort();
          } else {
            init.signal?.addEventListener("abort", onAbort, { once: true });
          }
        },
      });
      return Promise.resolve(
        new Response(body, {
          status: 200,
          headers: { "Content-Type": "application/x-ndjson" },
        }),
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const proc = await sb.execBackground("sleep 1");
    // fetch must have received an AbortSignal.
    expect(seenSignal).toBeInstanceOf(AbortSignal);
    expect(seenSignal!.aborted).toBe(false);

    proc.kill();
    expect(seenSignal!.aborted).toBe(true);

    // wait() resolves promptly (no truncation error thrown despite there being
    // no exit frame, because the abort is recognised as an intentional kill).
    const result = await proc.wait();
    expect(result.exitCode).toBe(0);
    vi.unstubAllGlobals();
  });

  // Issue B: an abort surfacing as a rejected reader.read() (AbortError) is
  // treated as a kill, not a truncation error.
  it("treats an AbortError from the reader as a kill, not a truncation", async () => {
    const fetchMock = vi.fn().mockImplementation((_url, init: RequestInit) => {
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          const onAbort = () =>
            controller.error(
              Object.assign(new Error("aborted"), { name: "AbortError" }),
            );
          init.signal?.addEventListener("abort", onAbort, { once: true });
        },
      });
      return Promise.resolve(
        new Response(body, {
          status: 200,
          headers: { "Content-Type": "application/x-ndjson" },
        }),
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const proc = await sb.execBackground("sleep 1");
    proc.kill();
    // Resolves without throwing the truncation error.
    await expect(proc.wait()).resolves.toMatchObject({ exitCode: 0 });
    vi.unstubAllGlobals();
  });
});
