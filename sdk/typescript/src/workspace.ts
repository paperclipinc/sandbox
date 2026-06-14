// A durable, forkable agent workspace handle for the cluster client. Mirrors
// the Python mitos.workspace.Workspace: lazy (no cluster touch until a verb is
// called) and git-shaped (log, diff, fork, revert/checkout). Errors are
// LLM-legible AgentRunErrors carrying a stable code and a remediation.

import { AgentRunError } from "./errors.js";
import type { CustomObject, K8sApi } from "./k8s.js";

const API_GROUP = "mitos.run";
const API_VERSION = "v1alpha1";

export interface RevisionInfo {
  name: string;
  phase: string;
  lineage: string;
  resumable: boolean;
  created: string;
}

export interface DiffInfo {
  parent: string;
  added: string[];
  removed: string[];
  modified: string[];
}

function lineageOf(spec: Record<string, unknown> | undefined): string {
  const src = (spec?.["source"] ?? {}) as Record<string, unknown>;
  const fromClaim = src["fromClaim"];
  if (typeof fromClaim === "string" && fromClaim !== "") {
    return "fromClaim:" + fromClaim;
  }
  const fwr = src["fromWorkspaceRevision"] as { revision?: string } | undefined;
  if (fwr) {
    return "fromWorkspaceRevision:" + (fwr.revision ?? "");
  }
  return "root";
}

/**
 * A durable, forkable agent workspace handle. Construct via AgentRun.workspace,
 * createWorkspace, or getWorkspace.
 */
export class Workspace {
  readonly name: string;

  private readonly namespace: string;
  private readonly k8s: K8sApi;

  constructor(name: string, namespace: string, k8s: K8sApi) {
    this.name = name;
    this.namespace = namespace;
    this.k8s = k8s;
  }

  /** The latest committed revision name, or "" until the first revision commits. */
  async head(): Promise<string> {
    const ws = await this.k8s.getWorkspace(this.namespace, this.name);
    return ((ws.status ?? {})["head"] as string) ?? "";
  }

  /** Whether the workspace head pairs with a memory snapshot (resumable). */
  async resumable(): Promise<boolean> {
    const ws = await this.k8s.getWorkspace(this.namespace, this.name);
    return Boolean((ws.status ?? {})["resumable"]);
  }

  /** Lists the workspace's revisions, newest first. */
  async log(): Promise<RevisionInfo[]> {
    const list = await this.k8s.listRevisions(this.namespace);
    const revs: RevisionInfo[] = [];
    for (const o of list.items ?? []) {
      const spec = o.spec ?? {};
      const ref = (spec["workspaceRef"] ?? {}) as { name?: string };
      if (ref.name !== this.name) {
        continue;
      }
      revs.push({
        name: o.metadata?.name ?? "",
        phase: ((o.status ?? {})["phase"] as string) ?? "",
        lineage: lineageOf(spec),
        resumable: spec["memorySnapshotRef"] != null,
        created: o.metadata?.creationTimestamp ?? "",
      });
    }
    revs.sort((a, b) => (a.created < b.created ? 1 : a.created > b.created ? -1 : 0));
    return revs;
  }

  /** Returns the recorded content-hash diff of a revision against its parent. */
  async diff(revision: string): Promise<DiffInfo> {
    const o = await this.k8s.getRevision(this.namespace, revision);
    const summary = (o.status ?? {})["diffSummary"] as
      | { parentRevision?: string; added?: string[]; removed?: string[]; modified?: string[] }
      | undefined;
    if (!summary) {
      throw new AgentRunError(`revision ${revision} has no recorded diff`, {
        code: "no_diff",
        cause: "the revision was not captured with a {diff: true} output",
        remediation: "Terminate with outputs: [{ diff: true }] to record a diff.",
      });
    }
    return {
      parent: summary.parentRevision ?? "",
      added: summary.added ?? [],
      removed: summary.removed ?? [],
      modified: summary.modified ?? [],
    };
  }

  /**
   * Branch a committed revision into dstWorkspace (a content-addressed branch).
   * Returns the new revision name. dstWorkspace must exist.
   */
  async fork(revision: string, dstWorkspace: string): Promise<string> {
    const parent = await this.k8s.getRevision(this.namespace, revision);
    const manifest = (parent.spec ?? {})["contentManifest"] as string | undefined;
    const phase = (parent.status ?? {})["phase"] as string | undefined;
    if (phase !== "Committed" || !manifest) {
      throw new AgentRunError(`cannot fork uncommitted revision ${revision}`, {
        code: "revision_not_committed",
        cause: `revision ${revision} is not committed`,
        remediation: "Wait for the revision to commit before forking it.",
      });
    }
    const body: CustomObject = {
      apiVersion: `${API_GROUP}/${API_VERSION}`,
      kind: "WorkspaceRevision",
      metadata: {
        name: undefined,
        namespace: this.namespace,
      },
      spec: {
        workspaceRef: { name: dstWorkspace },
        source: { fromWorkspaceRevision: { workspace: this.name, revision } },
        contentManifest: manifest,
      },
    };
    // generateName + labels live alongside name; set them without losing the
    // typed metadata shape.
    (body.metadata as Record<string, unknown>)["generateName"] = dstWorkspace + "-";
    (body.metadata as Record<string, unknown>)["labels"] = {
      "mitos.run/workspace": dstWorkspace,
    };
    const created = await this.k8s.createRevision(this.namespace, body);
    return created.metadata?.name ?? "";
  }

  /**
   * Set this workspace head to a past revision by creating a new tip that
   * shares its content (revisions are immutable; a revert is a new tip).
   */
  async revert(revision: string): Promise<string> {
    return this.fork(revision, this.name);
  }

  /** checkout is an alias for revert: make a past state the new head. */
  async checkout(revision: string): Promise<string> {
    return this.revert(revision);
  }
}
