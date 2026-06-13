// Public types for the mitos TypeScript SDK. These mirror the Python SDK
// (sdk/python/mitos/types.py) with camelCased field names.

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

/** One rich display artifact from runCode (mirrors E2B's Result). data maps a
 * MIME type to its payload (base64 for image/png, raw text otherwise). */
export interface Result {
  data: Record<string, string>;
  isMainResult: boolean;
}

/** A structured exception from runCode (mirrors E2B's error). */
export interface ExecutionError {
  name: string;
  value: string;
  traceback: string[];
}

/** The full result of a runCode call (mirrors E2B's Execution). text is the
 * REPL last-expression value; logs holds buffered stdout/stderr; results are
 * the rich artifacts in order; error is the structured exception or null. */
export interface Execution {
  text: string | null;
  logs: { stdout: string[]; stderr: string[] };
  results: Result[];
  error: ExecutionError | null;
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

/**
 * A handle to a streaming exec started in the background. `wait()` drains the
 * stream to completion and resolves the aggregate ExecResult; `kill()` aborts
 * the underlying HTTP stream, which forkd turns into a guest process-group
 * kill.
 */
export interface BackgroundProcess {
  wait(): Promise<ExecResult>;
  kill(): void;
}
