import { describe, expect, it } from "vitest";

import { AgentRun, defaultPoolName } from "../src/client.js";
import { AgentRunError } from "../src/errors.js";
import type { CustomObject, CustomObjectList, K8sApi } from "../src/k8s.js";

// FakeK8s is a scriptable K8sApi so the cluster logic is tested without a live
// cluster. getClaim returns a queued sequence of statuses; readSecret returns a
// configured map; createClaim/deleteClaim record their inputs.
// notFound builds an error carrying a 404 statusCode, matching how the real
// KubeConfigApi surfaces a missing object so the client can tell absent from a
// real failure.
function notFound(): Error {
  const e = new Error("not found") as Error & { statusCode: number };
  e.statusCode = 404;
  return e;
}

class FakeK8s implements K8sApi {
  createdClaims: CustomObject[] = [];
  deletedClaims: string[] = [];
  createdPools: CustomObject[] = [];
  createdTemplates: CustomObject[] = [];
  getPoolCalls = 0;
  getCalls = 0;

  constructor(
    private opts: {
      getResponses: Array<Record<string, unknown>>;
      secret?: Record<string, string>;
      secretThrows?: boolean;
      listItems?: CustomObject[];
      poolExists?: boolean;
      // Image stored on the existing default pool's SandboxTemplate. When set
      // (with poolExists), getPool returns a pool whose templateRef resolves to
      // a SandboxTemplate carrying this image, so the reuse path can verify it.
      existingPoolImage?: string;
      // When set, getTemplate rejects with this status code (template missing).
      templateThrowsStatus?: number;
    },
  ) {}

  async getPool(_ns: string, name: string): Promise<CustomObject> {
    this.getPoolCalls += 1;
    if (this.opts.poolExists) {
      return {
        metadata: { name },
        spec: { templateRef: { name } },
      };
    }
    throw notFound();
  }

  async createPool(_ns: string, pool: CustomObject): Promise<void> {
    this.createdPools.push(pool);
  }

  async createTemplate(_ns: string, template: CustomObject): Promise<void> {
    this.createdTemplates.push(template);
  }

  async getTemplate(_ns: string, name: string): Promise<CustomObject> {
    if (this.opts.templateThrowsStatus !== undefined) {
      const e = new Error("template not found") as Error & { statusCode: number };
      e.statusCode = this.opts.templateThrowsStatus;
      throw e;
    }
    return { metadata: { name }, spec: { image: this.opts.existingPoolImage } };
  }

  async createClaim(_ns: string, claim: CustomObject): Promise<CustomObject> {
    this.createdClaims.push(claim);
    return claim;
  }

  async getClaim(_ns: string, name: string): Promise<CustomObject> {
    const idx = Math.min(this.getCalls, this.opts.getResponses.length - 1);
    this.getCalls += 1;
    const status = this.opts.getResponses[idx] ?? {};
    return { metadata: { name }, status };
  }

  async deleteClaim(_ns: string, name: string): Promise<void> {
    this.deletedClaims.push(name);
  }

  async listClaims(_ns: string): Promise<CustomObjectList> {
    return { items: this.opts.listItems ?? [] };
  }

  async readSecret(_ns: string, _name: string): Promise<Record<string, string>> {
    if (this.opts.secretThrows) {
      throw new Error("secret not found");
    }
    return this.opts.secret ?? {};
  }
}

const noSleep = async () => {};

describe("defaultPoolName", () => {
  it("slugifies an image deterministically", () => {
    expect(defaultPoolName("python")).toBe("mitos-default-python");
    expect(defaultPoolName("python:3.12-slim")).toBe("mitos-default-python-3.12-slim");
    expect(defaultPoolName("Python")).toBe("mitos-default-python"); // lowercased
  });

  it("strips a trailing '.' so the name stays a valid object name", () => {
    expect(defaultPoolName("python.")).toBe("mitos-default-python");
    expect(defaultPoolName("python-")).toBe("mitos-default-python");
  });

  it("bounds the slug to 40 chars after the prefix", () => {
    const long = defaultPoolName(
      "ghcr.io/paperclipinc/agent-python-with-a-very-long-tag:3.12",
    );
    expect(long.startsWith("mitos-default-")).toBe(true);
    expect(long.slice("mitos-default-".length).length).toBeLessThanOrEqual(40);
  });
});

describe("AgentRun construction", () => {
  it("throws a clear error when no K8sApi is provided", () => {
    expect(() => new AgentRun()).toThrow(AgentRunError);
  });
});

