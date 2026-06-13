// Cluster client for the mitos runtime on Kubernetes. Creates SandboxClaims
// (mitos.run/v1alpha1), polls them to Ready, reads the per-sandbox bearer
// token Secret, and hands back a Sandbox bound to the claim's HTTP endpoint.
// Mirrors the Python AgentRun (sdk/python/mitos/client.py). The token is
// read into memory only and is never logged.

import { AgentRunError } from "./errors.js";
import type { CustomObject, K8sApi } from "./k8s.js";
import { Sandbox, toBaseUrl } from "./sandbox.js";
import type { SandboxInfo, SandboxPhase } from "./types.js";

const API_GROUP = "mitos.run";
const API_VERSION = "v1alpha1";
// Suffix of the Secret holding a claim's sandbox API bearer token. Mirrors the
// controller constant and internal/agentcli tokenSecretSuffix.
const TOKEN_SECRET_SUFFIX = "-sandbox-token";

const DEFAULT_POLL_TIMEOUT_MS = 30_000;
const POLL_INTERVAL_MS = 50;

export interface AgentRunOptions {
  namespace?: string;
  k8s?: K8sApi;
  pollTimeoutMs?: number;
  /** Override the poll wait, for tests. Defaults to a real setTimeout. */
  sleep?: (ms: number) => Promise<void>;
}

export interface CreateOptions {
  name?: string;
  env?: Record<string, string>;
  timeout?: string;
}

function randomName(): string {
  const bytes = new Uint8Array(4);
  globalThis.crypto.getRandomValues(bytes);
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `sandbox-${hex}`;
}

function defaultSleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Cluster client. Requires a K8sApi: in production pass a KubeConfigApi; in
 * tests pass a fake.
 */
export class AgentRun {
  private readonly namespace: string;
  private readonly k8s: K8sApi;
  private readonly pollTimeoutMs: number;
  private readonly sleep: (ms: number) => Promise<void>;

  constructor(opts?: AgentRunOptions) {
    if (!opts?.k8s) {
      throw new AgentRunError("AgentRun requires a K8sApi implementation", {
        code: "missing_k8s_api",
        cause: "no k8s client was provided",
        remediation:
          "Pass { k8s: new KubeConfigApi() } for cluster mode, or use SandboxServer for direct mode.",
      });
    }
    this.namespace = opts.namespace ?? "default";
    this.k8s = opts.k8s;
    this.pollTimeoutMs = opts.pollTimeoutMs ?? DEFAULT_POLL_TIMEOUT_MS;
    this.sleep = opts.sleep ?? defaultSleep;
  }

  /**
   * Creates a sandbox from a pool: builds a SandboxClaim, polls until Ready,
   * reads the token Secret and status endpoint, and returns a bound Sandbox.
   */
  async create(pool: string, opts?: CreateOptions): Promise<Sandbox> {
    const name = opts?.name ?? randomName();
    const spec: Record<string, unknown> = {
      poolRef: { name: pool },
    };
    if (opts?.env) {
      spec["env"] = Object.entries(opts.env).map(([k, v]) => ({ name: k, value: v }));
    }
    if (opts?.timeout) {
      spec["timeout"] = opts.timeout;
    }

    const claim: CustomObject = {
      apiVersion: `${API_GROUP}/${API_VERSION}`,
      kind: "SandboxClaim",
      metadata: { name, namespace: this.namespace },
      spec,
    };

    await this.k8s.createClaim(this.namespace, claim);
    const { endpoint } = await this.waitReady(name);

    // Read the per-sandbox bearer token. A missing Secret is tolerated: the
    // sandbox stays tokenless and the API answers 401, surfacing the
    // misconfiguration without crashing. The value is never logged.
    let token: string | undefined;
    let secretEndpoint = "";
    try {
      const secret = await this.k8s.readSecret(this.namespace, name + TOKEN_SECRET_SUFFIX);
      token = secret["token"] || undefined;
      secretEndpoint = secret["endpoint"] ?? "";
    } catch {
      // No token Secret yet; proceed tokenless.
    }

    const resolved = endpoint || secretEndpoint;
    return new Sandbox({
      id: name,
      endpoint: toBaseUrl(resolved),
      token,
      terminator: async () => {
        await this.k8s.deleteClaim(this.namespace, name);
      },
    });
  }

  /**
   * Lists sandboxes (SandboxClaims) mapped to SandboxInfo, optionally filtered
   * by pool.
   */
  async list(pool?: string): Promise<SandboxInfo[]> {
    const list = await this.k8s.listClaims(this.namespace);
    const out: SandboxInfo[] = [];
    for (const obj of list.items ?? []) {
      const objPool = readString(obj.spec, ["poolRef", "name"]);
      if (pool && objPool !== pool) {
        continue;
      }
      const status = obj.status ?? {};
      out.push({
        name: obj.metadata?.name ?? "",
        phase: (status["phase"] as SandboxPhase) ?? "Pending",
        endpoint: (status["endpoint"] as string) ?? "",
        node: (status["node"] as string) ?? "",
        sandboxId: (status["sandboxID"] as string) ?? "",
        forkTimeMs: forkTimeMs(status),
        pool: objPool,
      });
    }
    return out;
  }

  private async waitReady(name: string): Promise<{ endpoint: string }> {
    const deadline = Date.now() + this.pollTimeoutMs;
    for (;;) {
      const obj = await this.k8s.getClaim(this.namespace, name);
      const status = obj.status ?? {};
      const phase = status["phase"] as SandboxPhase | undefined;
      const endpoint = (status["endpoint"] as string) ?? "";

      if (phase === "Ready" && endpoint !== "") {
        return { endpoint };
      }
      if (phase === "Failed") {
        throw new AgentRunError(`sandbox ${name} failed`, {
          code: "sandbox_failed",
          cause: `claim ${name} reached the Failed phase`,
          remediation:
            "Inspect the SandboxClaim status conditions and the pool capacity.",
        });
      }
      if (Date.now() >= deadline) {
        throw new AgentRunError(
          `sandbox ${name} not ready after ${this.pollTimeoutMs}ms`,
          {
            code: "ready_timeout",
            cause: `claim ${name} did not reach Ready within ${this.pollTimeoutMs}ms`,
            remediation:
              "Raise pollTimeoutMs, or check the controller is reconciling and the pool has capacity.",
          },
        );
      }
      await this.sleep(POLL_INTERVAL_MS);
    }
  }
}

function readString(obj: Record<string, unknown> | undefined, path: string[]): string {
  let cur: unknown = obj;
  for (const key of path) {
    if (cur && typeof cur === "object" && key in (cur as Record<string, unknown>)) {
      cur = (cur as Record<string, unknown>)[key];
    } else {
      return "";
    }
  }
  return typeof cur === "string" ? cur : "";
}

function forkTimeMs(status: Record<string, unknown>): number {
  const micros = status["forkTimeMicros"];
  if (typeof micros === "number") {
    return micros / 1000;
  }
  const ms = status["forkTimeMs"];
  return typeof ms === "number" ? ms : 0;
}
