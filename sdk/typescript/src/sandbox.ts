// A running sandbox: exec, file IO, and terminate over the sandbox API
// (forkd :9091 or the standalone sandbox-server). Mirrors the Python Sandbox
// surface (sdk/python/agent_run/sandbox.py), camelCased.

import { AgentRunError } from "./errors.js";
import { HttpClient, validSandboxId } from "./http.js";
import type { ExecResult, FileInfo } from "./types.js";

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
   * Runs a command in the sandbox. POSTs /v1/exec {sandbox, command, timeout}
   * and maps the snake_case response into an ExecResult.
   */
  async exec(
    command: string,
    opts?: { timeoutSeconds?: number },
  ): Promise<ExecResult> {
    const body: Record<string, unknown> = {
      sandbox: this.id,
      command,
    };
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
