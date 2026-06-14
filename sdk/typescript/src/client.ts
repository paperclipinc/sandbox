// Cluster client for the mitos runtime on Kubernetes. Creates SandboxClaims
// (mitos.run/v1alpha1), polls them to Ready, reads the per-sandbox bearer
// token Secret, and hands back a Sandbox bound to the claim's HTTP endpoint.
// Mirrors the Python AgentRun (sdk/python/mitos/client.py). The token is
// read into memory only and is never logged.

import { AgentRunError } from "./errors.js";
import type { CustomObject, K8sApi } from "./k8s.js";
import { Sandbox, toBaseUrl } from "./sandbox.js";
import type { TerminateOptions } from "./sandbox.js";
import type { SandboxInfo, SandboxPhase } from "./types.js";
import { Workspace } from "./workspace.js";

const API_GROUP = "mitos.run";
const API_VERSION = "v1alpha1";
// Suffix of the Secret holding a claim's sandbox API bearer token. Mirrors the
// controller constant and internal/agentcli tokenSecretSuffix.
const TOKEN_SECRET_SUFFIX = "-sandbox-token";

const DEFAULT_POLL_TIMEOUT_MS = 30_000;
const POLL_INTERVAL_MS = 50;

const DEFAULT_POOL_PREFIX = "mitos-default-";

/**
 * Deterministic default-pool name for an image: lowercased, "/" and ":" to "-",
 * other unsafe characters collapsed, bounded to 40 chars after the prefix, with
 * leading/trailing "-" and "." stripped (a trailing "." is an invalid object
 * name). Kept byte-for-byte equivalent to the Python default_pool_name so both
 * SDKs target the same pool object.
 */
export function defaultPoolName(image: string): string {
  const collapsed = image
    .toLowerCase()
    .replace(/[/:]/g, "-")
    .replace(/[^a-z0-9.-]+/g, "-");
  // Bound first, then strip trailing/leading "-" and "." so truncation can
  // never leave a name ending in "." or "-" (both invalid object-name tails).
  const slug = collapsed.slice(0, 40).replace(/^[-.]+|[-.]+$/g, "");
  return DEFAULT_POOL_PREFIX + slug;
}

function statusOf(e: unknown): number | undefined {
  if (e && typeof e === "object") {
    const anyE = e as { statusCode?: number; response?: { statusCode?: number } };
    return anyE.statusCode ?? anyE.response?.statusCode;
  }
  return undefined;
}

function isNotFound(e: unknown): boolean {
  return statusOf(e) === 404;
}

function isConflict(e: unknown): boolean {
  return statusOf(e) === 409;
}

export interface AgentRunOptions {
  namespace?: string;
  k8s?: K8sApi;
  pollTimeoutMs?: number;
  /** Override the poll wait, for tests. Defaults to a real setTimeout. */
  sleep?: (ms: number) => Promise<void>;
  /**
   * Whether sandbox(image) may lazily create a default pool when none is named.
   * Defaults to true; set false to require an explicit pool (admin opt-out).
   */
  allowDefaultPool?: boolean;
}

