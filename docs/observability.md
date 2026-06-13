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

### Trace-to-revision link

When a claim terminates with a bound workspace, the dehydrate path captures the
sandbox `/workspace` into a new WorkspaceRevision. That capture is a child span
and the reconcile trace id is carried onto the revision, so a committed revision
resolves to the exact orchestrator request that produced it, and the reverse:

```
controller.reconcileClaim         (controller)
  workspace.dehydrate             (controller, on terminate)
```

- `workspace.dehydrate` opens when `dehydrateOnTerminate` runs, as a child of
  `controller.reconcileClaim` (same trace id). Attributes: `workspace.name`,
  `revision.name`, `content.manifest.digest` (the contentManifest digest, a
  content address), `captured.path.count`, and `memory.snapshot.paired` (a
  bool). Names, a digest, a count, and a bool only; no secret value.
- The active reconcile trace id is stamped on the new revision as the
  `mitos.run/trace-id` annotation BEFORE the revision is created, but only
  when tracing is enabled (the trace id is valid). With tracing off (the no-op
  provider) the annotation is omitted: a fake all-zero id is never written.
- The same trace id rides the `revision.created` feed CloudEvent as its
  `traceId` field (read from the annotation, empty when absent), so an external
  indexer correlates the revision event with the orchestrator trace without
  polling and without a secret. See the Audit/feed surface and `internal/eventfeed`.

This links the CONTROL-plane trace to the revision. The guest-side first-exec
and guest-ready spans (the in-VM telemetry tail) are the remaining piece; see
OPEN.

### Secret safety

Spans carry only ids (claim name/namespace, pool, node, snapshot id, sandbox
id, workspace and revision names, the contentManifest digest), counts, and
timings. The `mitos.run/trace-id` annotation and the feed `traceId` field
are opaque correlation ids, not secrets. No span attribute, annotation, or feed
field carries a secret value, env value, file content, or token.

### PROVEN

- The span tree and attributes above are asserted with an in-memory span
  recorder in CI (`internal/observability`, plus the controller and daemon
  span tests).
- Cross-process trace-id propagation over gRPC is asserted: a server span shares
  the client span's trace id.
- The trace-to-revision link is asserted in CI: the reconcile trace id is
  stamped on the WorkspaceRevision (`mitos.run/trace-id`) and equals the
  `workspace.dehydrate` span's trace id, the span is a child of the reconcile
  span with the expected attributes and no secret values, the annotation is
  omitted when tracing is off, and the `revision.created` feed event carries the
  trace id (empty when absent).

### OPEN

- The guest-side first-exec and guest-ready spans (the in-VM telemetry bridge
  over vsock: cpu steal, balloon pressure, in-guest process table) are the
  bare-metal tail and are not yet wired; see issue #29.
- A single trace id stamped across Hubble network flows needs the Cilium/Hubble
  integration.
