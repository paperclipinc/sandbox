// An interactive pseudo-terminal in a sandbox, mirroring E2B's sandbox.pty.
// Transport is a WebSocket to the sandbox API's /v1/pty (subprotocol
// mitos.pty.v1), gated by the per-sandbox bearer token. The token is sent in
// the Authorization header (the Node `ws` client supports request headers,
// which the platform global WebSocket does not). It is never logged.

import WebSocket from "ws";

/** Frame kinds on the PTY WebSocket. input/resize flow client->guest;
 * output/exit flow guest->client. */
type PtyFrame =
  | { kind: "input"; data: string }
  | { kind: "resize"; cols: number; rows: number }
  | { kind: "output"; data: string }
  | { kind: "exit"; exit_code: number; error?: string };

export interface CreatePtyOptions {
  /** Full ws:// or wss:// URL including ?sandbox=&cols=&rows=. */
  url: string;
  /** Per-sandbox bearer token; sent in the Authorization header. */
  token?: string;
  /** Receives raw terminal output bytes as they arrive. */
  onData: (data: Uint8Array) => void;
}

/** A live interactive terminal handle. */
export class Pty {
  private readonly ws: WebSocket;
  private readonly onData: (data: Uint8Array) => void;
  private exitCode: number | undefined;
  private readonly exited: Promise<number>;
  private resolveExit!: (code: number) => void;

  constructor(ws: WebSocket, onData: (data: Uint8Array) => void) {
    this.ws = ws;
    this.onData = onData;
    this.exited = new Promise<number>((resolve) => {
      this.resolveExit = resolve;
    });
    this.ws.on("message", (raw: WebSocket.RawData) => this.handleMessage(raw));
    this.ws.on("close", () => {
      if (this.exitCode === undefined) {
        this.exitCode = -1;
        this.resolveExit(-1);
      }
    });
  }

  private handleMessage(raw: WebSocket.RawData): void {
    const text = raw.toString();
    const frame = JSON.parse(text) as PtyFrame;
    if (frame.kind === "output") {
      this.onData(b64ToBytes(frame.data));
    } else if (frame.kind === "exit") {
      this.exitCode = frame.exit_code;
      this.resolveExit(frame.exit_code);
      this.ws.close();
    }
  }

  /** Send raw keystroke bytes to the shell. */
  sendInput(data: Uint8Array): void {
    const frame: PtyFrame = { kind: "input", data: bytesToB64(data) };
    this.ws.send(JSON.stringify(frame));
  }

  /** Resize the terminal (TIOCSWINSZ in the guest, then SIGWINCH). */
  resize(cols: number, rows: number): void {
    const frame: PtyFrame = { kind: "resize", cols, rows };
    this.ws.send(JSON.stringify(frame));
  }

  /** Force-close; the guest kills the shell process group on disconnect. */
  kill(): void {
    this.ws.close();
    if (this.exitCode === undefined) {
      this.exitCode = -1;
      this.resolveExit(-1);
    }
  }

  /** Resolve with the shell exit code (or -1 if the connection dropped before
   * a terminal exit frame). */
  wait(): Promise<number> {
    return this.exited;
  }
}

/** Open a PTY WebSocket and resolve once it is open. The bearer token rides the
 * Authorization request header (supported by the Node `ws` client), so forkd's
 * ptyAuth gate sees it on the upgrade. */
export function createPty(opts: CreatePtyOptions): Promise<Pty> {
  return new Promise((resolve, reject) => {
    const headers: Record<string, string> = {};
    if (opts.token) {
      headers["Authorization"] = `Bearer ${opts.token}`;
    }
    const ws = new WebSocket(opts.url, ["mitos.pty.v1"], { headers });
    const pty = new Pty(ws, opts.onData);
    ws.on("open", () => resolve(pty));
    ws.on("error", (err: Error) =>
      reject(new Error(`pty websocket error: ${err.message}`)),
    );
  });
}

function bytesToB64(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) {
    bin += String.fromCharCode(b);
  }
  return btoa(bin);
}

function b64ToBytes(b64: string): Uint8Array {
  if (!b64) {
    return new Uint8Array(0);
  }
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) {
    out[i] = bin.charCodeAt(i);
  }
  return out;
}
