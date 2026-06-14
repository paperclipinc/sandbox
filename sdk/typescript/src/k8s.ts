// Kubernetes access for the cluster client. The K8sApi interface is the thin
// seam the AgentRun cluster client talks to, so the cluster logic is unit
// testable with a fake (see test/client.test.ts). The real implementation,
// KubeConfigApi, wraps @kubernetes/client-node.
//
// Direct mode (SandboxServer) does NOT need @kubernetes/client-node: it is only
// imported lazily inside KubeConfigApi, so a direct-mode consumer never pulls
// the k8s client into its bundle.

const API_GROUP = "mitos.run";
const API_VERSION = "v1alpha1";

/** A Kubernetes custom object as a plain JSON shape. */
export interface CustomObject {
  apiVersion?: string;
  kind?: string;
  metadata?: { name?: string; namespace?: string; creationTimestamp?: string };
  spec?: Record<string, unknown>;
  status?: Record<string, unknown>;
}

/** A list of custom objects. */
export interface CustomObjectList {
  items: CustomObject[];
}

/**
 * The minimal Kubernetes surface the cluster client needs. Operates on
 * SandboxClaim custom objects (plural "sandboxclaims") and core Secrets.
 * Implemented for real by KubeConfigApi and by a fake in tests.
 */
export interface K8sApi {
  createClaim(namespace: string, claim: CustomObject): Promise<CustomObject>;
  getClaim(namespace: string, name: string): Promise<CustomObject>;
  deleteClaim(namespace: string, name: string): Promise<void>;
  listClaims(namespace: string): Promise<CustomObjectList>;
  /**
   * Gets a SandboxPool. Rejects with an error carrying statusCode 404 when the
   * pool is absent, so the lazy-default-pool path can tell absent from a real
   * failure.
   */
  getPool(namespace: string, name: string): Promise<CustomObject>;
  /** Creates a SandboxPool. */
  createPool(namespace: string, pool: CustomObject): Promise<void>;
  /** Creates a SandboxTemplate (the pool's templateRef target). */
  createTemplate(namespace: string, template: CustomObject): Promise<void>;
  /**
   * Gets a SandboxTemplate. Used by the default-pool reuse path to confirm a
   * reused pool runs the requested image (guarding against a slug collision).
   */
  getTemplate(namespace: string, name: string): Promise<CustomObject>;
  /**
   * Reads a Secret and returns its data as decoded UTF-8 strings keyed by the
   * Secret key. Values are held in memory only and must never be logged.
   */
  readSecret(namespace: string, name: string): Promise<Record<string, string>>;

  // --- Workspace verbs (mitos.run/v1alpha1 Workspace and WorkspaceRevision) ---

  /** Creates a Workspace custom object. */
  createWorkspace(namespace: string, workspace: CustomObject): Promise<CustomObject>;
  /** Gets a Workspace custom object. */
  getWorkspace(namespace: string, name: string): Promise<CustomObject>;
  /** Lists Workspace custom objects. */
  listWorkspaces(namespace: string): Promise<CustomObjectList>;
  /** Deletes a Workspace; its revisions are garbage-collected by owner ref. */
  deleteWorkspace(namespace: string, name: string): Promise<void>;
  /** Lists WorkspaceRevision custom objects. */
  listRevisions(namespace: string): Promise<CustomObjectList>;
  /** Gets a WorkspaceRevision custom object. */
  getRevision(namespace: string, name: string): Promise<CustomObject>;
  /** Creates a WorkspaceRevision custom object (the fork/revert tip). */
  createRevision(namespace: string, revision: CustomObject): Promise<CustomObject>;
  /** Merge-patches a SandboxClaim's spec (used by bind and terminate outputs). */
  patchClaim(namespace: string, name: string, patch: Record<string, unknown>): Promise<void>;
}

/**
 * Real K8sApi over @kubernetes/client-node. Kept deliberately thin and is not
 * unit tested (the live k8s calls are covered by integration, not here). The
 * client library is imported lazily so direct mode never needs it installed at
 * import time.
 */
export class KubeConfigApi implements K8sApi {
  private customApi: any;
  private coreApi: any;
  private ready: Promise<void>;

  constructor(opts?: { kubeconfig?: string; inCluster?: boolean }) {
    this.ready = this.init(opts);
  }

