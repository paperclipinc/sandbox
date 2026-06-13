// A running sandbox: exec, file IO, and terminate over the sandbox API
// (forkd :9091 or the standalone sandbox-server). Mirrors the Python Sandbox
// surface (sdk/python/mitos/sandbox.py), camelCased.

import { AgentRunError } from "./errors.js";
import { HttpClient, validSandboxId } from "./http.js";
import type { BackgroundProcess, ExecResult, FileInfo } from "./types.js";

/** A function that tears a sandbox down. Injected by the owning client so the
 * cluster client deletes a SandboxClaim while the direct client issues a
 * DELETE /v1/sandboxes/{id}. */
export type Terminator = () => Promise<void>;

export interface SandboxOptions {
  id: string;
  endpoint: string;
  token?: string;
  /** Pre-built transport. When omitted, one is built from endpoint + token. */
  http?: HttpClient;
  /** Custom teardown. When omitted, terminate() is a no-op for the bare
   * Sandbox (the owning client supplies one). */
  terminator?: Terminator;
}

// Wire shapes returned by the sandbox API. snake_case as the Go handlers emit.
interface execResponseWire {
  exit_code: number;
  stdout?: string;
  stderr?: string;
  exec_time_ms?: number;
}

interface readResponseWire {
  content: string;
  size?: number;
}

interface listEntryWire {
  name: string;
  is_dir: boolean;
  size: number;
  mode?: number;
  modified_at?: string;
}

interface listResponseWire {
  entries: listEntryWire[];
}

/**
 * File operations on a sandbox. POST /v1/files/{read,write,list}.
 */
export class SandboxFiles {
  constructor(
    private readonly sandbox: Sandbox,
    private readonly http: HttpClient,
  ) {}

  async read(path: string): Promise<string> {
    const resp = await this.http.post<readResponseWire>("/v1/files/read", {
      sandbox: this.sandbox.id,
      path,
    });
    return resp.content;
  }

  async write(
    path: string,
    content: string,
    opts?: { mode?: number },
  ): Promise<void> {
    const body: Record<string, unknown> = {
      sandbox: this.sandbox.id,
      path,
      content,
    };
    if (opts?.mode !== undefined) {
      body["mode"] = opts.mode;
    }
    await this.http.post<{ status?: string }>("/v1/files/write", body);
  }

  async list(path: string = "/"): Promise<FileInfo[]> {
    const resp = await this.http.post<listResponseWire>("/v1/files/list", {
      sandbox: this.sandbox.id,
      path,
    });
    const entries = resp.entries ?? [];
    return entries.map((e) => ({
      name: e.name,
      isDir: e.is_dir,
      size: e.size,
      mode: e.mode ?? 0,
      modifiedAt: e.modified_at,
    }));
  }
}

/**
 * A running sandbox instance. Holds {id, endpoint, token, http} and exposes
 * exec, files, and terminate.
 */
export class Sandbox {
  readonly id: string;
  readonly endpoint: string;
  readonly files: SandboxFiles;

  private readonly http: HttpClient;
  private readonly terminator?: Terminator;

  constructor(opts: SandboxOptions) {
    if (!validSandboxId(opts.id)) {
      throw new AgentRunError(`invalid sandbox id: ${JSON.stringify(opts.id)}`, {
        code: "invalid_sandbox_id",
        cause: "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
        remediation:
          "Use a sandbox id of alphanumerics, underscore, or hyphen (no '/' or '..'), up to 64 chars.",
      });
    }
    this.id = opts.id;
    this.endpoint = opts.endpoint;
    this.http = opts.http ?? new HttpClient(toBaseUrl(opts.endpoint), opts.token);
    this.terminator = opts.terminator;
    this.files = new SandboxFiles(this, this.http);
  }

  /**
   * Runs a command in the sandbox. With no stream callbacks it POSTs /v1/exec
   * and maps the snake_case response. With onStdout/onStderr it streams
   * /v1/exec/stream (NDJSON) and fires the callbacks per chunk while still
   * resolving the full aggregate ExecResult.
   */
  async exec(
    command: string,
    opts?: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
  ): Promise<ExecResult> {
    if (!opts?.onStdout && !opts?.onStderr) {
      const body: Record<string, unknown> = { sandbox: this.id, command };
      if (opts?.timeoutSeconds !== undefined) {
        body["timeout"] = opts.timeoutSeconds;
      }
      const resp = await this.http.post<execResponseWire>("/v1/exec", body);
      return {
        exitCode: resp.exit_code,
        stdout: resp.stdout ?? "",
        stderr: resp.stderr ?? "",
        execTimeMs: resp.exec_time_ms,
      };
    }
    return this.streamExec(command, opts);
  }