describe("AgentRun.create", () => {
  it("builds the right claim spec, polls to Ready, reads the token, and binds the Sandbox", async () => {
    const fake = new FakeK8s({
      getResponses: [
        { phase: "Pending" },
        { phase: "Restoring" },
        { phase: "Ready", endpoint: "10.0.0.5:9091", sandboxID: "sbx-abc" },
      ],
      secret: { token: "tok-cluster-secret", endpoint: "10.0.0.5:9091" },
    });
    const run = new AgentRun({ k8s: fake, namespace: "team-a", sleep: noSleep });

    const sandbox = await run.create("python-pool", {
      name: "sbx-1",
      env: { FOO: "bar" },
      timeout: "30m",
    });

    // Claim spec is correct.
    expect(fake.createdClaims).toHaveLength(1);
    const claim = fake.createdClaims[0];
    expect(claim.apiVersion).toBe("mitos.run/v1alpha1");
    expect(claim.kind).toBe("SandboxClaim");
    expect(claim.metadata).toEqual({ name: "sbx-1", namespace: "team-a" });
    expect(claim.spec).toEqual({
      poolRef: { name: "python-pool" },
      env: [{ name: "FOO", value: "bar" }],
      timeout: "30m",
    });

    // Polled until Ready.
    expect(fake.getCalls).toBe(3);

    // Sandbox carries the endpoint and token.
    expect(sandbox.id).toBe("sbx-1");
    expect(sandbox.endpoint).toBe("http://10.0.0.5:9091");
  });

  it("times out with a clear error when the claim never becomes Ready", async () => {
    const fake = new FakeK8s({ getResponses: [{ phase: "Pending" }] });
    const run = new AgentRun({ k8s: fake, pollTimeoutMs: 5, sleep: noSleep });

    let caught: AgentRunError | undefined;
    try {
      await run.create("python-pool");
    } catch (e) {
      caught = e as AgentRunError;
    }
    expect(caught).toBeInstanceOf(AgentRunError);
    expect(caught!.code).toBe("ready_timeout");
    expect(caught!.message).toContain("not ready");
  });

  it("never surfaces the token in a thrown error", async () => {
    const token = "ultra-secret-bearer";
    // Claim goes Ready, secret is read, but a later exec fails with the token
    // echoed back. The token must not appear in the thrown error.
    const fake = new FakeK8s({
      getResponses: [{ phase: "Ready", endpoint: "127.0.0.1:1/will-refuse" }],
      secret: { token },
    });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });
    const sandbox = await run.create("p");

    let caught: unknown;
    try {
      // Endpoint is unroutable, so exec rejects; assert the token is absent.
      await sandbox.exec("noop");
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeTruthy();
    expect(JSON.stringify(caught)).not.toContain(token);
    expect(String((caught as Error).message)).not.toContain(token);
  });

  it("tolerates a missing token Secret (stays tokenless)", async () => {
    const fake = new FakeK8s({
      getResponses: [{ phase: "Ready", endpoint: "10.0.0.9:9091" }],
      secretThrows: true,
    });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });
    const sandbox = await run.create("p");
    expect(sandbox.endpoint).toBe("http://10.0.0.9:9091");
  });

  it("fails fast when the claim reaches Failed", async () => {
    const fake = new FakeK8s({ getResponses: [{ phase: "Failed" }] });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });
    await expect(run.create("p")).rejects.toMatchObject({ code: "sandbox_failed" });
  });

  it("terminate deletes the claim", async () => {
    const fake = new FakeK8s({
      getResponses: [{ phase: "Ready", endpoint: "10.0.0.9:9091" }],
      secret: { token: "t" },
    });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });
    const sandbox = await run.create("p", { name: "sbx-del" });
    await sandbox.terminate();
    expect(fake.deletedClaims).toEqual(["sbx-del"]);
  });
});

