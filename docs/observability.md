# Observability

This document describes the three observability signals the control plane
exports today (distributed traces, a structured audit log, and Prometheus
metrics) and the `kubectl sandbox` plugin for operators. Each section marks what
is PROVEN in CI versus what remains OPEN.

The governing rule is the secret-safety rule from CLAUDE.md: no signal ever
carries secret values, file content, env values, or bearer tokens. Traces carry
ids, counts, and timings; the audit log carries command strings, paths, and
byte counts; metric labels are pool names and fixed reason codes.

## Distributed tracing

### The trace model

A claim's life produces one trace spanning two processes:

```
controller.reconcileClaim         (controller)
  controller.forkOnNode           (controller)
    forkd.Fork                     (forkd, via gRPC)
      engine.fork                  (forkd, KVM snapshot restore)
```

- `controller.reconcileClaim` opens when the SandboxClaim reconciler runs.
  Attributes: `claim.name`, `claim.namespace`, and `pool` (the pool the claim
  resolves to).
- `controller.forkOnNode` covers node selection plus the gRPC call to the chosen
  forkd. Attributes: `node` (the selected node), `snapshot` (the snapshot id).
- `forkd.Fork` is the server span on the forkd side of the gRPC call.
  Attributes: `snapshot.id`, `sandbox.id`, and `fork_time_ms` once the fork
  completes.
- `engine.fork` covers the KVM snapshot restore inside forkd. Attributes:
  `snapshot.id`, `sandbox.id`, `fork_time_ms`.

### Cross-process propagation

The controller and forkd are distinct processes. The trace id crosses the gRPC
boundary by W3C trace-context propagation installed on the gRPC client and
server stats handlers (`observability.GRPCClientStatsHandler` and
`observability.GRPCServerStatsHandler`). The `forkd.Fork` span is therefore a
child of `controller.forkOnNode` under the same trace id, so a single trace
covers the controller's decision through the KVM restore.

### Enabling it

Tracing is off by default and zero-cost when off (no exporter is installed).
Enable it by pointing both processes at an OTLP gRPC collector:

```
controller --otlp-endpoint=otel-collector:4317
forkd      --otlp-endpoint=otel-collector:4317
```

The endpoint is a `host:port` for an OTLP gRPC receiver. An empty value disables
tracing.

### Secret safety

Spans carry only ids (claim name/namespace, pool, node, snapshot id, sandbox
id), counts, and timings. No span attribute carries a secret value, env value,
file content, or token.

### PROVEN

- The span tree and attributes above are asserted with an in-memory span
  recorder in CI (`internal/observability`, plus the controller and daemon
  span tests).
- Cross-process trace-id propagation over gRPC is asserted: a server span shares
  the client span's trace id.

### OPEN

- The guest-telemetry vsock bridge (cpu steal, balloon pressure, in-guest
  process table) is not yet wired; see issue #29.