  /**
   * Starts a long-running command and returns a handle. wait() drains the
   * stream; kill() aborts it so forkd cancels the guest process group. The
   * default timeout is one day so a background server is not reaped by the
   * per-exec timeout.
   */
  async execBackground(
    command: string,
    opts?: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
  ): Promise<BackgroundProcess> {
    const controller = new AbortController();
    const timeout = opts?.timeoutSeconds ?? 86400;
    const promise = this.streamExec(
      command,
      { ...opts, timeoutSeconds: timeout },
      controller.signal,
    );
    return {
      wait: () => promise,
      kill: () => controller.abort(),
    };
  }

  private async streamExec(
    command: string,
    opts: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
    signal?: AbortSignal,
  ): Promise<ExecResult> {
    const body: Record<string, unknown> = { sandbox: this.id, command };
    if (opts.timeoutSeconds !== undefined) {
      body["timeout"] = opts.timeoutSeconds;
    }
    const resp = await this.http.postStream("/v1/exec/stream", body, signal);
    const reader = resp.body!.getReader();
    const decoder = new TextDecoder();
    const td = new TextDecoder();
    let buffered = "";
    let exitCode = 0;
    let execTimeMs: number | undefined;
    let sawExit = false;
    const outParts: string[] = [];
    const errParts: string[] = [];

    const handleLine = (line: string) => {
      if (line === "") return;
      const frame = JSON.parse(line) as {
        stream?: string;
        data?: string;
        exit_code?: number;
        exec_time_ms?: number;
        error?: string;
      };
      if (frame.exit_code !== undefined && frame.stream === undefined) {
        exitCode = frame.exit_code;
        execTimeMs = frame.exec_time_ms;
        sawExit = true;
        if (frame.error) {
          throw new AgentRunError(`exec stream error: ${frame.error}`, {
            code: "exec_stream_error",
            cause: frame.error,
            remediation: "Inspect the command and the forkd logs for the failure.",
          });
        }
        return;
      }
      const bytes = frame.data
        ? Uint8Array.from(Buffer.from(frame.data, "base64"))
        : new Uint8Array();
      const text = td.decode(bytes);
      if (frame.stream === "stderr") {
        errParts.push(text);
        opts.onStderr?.(bytes);
      } else {
        outParts.push(text);
        opts.onStdout?.(bytes);
      }
    };

    let aborted = false;
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (signal?.aborted) {
          aborted = true;
          await reader.cancel();
          break;
        }
        if (done) break;
        buffered += decoder.decode(value, { stream: true });
        let nl: number;
        while ((nl = buffered.indexOf("\n")) >= 0) {
          const line = buffered.slice(0, nl);
          buffered = buffered.slice(nl + 1);
          handleLine(line);
        }
      }
    } catch (e) {
      // An abort tears the fetch down: reader.read() rejects with an
      // AbortError. That is an intentional kill, not a truncation; fall through
      // and return the partial result rather than the truncation error below.
      if (signal?.aborted || (e instanceof Error && e.name === "AbortError")) {
        aborted = true;
      } else {
        throw e;
      }
    }
    if (!aborted && buffered.trim() !== "") {
      handleLine(buffered.trim());
    }

    if (!aborted && !sawExit) {
      // The body ended before the terminal exit frame: the stream was
      // truncated or dropped. Surface it as an error rather than a misleading
      // exitCode=0 success.
      throw new AgentRunError(
        "exec stream ended before the terminal exit frame",
        {
          code: "exec_stream_truncated",
          cause:
            "the connection was truncated or dropped; the exit code is unknown",
          remediation:
            "Retry the command; if it persists, inspect the forkd or sandbox-server logs for a dropped connection.",
        },
      );
    }

    return {
      exitCode,
      stdout: outParts.join(""),
      stderr: errParts.join(""),
      execTimeMs,
    };
  }

  /**
   * Tears the sandbox down via the injected terminator. A bare Sandbox with no
   * terminator is a no-op.
   */
  async terminate(): Promise<void> {
    if (this.terminator) {
      await this.terminator();
    }
  }
}

/**
 * Normalises an endpoint into a base URL. An endpoint that already carries a
 * scheme is used as-is; a bare host:port (as the cluster status reports) gets
 * an http:// prefix.
 */
export function toBaseUrl(endpoint: string): string {
  if (/^https?:\/\//.test(endpoint)) {
    return endpoint;
  }
  return `http://${endpoint}`;
}
