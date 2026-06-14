// Direct client for the standalone sandbox-server (cmd/sandbox-server). No
// Kubernetes required. Tokenless by design: the standalone server runs its
// sandbox API with AllowTokenless, so no bearer token is sent. Mirrors the
// Python SandboxServer (sdk/python/mitos/direct.py).

import { HttpClient, validSandboxId } from "./http.js";
import { Sandbox } from "./sandbox.js";
import { AgentRunError } from "./errors.js";

// Wire shapes from cmd/sandbox-server.
interface templateWire {
  id: string;
  ready: boolean;
  created_at: string;
  creation_time_ms: number;
}

interface forkWire {
  id: string;
  template_id: string;
  endpoint: string;
  fork_time_ms: number;
}

interface sandboxWire {
  id: string;
  template_id: string;
  endpoint: string;
  created_at: string;
  fork_time_ms: number;
}

/**
 * A template as reported by the sandbox-server.
 */
export interface Template {
  id: string;
  ready: boolean;
  createdAt: string;
  creationTimeMs: number;
}

/**
 * A sandbox summary as reported by the sandbox-server.
 */
export interface ServerSandbox {
  id: string;
  templateId: string;
  endpoint: string;
  createdAt: string;
  forkTimeMs: number;
}

function randomSandboxId(): string {
  // 8 random hex chars, matching the Python "sandbox-<hex>" convention.
  const bytes = new Uint8Array(4);
  globalThis.crypto.getRandomValues(bytes);
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `sandbox-${hex}`;
}

/**
 * Client for the standalone sandbox-server REST API. fork() returns a Sandbox
 * bound to the server (exec and files round-trip through the server URL, and
 * terminate issues DELETE /v1/sandboxes/{id}).
 */
export class SandboxServer {
  readonly url: string;
  private readonly http: HttpClient;

  constructor(url: string = "http://localhost:8080") {
    this.url = url.replace(/\/+$/, "");
    // Tokenless: the standalone server has no token-minting control plane.
    this.http = new HttpClient(this.url);
  }

  async listTemplates(): Promise<Template[]> {
    const out = await this.http.get<templateWire[]>("/v1/templates");
    return (out ?? []).map(toTemplate);
  }

  async createTemplate(
    id: string,
    opts?: { initWaitSeconds?: number },
  ): Promise<Template> {
    const out = await this.http.post<templateWire>("/v1/templates", {
      id,
      init_wait_seconds: opts?.initWaitSeconds ?? 5,
    });
    return toTemplate(out);
  }

  /**
   * Forks a sandbox from a named template. Returns a Sandbox bound to this
   * server (the per-sandbox bearer token applies only in cluster mode; direct
   * mode is tokenless). When `id` is omitted a random one is generated.
   */
  async fork(template: string, id?: string): Promise<Sandbox> {
    const sandboxId = id ?? randomSandboxId();
    if (!validSandboxId(sandboxId)) {
      throw new AgentRunError(`invalid sandbox id: ${JSON.stringify(sandboxId)}`, {
        code: "invalid_sandbox_id",
        cause: "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
        remediation:
          "Pass a sandbox id of alphanumerics, underscore, or hyphen, up to 64 chars.",
      });
    }
    const out = await this.http.post<forkWire>("/v1/fork", {
      template,
      id: sandboxId,
    });
    const resolvedId = out.id || sandboxId;
    // Exec and files round-trip through the server URL (the returned endpoint is
    // the server's own address); terminate deletes via the server.
    return new Sandbox({
      id: resolvedId,
      endpoint: this.url,
      http: this.http,
      terminator: async () => {
        // Direct mode has no workspaces; terminate deletes and reports unbound.
        await this.terminate(resolvedId);
        return undefined;
      },
    });
  }

  async listSandboxes(): Promise<ServerSandbox[]> {
    const out = await this.http.get<sandboxWire[]>("/v1/sandboxes");
    return (out ?? []).map(toServerSandbox);
  }

  private async terminate(id: string): Promise<void> {
    if (!validSandboxId(id)) {
      throw new AgentRunError(`invalid sandbox id: ${JSON.stringify(id)}`, {
        code: "invalid_sandbox_id",
        cause: "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
        remediation: "Terminate only ids that match the sandbox id allowlist.",
      });
    }
    await this.http.del(`/v1/sandboxes/${encodeURIComponent(id)}`);
  }
}

function toTemplate(t: templateWire): Template {
  return {
    id: t.id,
    ready: t.ready,
    createdAt: t.created_at,
    creationTimeMs: t.creation_time_ms,
  };
}

function toServerSandbox(s: sandboxWire): ServerSandbox {
  return {
    id: s.id,
    templateId: s.template_id,
    endpoint: s.endpoint,
    createdAt: s.created_at,
    forkTimeMs: s.fork_time_ms,
  };
}
