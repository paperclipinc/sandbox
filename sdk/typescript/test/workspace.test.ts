import { describe, it, expect } from "vitest";

import { AgentRun } from "../src/client.js";
import { AgentRunError } from "../src/errors.js";
import type { CustomObject, CustomObjectList, K8sApi } from "../src/k8s.js";

const noSleep = async () => {};

// FakeK8s implements the workspace-relevant slice of K8sApi. The sandbox verbs
// throw if hit (unused by these tests).
class FakeK8s implements K8sApi {
  createdWorkspaces: CustomObject[] = [];
  createdRevisions: CustomObject[] = [];
  patchedClaims: Array<{ name: string; patch: Record<string, unknown> }> = [];
  deletedClaims: string[] = [];

  constructor(
    private opts: {
      revisions?: CustomObject[];
      revision?: CustomObject;
      claim?: CustomObject;
    } = {},
  ) {}

  async createWorkspace(_ns: string, ws: CustomObject): Promise<CustomObject> {
    this.createdWorkspaces.push(ws);
    return ws;
  }
  async getWorkspace(_ns: string, name: string): Promise<CustomObject> {
    return { metadata: { name }, status: {} };
  }
  async listWorkspaces(_ns: string): Promise<CustomObjectList> {
    return { items: this.createdWorkspaces };
  }
  async deleteWorkspace(_ns: string, _name: string): Promise<void> {}
  async listRevisions(_ns: string): Promise<CustomObjectList> {
    return { items: this.opts.revisions ?? [] };
  }
  async getRevision(_ns: string, name: string): Promise<CustomObject> {
    return this.opts.revision ?? { metadata: { name } };
  }
  async createRevision(_ns: string, rev: CustomObject): Promise<CustomObject> {
    this.createdRevisions.push(rev);
    return { ...rev, metadata: { ...(rev.metadata ?? {}), name: "branch-generated" } };
  }
  async patchClaim(_ns: string, name: string, patch: Record<string, unknown>): Promise<void> {
    this.patchedClaims.push({ name, patch });
  }

  // Unused sandbox slice.
  async createClaim(): Promise<CustomObject> {
    throw new Error("unused");
  }
  async getClaim(_ns: string, name: string): Promise<CustomObject> {
    return this.opts.claim ?? { metadata: { name } };
  }
  async deleteClaim(_ns: string, name: string): Promise<void> {
    this.deletedClaims.push(name);
  }
  async listClaims(): Promise<CustomObjectList> {
    return { items: [] };
  }
  async getPool(): Promise<CustomObject> {
    throw new Error("unused");
  }
  async createPool(): Promise<void> {}
  async createTemplate(): Promise<void> {}
  async getTemplate(): Promise<CustomObject> {
    throw new Error("unused");
  }
  async readSecret(): Promise<Record<string, string>> {
    return {};
  }
}

describe("workspace", () => {
  it("create posts a Workspace CRD", async () => {
    const fake = new FakeK8s();
    const c = new AgentRun({ k8s: fake, namespace: "ns", sleep: noSleep });
    const ws = await c.createWorkspace("proj-x");
    expect(ws.name).toBe("proj-x");
    expect(fake.createdWorkspaces).toHaveLength(1);
    expect(fake.createdWorkspaces[0].kind).toBe("Workspace");
    expect(fake.createdWorkspaces[0].metadata?.name).toBe("proj-x");
  });

  it("log returns revisions newest first", async () => {
    const fake = new FakeK8s({
      revisions: [
        {
          metadata: { name: "proj-x-1", creationTimestamp: "2026-06-01T00:00:00Z" },
          spec: { workspaceRef: { name: "proj-x" }, source: { fromClaim: "c1" } },
          status: { phase: "Committed" },
        },
        {
          metadata: { name: "proj-x-2", creationTimestamp: "2026-06-02T00:00:00Z" },
          spec: { workspaceRef: { name: "proj-x" }, source: { fromClaim: "c2" } },
          status: { phase: "Committed" },
        },
      ],
    });
    const c = new AgentRun({ k8s: fake, namespace: "ns", sleep: noSleep });
    const revs = await c.workspace("proj-x").log();
    expect(revs.map((r) => r.name)).toEqual(["proj-x-2", "proj-x-1"]);
    expect(revs[0].lineage).toBe("fromClaim:c2");
  });

  it("fork of an uncommitted revision throws an LLM-legible error", async () => {
    const fake = new FakeK8s({
      revision: {
        metadata: { name: "proj-x-1" },
        spec: { workspaceRef: { name: "proj-x" } },
        status: { phase: "Pending" },
      },
    });
    const c = new AgentRun({ k8s: fake, namespace: "ns", sleep: noSleep });
    const ws = c.workspace("proj-x");
    await expect(ws.fork("proj-x-1", "branch")).rejects.toMatchObject({
      code: "revision_not_committed",
    });
  });

  it("fork of a committed revision creates a fromWorkspaceRevision edge", async () => {
    const fake = new FakeK8s({
      revision: {
        metadata: { name: "proj-x-1" },
        spec: { workspaceRef: { name: "proj-x" }, contentManifest: "deadbeef" },
        status: { phase: "Committed" },
      },
    });
    const c = new AgentRun({ k8s: fake, namespace: "ns", sleep: noSleep });
    const newRev = await c.workspace("proj-x").fork("proj-x-1", "branch");
    expect(newRev).toBe("branch-generated");
    const created = fake.createdRevisions[0];
    expect(created.spec?.["source"]).toEqual({
      fromWorkspaceRevision: { workspace: "proj-x", revision: "proj-x-1" },
    });
    expect(created.spec?.["contentManifest"]).toBe("deadbeef");
  });
});