- Grafana dashboards and PrometheusRule alerts that pivot on the trace id are a
  1.0 maturity item (#29).

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
forkd --audit-log=/var/log/mitos/audit.jsonl   # append to a file
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
| `mitos_fork_duration_seconds` | histogram | Time to fork a sandbox from snapshot, as measured by forkd. |
| `mitos_active_sandboxes` | gauge | Currently running sandboxes on this node. |
| `mitos_memory_shared_bytes` | gauge | CoW-aware shared memory: each template's shared page set counted once. |
| `mitos_memory_unique_bytes` | gauge | Per-fork unique (dirty-page) memory summed over all sandboxes. |
| `mitos_cow_memory_savings_bytes` | gauge | Memory the CoW model reveals is not consumed per-fork (naive minus CoW-aware). |
| `mitos_metered_disk_bytes` | gauge | CoW-aware metered backing storage: template volume seeds counted once. |

### Controller-level

| Metric | Type | Meaning |
|--------|------|---------|
| `mitos_claim_pending_total` | counter | Times a claim was requeued because no node had a ready snapshot (the claim stayed Pending). |
| `mitos_orphan_sweeps_total` | counter | Orphan sandbox VMs terminated by the garbage collector. |
| `mitos_claim_errors_total{pool,reason}` | counter | Claims that failed terminally, by pool and coarse reason (`fork`, `secret`, `volume`, `token`). |
| `mitos_pool_ready_snapshots{pool}` | gauge | Ready snapshots per pool, as of the last pool reconcile. |

Counter versus gauge for pending: `mitos_claim_pending_total` is a counter of
pending-requeue EVENTS, not a live gauge of currently-pending claims. A counter
is exact and lock-free to bump at the requeue site; an honest live gauge of
currently-pending claims would need a periodic recount with its own staleness
window. The counter directly answers "how often are claims failing to place".

### PROVEN

The controller metric increments are asserted in CI (the increments fire on the
pending-requeue, orphan-sweep, claim-error, and pool-reconcile paths).

### OPEN

- Snapshot-distribution lag is not yet exported.
- OpenCost and Hubble layers ride on husk pods (#18).

The Grafana dashboard, PrometheusRule alerts with runbook URLs, and the
conditions/reason-code catalogue that ride on these metrics are shipped; see
"Dashboards, alerts, runbooks" below.

## `kubectl sandbox` plugin

The `kubectl-sandbox` binary is a kubectl plugin that lists mitos.run sandbox
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

## Dashboards, alerts, runbooks

The `deploy/monitoring/` kustomize layer packages a Grafana dashboard and a
PrometheusRule over the metrics above, with one runbook per alert. It is OPT-IN:
it is NOT part of the default `kubectl apply -k deploy/` base, because it has
external dependencies. Install it separately once those are present:

```
kubectl apply -k deploy/monitoring/
```

### Artifacts

- `deploy/monitoring/prometheusrule.yaml`: a `monitoring.coreos.com/v1`
  PrometheusRule with five alerts: ClaimErrorRateHigh, ClaimsPendingSustained,
  WarmPoolStarved, OrphanSweepSpike, and ForkLatencyHigh. Each alert links a
  `docs/runbooks/` file via its `runbook_url` annotation.
- `deploy/monitoring/dashboard.json` plus `dashboard-configmap.yaml`: a Grafana
  dashboard (fork latency p50/p99, active sandboxes, CoW memory density, metered
  disk, claims pending, claim error rate by pool/reason, pool ready snapshots,
  orphan sweeps), wrapped in a ConfigMap.
- `docs/runbooks/*.md`: one runbook per alert (signal, likely causes, diagnosis
  with `kubectl sandbox ls/ps/top` and the metrics to check, remediation,
  escalation).
- `docs/conditions.md`: the normative reason-code catalogue the runbooks cite.

### Dependencies

- The PrometheusRule needs the Prometheus Operator (the
  `monitoring.coreos.com/v1` PrometheusRule CRD) and an operator whose
  `ruleSelector` matches the rule's labels.
- The dashboard ConfigMap uses the Grafana sidecar convention: the
  `grafana_dashboard: "1"` label, picked up by a Grafana running the dashboard
  sidecar (the kube-prometheus-stack default).

### Thresholds and the latency target

Every alert threshold is environment-tunable: the numbers in the PrometheusRule
are defensible starting points, not established SLOs, and each runbook says to
tune them from the cluster's observed baseline. In particular the ForkLatencyHigh
threshold is NOT the bare-metal latency target: the `<=10ms` p99 fork is a
bare-metal TARGET, while the alert fires on a looser, cluster-specific budget so a
busy or virtualized node does not page on the target itself.

### PROVEN

- The PrometheusRule is promtool-validated in CI (the manifests job extracts
  `.spec.groups` and runs `promtool check rules`), so a bad PromQL expression
  fails the build.
- The dashboard and the alerts reference only metrics the control plane actually
  exports (verified by grepping the metric names in `deploy/monitoring/` against
  `internal/`).
- The reason-code catalogue in `docs/conditions.md` covers every condition reason
  the controllers emit.

### OPEN

- Per-cluster threshold tuning is left to the operator (the runbooks say to tune).
- Helm-chart packaging of the dashboard and alerts is not done; the
  `deploy/monitoring/` kustomize layer is the current slice.
- Hubble flow panels and OpenCost cost-attribution need the Cilium/OpenCost
  integrations (#18).
- A snapshot-distribution-lag metric and its alert need the multi-node
  distribution path (#3).
