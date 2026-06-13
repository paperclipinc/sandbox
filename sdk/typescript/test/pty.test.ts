import { describe, it, expect, afterEach } from "vitest";
import { WebSocketServer } from "ws";
import { createPty } from "../src/pty.js";

let server: WebSocketServer | undefined;

afterEach(() => {
  server?.close();
  server = undefined;
});

function startEchoServer(): Promise<number> {
  return new Promise((resolve) => {
    server = new WebSocketServer({ port: 0 });
    server.on("connection", (sock) => {
      sock.on("message", (raw: Buffer) => {
        const frame = JSON.parse(raw.toString());
        if (frame.kind === "input") {
          const data: string = frame.data ?? "";
          const decoded = Buffer.from(data, "base64").toString("utf8");
          if (decoded === "exit\n") {
            sock.send(JSON.stringify({ kind: "exit", exit_code: 0 }));
            return;
          }
          sock.send(JSON.stringify({ kind: "output", data }));
        }
      });
    });
    server.on("listening", () => {
      const addr = server!.address();
      resolve(typeof addr === "object" && addr ? addr.port : 0);
    });
  });
}

describe("pty", () => {
  it("echoes input and reports exit", async () => {
    const port = await startEchoServer();
    const chunks: Uint8Array[] = [];
    const pty = await createPty({
      url: `ws://127.0.0.1:${port}/v1/pty?sandbox=sb1&cols=80&rows=24`,
      onData: (b) => chunks.push(b),
    });

    pty.sendInput(new TextEncoder().encode("ts-hi\n"));
    await new Promise((r) => setTimeout(r, 200));
    const got = new TextDecoder().decode(
      Uint8Array.from(chunks.flatMap((c) => Array.from(c))),
    );
    expect(got).toBe("ts-hi\n");

    pty.sendInput(new TextEncoder().encode("exit\n"));
    const code = await pty.wait();
    expect(code).toBe(0);
  });

  it("resize does not throw", async () => {
    const port = await startEchoServer();
    const pty = await createPty({
      url: `ws://127.0.0.1:${port}/v1/pty?sandbox=sb1`,
      onData: () => {},
    });
    pty.resize(120, 40);
    pty.sendInput(new TextEncoder().encode("exit\n"));
    expect(await pty.wait()).toBe(0);
  });
});