export interface CreateOptions {
  name?: string;
  env?: Record<string, string>;
  timeout?: string;
  /**
   * Bind the sandbox to a durable Workspace by name. On activation the
   * controller hydrates the workspace head into /workspace; on terminate it
   * dehydrates a new committed revision.
   */
  workspace?: string;
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
  private readonly allowDefaultPool: boolean;

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
    this.allowDefaultPool = opts.allowDefaultPool ?? true;
  }

  /**
   * The one-liner entry point (docs/api/v2-spec.md section 1.2). Pass an image
   * for the lazy path (ensures mitos-default-<image-slug>, creating it and its
   * SandboxTemplate if absent and allowed) or { pool } for the explicit path,
   * which never creates a pool. Exactly one of image or { pool } is required.
   */
  async sandbox(
    image?: string,
    opts?: CreateOptions & { pool?: string },
  ): Promise<Sandbox> {
    let pool = opts?.pool;
    if (!pool && !image) {
      throw new AgentRunError("sandbox() needs an image or a pool", {
        code: "missing_image_or_pool",
        remediation: 'Pass an image ("python") or { pool: "my-pool" }.',
      });
    }
    if (!pool) {
      if (!this.allowDefaultPool) {
        throw new AgentRunError("default pools are disabled on this client", {
          code: "no_default_pool",
          remediation:
            "Pass { pool } for an existing pool, or set allowDefaultPool: true.",
        });
      }
      pool = await this.ensureDefaultPool(image as string);
    }
    return this.create(pool, opts);
  }

  /**
   * Reconnect to an existing sandbox by name: a durable handle across
   * processes. Resolves the endpoint and the per-sandbox token from the
   * cluster. The token is read into memory only and is never logged.
   */
  async fromName(name: string): Promise<Sandbox> {
    const obj = await this.k8s.getClaim(this.namespace, name);
    const status = obj.status ?? {};
    const endpoint = (status["endpoint"] as string) ?? "";
    let token: string | undefined;
    try {
      const secret = await this.k8s.readSecret(this.namespace, name + TOKEN_SECRET_SUFFIX);
      token = secret["token"] || undefined;
    } catch {
      // No token Secret; proceed tokenless.
    }
    return new Sandbox({
      id: name,
      endpoint: toBaseUrl(endpoint),
      token,
      terminator: this.makeTerminator(name),
    });
  }

  private async ensureDefaultPool(image: string): Promise<string> {
    const name = defaultPoolName(image);
    try {
      const existing = await this.k8s.getPool(this.namespace, name);
      await this.verifyPoolImage(existing, name, image);
      return name;
    } catch (e) {
      if (e instanceof AgentRunError) {
        throw e;
      }
      if (!isNotFound(e)) {
        throw e;
      }
    }
    // The CRD splits image from pool: SandboxTemplate.spec.image, SandboxPool.
    // spec.templateRef. Create both under the same deterministic name.
    const template: CustomObject = {
      apiVersion: `${API_GROUP}/${API_VERSION}`,
      kind: "SandboxTemplate",
      metadata: { name, namespace: this.namespace },
      spec: { image },
    };
    try {
      await this.k8s.createTemplate(this.namespace, template);
    } catch (e) {
      if (!isConflict(e)) {
        throw e; // 409: another caller raced us; reuse it
      }
    }
    const pool: CustomObject = {
      apiVersion: `${API_GROUP}/${API_VERSION}`,
      kind: "SandboxPool",
      metadata: { name, namespace: this.namespace },
      spec: { templateRef: { name }, replicas: 1 },
    };
    try {
      await this.k8s.createPool(this.namespace, pool);
    } catch (e) {
      if (!isConflict(e)) {
        throw e;
      }
    }
    return name;
  }

  /**
   * Guards the default-pool reuse path against a slug collision serving the
   * wrong image. The slug normalizes ":"/"/" and other characters to "-", so
   * two distinct images can map to one default pool (for example "python:3.11"
   * and "python-3.11"). Reads the referenced SandboxTemplate's spec.image and
   * compares it to the requested image; a mismatch throws rather than silently
   * running the first caller's image.
   */
  private async verifyPoolImage(
    pool: CustomObject,
    name: string,
    image: string,
  ): Promise<void> {
    const templateRef = (pool.spec?.["templateRef"] ?? {}) as { name?: string };
    const templateName = templateRef.name ?? name;
    const remediation = `Pass { pool: "${name}" } explicitly to reuse this pool, or use a distinct image that maps to a different default pool.`;
    let template: CustomObject;
    try {
      template = await this.k8s.getTemplate(this.namespace, templateName);
    } catch (e) {
      // Pool with no resolvable template: cannot prove the image, so fail
      // closed rather than risk the wrong image.
      throw new AgentRunError(
        `default pool ${name} references template ${templateName} that could not be read`,
        {
          code: "pool_image_mismatch",
          cause: `reading SandboxTemplate ${templateName} failed with status ${statusOf(e) ?? "unknown"}`,
          remediation,
        },
      );
    }
    const existingImage = (template.spec?.["image"] as string | undefined);
    if (existingImage !== image) {
      throw new AgentRunError(
        `default pool ${name} already exists for a different image`,
        {
          code: "pool_image_mismatch",
          cause: `pool ${name} runs image ${JSON.stringify(existingImage)}, not the requested ${JSON.stringify(image)} (the image slug collides)`,
          remediation,
        },
      );
    }
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
    if (opts?.workspace) {
      spec["workspaceRef"] = { name: opts.workspace };
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
      terminator: this.makeTerminator(name),
    });
  }

  /**
   * Builds the workspace-aware terminator for a claim: when terminate is called
   * with outputs or checkpoint, it merge-patches the claim spec first (the
   * controller dehydrates with those outputs on the way out), then reads the
   * claim's workspaceRef, deletes the claim, and returns the bound workspace
   * name (or undefined when the sandbox is unbound).
   */
  private makeTerminator(name: string): (opts?: TerminateOptions) => Promise<string | undefined> {
    return async (opts?: TerminateOptions) => {
      const specOutputs: Array<Record<string, unknown>> = [];
      for (const o of opts?.outputs ?? []) {
        if (typeof o === "string") {
          specOutputs.push({ path: o });
        } else {
          specOutputs.push(o);
        }
      }
      const patchSpec: Record<string, unknown> = {};
      if (specOutputs.length > 0) {
        patchSpec["outputs"] = specOutputs;
      }
      if (opts?.checkpoint) {
        patchSpec["checkpointOnTerminate"] = true;
      }
      if (Object.keys(patchSpec).length > 0) {
        await this.k8s.patchClaim(this.namespace, name, { spec: patchSpec });
      }
      let workspaceRef: string | undefined;
      try {
        const claim = await this.k8s.getClaim(this.namespace, name);
        const ref = ((claim.spec ?? {})["workspaceRef"] ?? {}) as { name?: string };
        workspaceRef = ref.name;
      } catch {
        // Claim already gone; report unbound.
      }
      await this.k8s.deleteClaim(this.namespace, name);
      return workspaceRef;
    };
  }

  /** Create an empty durable Workspace. */
  async createWorkspace(name: string): Promise<Workspace> {
    const body: CustomObject = {
      apiVersion: `${API_GROUP}/${API_VERSION}`,
      kind: "Workspace",
      metadata: { name, namespace: this.namespace },
      spec: {},
    };
    await this.k8s.createWorkspace(this.namespace, body);
    return new Workspace(name, this.namespace, this.k8s);
  }

  /** Lazy handle to a workspace (does not touch the cluster). */
  workspace(name: string): Workspace {
    return new Workspace(name, this.namespace, this.k8s);
  }

  /** Reconnect to an existing workspace, throwing if it is absent. */
  async getWorkspace(name: string): Promise<Workspace> {
    try {
      await this.k8s.getWorkspace(this.namespace, name);
    } catch (e) {
      throw new AgentRunError(`workspace ${name} not found`, {
        code: "workspace_not_found",
        cause: `getting Workspace ${name} failed with status ${statusOf(e) ?? "unknown"}`,
        remediation: "Create it with createWorkspace(name) first.",
      });
    }
    return new Workspace(name, this.namespace, this.k8s);
  }

  /** List the workspaces in the client's namespace. */
  async listWorkspaces(): Promise<Workspace[]> {
    const list = await this.k8s.listWorkspaces(this.namespace);
    return (list.items ?? []).map(
      (o) => new Workspace(o.metadata?.name ?? "", this.namespace, this.k8s),
    );
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