- A single trace id stamped across the pod, its logs, Hubble flows, and
  Workspace revisions needs husk pods (#18) and the Workspace (#21).

## Audit log

### What it is

forkd can emit one structured JSON record per exec or file operation served by
its sandbox API. Each record is one line of JSON:

```json
{
  "sandbox_id": "sbx-abc123",
  "op": "exec",
  "detail": "python train.py",
  "bytes": 0,
  "unix": 1749643200,
  "ok": true
}
```

Fields:

- `sandbox_id`: the sandbox the operation ran against.
- `op`: the operation kind (for example `exec`, file read, file write).
- `detail`: a safe human summary. For exec it is the command string, truncated
  to 256 runes with an explicit truncation note. For file ops it is the path.
  It never contains file content or secret values.
- `bytes`: the byte COUNT of file content read or written. The content itself
  is never recorded.
- `unix`: the event time in Unix seconds.
- `ok`: whether the handler served the operation without error. For exec a
  non-zero exit code is still `ok: true` (the call succeeded); the exit code is
  reported in `detail`.

### What is logged versus never logged

Logged: sandbox id, operation, the command string or file path, and the byte
count.

Never logged: file content, env values, secret values, and bearer tokens.
Commands are not secret values, so the command string is recorded; the 256-rune
bound only keeps records small.

### Enabling it

Auditing is off by default. Enable it on forkd:

```
forkd --audit-log=/var/log/agentrun/audit.jsonl   # append to a file
forkd --audit-log=-                                # or "stderr": write to stderr
```

An empty value disables auditing. File paths are opened append-only (mode
`0o600`). Audit writes never break the request path: an encoding error drops the
record rather than failing the operation.

### PROVEN

Content-safety is asserted in CI: the audit tests confirm records carry the
command/path and byte count but never the file content or secret values, and
that exec commands are truncated.

## Metrics

All metrics are Prometheus and exposed at `/metrics`. Node-level fork metrics
live on forkd's default registry; controller-level claim and pool metrics
register with controller-runtime's registry on the controller's `/metrics`
endpoint. No metric carries a secret value; labels are pool names and fixed
reason codes only.

### Node-level (forkd)

| Metric | Type | Meaning |
|--------|------|---------|
| `agentrun_fork_duration_seconds` | histogram | Time to fork a sandbox from snapshot, as measured by forkd. |
| `agentrun_active_sandboxes` | gauge | Currently running sandboxes on this node. |
| `agentrun_memory_shared_bytes` | gauge | Copy-on-write shared memory across forks. |
| `agentrun_memory_unique_bytes` | gauge | Per-fork unique (dirty-page) memory at fork time. |

### Controller-level

| Metric | Type | Meaning |
|--------|------|---------|
| `agentrun_claim_pending_total` | counter | Times a claim was requeued because no node had a ready snapshot (the claim stayed Pending). |
| `agentrun_orphan_sweeps_total` | counter | Orphan sandbox VMs terminated by the garbage collector. |
| `agentrun_claim_errors_total{pool,reason}` | counter | Claims that failed terminally, by pool and coarse reason (`fork`, `secret`, `volume`, `token`). |
| `agentrun_pool_ready_snapshots{pool}` | gauge | Ready snapshots per pool, as of the last pool reconcile. |

Counter versus gauge for pending: `agentrun_claim_pending_total` is a counter of
pending-requeue EVENTS, not a live gauge of currently-pending claims. A counter
is exact and lock-free to bump at the requeue site; an honest live gauge of
currently-pending claims would need a periodic recount with its own staleness
window. The counter directly answers "how often are claims failing to place".

### PROVEN

The controller metric increments are asserted in CI (the increments fire on the
pending-requeue, orphan-sweep, claim-error, and pool-reconcile paths).

### OPEN

- Snapshot-distribution lag is not yet exported.
- Grafana dashboards, PrometheusRule alerts with runbook URLs, and a
  conditions/reason-code catalogue are a 1.0 maturity item (#29).
- OpenCost and Hubble layers ride on husk pods (#18).

## `kubectl sandbox` plugin

The `kubectl-sandbox` binary is a kubectl plugin that lists agentrun.dev sandbox
objects. It resolves the cluster connection from the standard kubeconfig
resolution (`KUBECONFIG`, `--kubeconfig`, or in-cluster).

### Subcommands

```
kubectl sandbox ls [-n namespace] [-A]          list SandboxClaims
kubectl sandbox ps [name] [-n namespace] [-A]   list SandboxForks, or one claim's forks
```

- `ls` prints SandboxClaims with columns NAME, POOL, PHASE, NODE, ENDPOINT, AGE.
- `ps` prints SandboxForks with columns NAME, SOURCE, READY, AGE. Given a claim
  name, it filters to forks whose source is that claim.
- `-n` scopes to a namespace (default `default`); `-A` lists all namespaces.

Ages render kubectl-style (`30s`, `2m`, `3h`, `5d`); missing node, endpoint, or
source cells render as `-`.

### Installing it

kubectl discovers plugins named `kubectl-<name>` on PATH. Build the binary and
put it on PATH as `kubectl-sandbox`:

```
go build -o kubectl-sandbox ./cmd/kubectl-sandbox/
mv kubectl-sandbox /usr/local/bin/        # any directory on PATH
kubectl sandbox ls
```

### PROVEN

The table formatting is asserted in CI: columns, values, kubectl-style age
strings, empty-list messages, and missing-cell dashes
(`internal/cli/sandboxtable`).

### OPEN

`kubectl sandbox top/tree/exec/logs` are documented follow-ups (#29); invoking
them prints a "not yet implemented" notice.
