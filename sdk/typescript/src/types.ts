// Public types for the agent-run TypeScript SDK. These mirror the Python SDK
// (sdk/python/agent_run/types.py) with camelCased field names.

/**
 * Fork policy for a volume: how the snapshot fork treats it.
 * Mirrors api/v1alpha1 ForkPolicy.
 */
export type ForkPolicy = "Fresh" | "Share" | "Snapshot" | "Clone";

/**
 * Lifecycle phase of a sandbox claim, mirroring the SandboxClaim CRD status.
 */
export type SandboxPhase =
  | "Pending"
  | "Restoring"
  | "Ready"
  | "Terminating"
  | "Terminated"
  | "Failed";

/**
 * Result of an exec call. Maps the sandbox API exec response
 * ({exit_code, stdout, stderr, exec_time_ms}) into camelCase.
 */
export interface ExecResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  execTimeMs?: number;
}

/**
 * A directory entry returned by files.list. Mirrors the guest agent file
 * listing shape ({name, is_dir, size, mode, modified_at}).
 */
export interface FileInfo {
  name: string;
  isDir: boolean;
  size: number;
  mode: number;
  modifiedAt?: string;
}

/**
 * Summary of a sandbox as observed from the cluster (a SandboxClaim) or the
 * standalone server.
 */
export interface SandboxInfo {
  name: string;
  phase: SandboxPhase;
  endpoint: string;
  node: string;
  sandboxId: string;
  forkTimeMs: number;
  pool: string;
}

/**
 * Status of a SandboxPool.
 */
export interface PoolStatus {
  name: string;
  readySnapshots: number;
  desired: number;
  nodeDistribution: Record<string, number>;
}

/**
 * Summary of a single fork produced by a SandboxFork.
 */
export interface ForkInfo {
  name: string;
  sandboxId: string;
  endpoint: string;
  node: string;
  phase: SandboxPhase;
  forkTimeMs: number;
}