describe("AgentRun.list", () => {
  it("maps claims to SandboxInfo and filters by pool", async () => {
    const items: CustomObject[] = [
      {
        metadata: { name: "a" },
        spec: { poolRef: { name: "p1" } },
        status: {
          phase: "Ready",
          endpoint: "10.0.0.1:9091",
          node: "n1",
          sandboxID: "sbx-a",
          forkTimeMicros: 2500,
        },
      },
      {
        metadata: { name: "b" },
        spec: { poolRef: { name: "p2" } },
        status: { phase: "Pending" },
      },
    ];
    const fake = new FakeK8s({ getResponses: [], listItems: items });
    const run = new AgentRun({ k8s: fake, sleep: noSleep });

    const all = await run.list();
    expect(all).toHaveLength(2);
    expect(all[0]).toEqual({
      name: "a",
      phase: "Ready",
      endpoint: "10.0.0.1:9091",
      node: "n1",
      sandboxId: "sbx-a",
      forkTimeMs: 2.5,
      pool: "p1",
    });
    expect(all[1].phase).toBe("Pending");
    expect(all[1].pool).toBe("p2");

    const filtered = await run.list("p1");
    expect(filtered.map((x) => x.name)).toEqual(["a"]);
  });
});

describe("AgentRun.sandbox(image) lazy default pool", () => {
  const ready = [{ phase: "Ready", endpoint: "10.0.0.5:9091", sandboxID: "sbx" }];

  it("creates mitos-default-<image> when the pool is absent, then claims from it", async () => {
    const fake = new FakeK8s({ getResponses: ready, poolExists: false });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    const sb = await c.sandbox("python");
    expect(fake.createdTemplates).toHaveLength(1);
    expect(fake.createdTemplates[0].metadata?.name).toBe("mitos-default-python");
    expect(fake.createdTemplates[0].spec).toEqual({ image: "python" });
    expect(fake.createdPools).toHaveLength(1);
    expect(fake.createdPools[0].metadata?.name).toBe("mitos-default-python");
    expect(fake.createdPools[0].spec).toEqual({
      templateRef: { name: "mitos-default-python" },
      replicas: 1,
    });
    expect(fake.createdClaims[0].spec).toEqual({ poolRef: { name: "mitos-default-python" } });
    expect(sb.id).toMatch(/^sandbox-/);
  });

  it("reuses an existing default pool (no create)", async () => {
    const fake = new FakeK8s({
      getResponses: ready,
      poolExists: true,
      existingPoolImage: "python",
    });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await c.sandbox("python");
    expect(fake.createdPools).toHaveLength(0);
    expect(fake.createdTemplates).toHaveLength(0);
  });

  it("raises pool_image_mismatch when a colliding slug reuses a pool for a different image", async () => {
    // image A ("python-3.11") created the default pool; calling with image B
    // ("python:3.11") collides to the same slug mitos-default-python-3.11.
    expect(defaultPoolName("python:3.11")).toBe(defaultPoolName("python-3.11"));
    const fake = new FakeK8s({
      getResponses: ready,
      poolExists: true,
      existingPoolImage: "python-3.11",
    });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await expect(c.sandbox("python:3.11")).rejects.toMatchObject({
      code: "pool_image_mismatch",
    });
    expect(fake.createdClaims).toHaveLength(0); // no sandbox was created
  });

  it("fails closed when the reused pool's template cannot be read", async () => {
    const fake = new FakeK8s({
      getResponses: ready,
      poolExists: true,
      templateThrowsStatus: 404,
    });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await expect(c.sandbox("python")).rejects.toMatchObject({
      code: "pool_image_mismatch",
    });
  });

  it("explicit pool never creates a pool", async () => {
    const fake = new FakeK8s({ getResponses: ready, poolExists: false });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await c.sandbox("python", { pool: "my-pool" });
    expect(fake.getPoolCalls).toBe(0);
    expect(fake.createdPools).toHaveLength(0);
    expect(fake.createdClaims[0].spec).toEqual({ poolRef: { name: "my-pool" } });
  });

  it("fromName reconnects a Ready sandbox", async () => {
    const fake = new FakeK8s({
      getResponses: [{ phase: "Ready", endpoint: "10.0.0.9:8443", sandboxID: "sbx" }],
      secret: { token: "tok" },
    });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    const sb = await c.fromName("agent-session-1");
    expect(sb.id).toBe("agent-session-1");
    expect(sb.endpoint).toContain("10.0.0.9:8443");
  });

  it("opt-out raises without a pool", async () => {
    const fake = new FakeK8s({ getResponses: ready, poolExists: false });
    const c = new AgentRun({ k8s: fake, allowDefaultPool: false, sleep: noSleep });
    await expect(c.sandbox("python")).rejects.toMatchObject({ code: "no_default_pool" });
  });

  it("requires an image or a pool", async () => {
    const fake = new FakeK8s({ getResponses: ready });
    const c = new AgentRun({ k8s: fake, sleep: noSleep });
    await expect(c.sandbox()).rejects.toMatchObject({ code: "missing_image_or_pool" });
  });
});