  private async init(opts?: { kubeconfig?: string; inCluster?: boolean }): Promise<void> {
    const k8s = await import("@kubernetes/client-node");
    const kc = new k8s.KubeConfig();
    if (opts?.inCluster) {
      kc.loadFromCluster();
    } else if (opts?.kubeconfig) {
      kc.loadFromFile(opts.kubeconfig);
    } else {
      kc.loadFromDefault();
    }
    this.customApi = kc.makeApiClient(k8s.CustomObjectsApi);
    this.coreApi = kc.makeApiClient(k8s.CoreV1Api);
  }

  async createClaim(namespace: string, claim: CustomObject): Promise<CustomObject> {
    await this.ready;
    const res = await this.customApi.createNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "sandboxclaims",
      body: claim,
    });
    return res as CustomObject;
  }

  async getClaim(namespace: string, name: string): Promise<CustomObject> {
    await this.ready;
    const res = await this.customApi.getNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "sandboxclaims",
      name,
    });
    return res as CustomObject;
  }

  async deleteClaim(namespace: string, name: string): Promise<void> {
    await this.ready;
    await this.customApi.deleteNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "sandboxclaims",
      name,
    });
  }

  async listClaims(namespace: string): Promise<CustomObjectList> {
    await this.ready;
    const res = await this.customApi.listNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "sandboxclaims",
    });
    return res as CustomObjectList;
  }

  async getPool(namespace: string, name: string): Promise<CustomObject> {
    await this.ready;
    const res = await this.customApi.getNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "sandboxpools",
      name,
    });
    return res as CustomObject;
  }

  async createPool(namespace: string, pool: CustomObject): Promise<void> {
    await this.ready;
    await this.customApi.createNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "sandboxpools",
      body: pool,
    });
  }

  async createTemplate(namespace: string, template: CustomObject): Promise<void> {
    await this.ready;
    await this.customApi.createNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "sandboxtemplates",
      body: template,
    });
  }

  async getTemplate(namespace: string, name: string): Promise<CustomObject> {
    await this.ready;
    const res = await this.customApi.getNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "sandboxtemplates",
      name,
    });
    return res as CustomObject;
  }

  async readSecret(
    namespace: string,
    name: string,
  ): Promise<Record<string, string>> {
    await this.ready;
    const res = await this.coreApi.readNamespacedSecret({ name, namespace });
    const data: Record<string, string> = {};
    const raw = (res?.data ?? {}) as Record<string, string>;
    for (const [k, v] of Object.entries(raw)) {
      // Secret values are base64-encoded; decode without logging the value.
      data[k] = Buffer.from(v, "base64").toString("utf-8");
    }
    return data;
  }

  async createWorkspace(namespace: string, workspace: CustomObject): Promise<CustomObject> {
    await this.ready;
    const res = await this.customApi.createNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "workspaces",
      body: workspace,
    });
    return res as CustomObject;
  }

  async getWorkspace(namespace: string, name: string): Promise<CustomObject> {
    await this.ready;
    const res = await this.customApi.getNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "workspaces",
      name,
    });
    return res as CustomObject;
  }

  async listWorkspaces(namespace: string): Promise<CustomObjectList> {
    await this.ready;
    const res = await this.customApi.listNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "workspaces",
    });
    return res as CustomObjectList;
  }

  async deleteWorkspace(namespace: string, name: string): Promise<void> {
    await this.ready;
    await this.customApi.deleteNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "workspaces",
      name,
    });
  }

  async listRevisions(namespace: string): Promise<CustomObjectList> {
    await this.ready;
    const res = await this.customApi.listNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "workspacerevisions",
    });
    return res as CustomObjectList;
  }

  async getRevision(namespace: string, name: string): Promise<CustomObject> {
    await this.ready;
    const res = await this.customApi.getNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "workspacerevisions",
      name,
    });
    return res as CustomObject;
  }

  async createRevision(namespace: string, revision: CustomObject): Promise<CustomObject> {
    await this.ready;
    const res = await this.customApi.createNamespacedCustomObject({
      group: API_GROUP,
      version: API_VERSION,
      namespace,
      plural: "workspacerevisions",
      body: revision,
    });
    return res as CustomObject;
  }

  async patchClaim(
    namespace: string,
    name: string,
    patch: Record<string, unknown>,
  ): Promise<void> {
    await this.ready;
    await this.customApi.patchNamespacedCustomObject(
      {
        group: API_GROUP,
        version: API_VERSION,
        namespace,
        plural: "sandboxclaims",
        name,
        body: patch,
      },
      // A merge patch so spec.outputs/checkpointOnTerminate/workspaceRef are set
      // without clobbering the rest of the claim spec.
      undefined,
      undefined,
      undefined,
      undefined,
      { headers: { "Content-Type": "application/merge-patch+json" } },
    );
  }
}
