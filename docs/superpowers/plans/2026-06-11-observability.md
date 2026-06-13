# Observability Implementation Plan (issue #29)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Advance issue #29. Give operators the self-hosted answer to "why is it slow" and "what did this sandbox do": OpenTelemetry tracing across the claim path (controller decision through forkd gRPC through the engine restore), a toggleable structured audit log of every exec and file operation per sandbox, the metrics the README/ROADMAP list but does not yet export (pending-claims depth, orphan-sweep counts, per-pool claim error rates), and a minimal `kubectl sandbox` plugin (ls/ps) so operators see sandboxes through kubectl. Verified in standard CI with an in-memory span exporter and audit/metric assertions; the full cross-process trace (including the real restore and exec) is asserted in KVM CI.

**Honesty constraint:** the README "Monitoring" section and ROADMAP already name metrics; this PR makes the listed-but-missing ones real and adds tracing/audit, and the docs state precisely what is exported and proven vs still open (Grafana dashboards, PrometheusRule alerts, the full guest-telemetry bridge, OpenCost/Hubble layers tied to husk pods). No metric or span is documented that the code does not emit and a test does not assert.

**Architecture:** A small `internal/observability` (or `internal/otel`) package centralizes tracer/exporter setup (a no-op tracer by default; an OTLP exporter when configured; an in-memory exporter for tests). The controller wraps reconciles and the forkd gRPC call in spans, propagating the trace context over gRPC metadata (otelgrpc interceptors on both the controller dial and the forkd server). forkd wraps Fork and the engine restore in child spans. The audit log is a structured logger on the SandboxAPI, emitting one record per exec/file op with sandbox id, operation, a safe summary (command string is logged; file CONTENT and secret values are never logged; large bodies are sized not dumped), and a timestamp, gated by an `--audit-log` flag. New Prometheus metrics live beside the existing ones. The `kubectl-sandbox` binary lists SandboxClaims/Forks via the k8s API and formats a table.

**Dependency (Task 1, verify go-1.24 first):** OpenTelemetry `go.opentelemetry.io/otel`, `otel/sdk`, `otel/exporters/otlp/otlptrace/otlptracegrpc`, and `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc`. Before adding, confirm the versions resolve with a `go` directive <= 1.24 and do not force a toolchain bump (the lesson from go-containerregistry; otel is widely go-1.22+ compatible, but verify and pin a compatible set). If any required otel module forces go >= 1.25, pin the last go-1.24-compatible otel release set and document.

**Context for the implementer:**
- Existing metrics: `internal/daemon/server.go` (forkDuration, activeSandboxes, memoryShared, memoryUnique, MustRegister; forkd exposes /metrics). Controller metrics use controller-runtime's registry (cmd/controller/main.go metrics server).
- Claim path: `internal/controller/sandboxclaim_controller.go` Reconcile -> selectNode -> forkOnNode (`forkd_client.go`, gRPC via NodeRegistry.GetConnection); forkd `internal/daemon/grpc_service.go` Fork -> `internal/daemon/server.go` Server.Fork -> engine.
- gRPC: forkd server built in `cmd/forkd/main.go` with the mTLS creds + the unary/stream identity interceptors (chain otelgrpc with them); controller dial in `node_registry.go` GetConnection (add otelgrpc stats handler / interceptor).
- Audit seam: `internal/daemon/sandbox_api.go` handlers (handleExec, handleReadFile, handleWriteFile, handleListDir, handleMkdir, handleRemove) already call `api.touch(sandboxID)`; the audit hook goes alongside.
- GC/pending: `internal/controller/gc.go` (orphan sweeps), the claim reconciler (pending claims when no node), pool reconciler (per-pool errors).
- kubectl plugin convention: a binary named `kubectl-sandbox` on PATH is invokable as `kubectl sandbox`.
- Conventions: CLAUDE.md authoritative. No em/en dashes. TDD. Explicit-path git add. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Lint darwin + GOOS=linux. Do not regress go 1.24. Secret values and file contents are NEVER logged or put in spans.

---

### Task 1: OpenTelemetry tracing across the claim path

**Files:** Create `internal/observability/tracing.go` + test; modify `cmd/controller/main.go`, `cmd/forkd/main.go`, `internal/controller/sandboxclaim_controller.go`, `internal/controller/forkd_client.go`, `internal/controller/node_registry.go`, `internal/daemon/grpc_service.go`, `internal/daemon/server.go`, `internal/fork/engine.go`.

- [ ] Verify the otel dependency set is go-1.24 compatible (see the dependency note); add and pin. If incompatible, pin the last compatible otel release and document.
- [ ] `internal/observability/tracing.go`: `Setup(ctx, serviceName, otlpEndpoint string) (shutdown func(ctx) error, err error)` building a TracerProvider (OTLP exporter when endpoint set, else a no-op/never-export provider so tracing is zero-cost when disabled). `Tracer(name) trace.Tracer`. A `func InMemoryForTest() (*tracetest.SpanRecorder, shutdown)` helper that installs an in-memory recorder as the global provider for assertions. Default OFF (no exporter) unless an endpoint is configured via flag/env.
- [ ] Controller: wrap the claim Reconcile in a span (`controller.reconcileClaim`, attributes: claim name, namespace, pool); the forkOnNode gRPC call is a child span. Add the otelgrpc stats handler to the controller's gRPC dial (node_registry GetConnection) so the trace context propagates to forkd.
- [ ] forkd: add the otelgrpc stats handler to the gRPC server (chained with the existing mTLS interceptors); Server.Fork starts a child span; the engine restore is a further child span (`engine.fork`, attribute: snapshot id, fork time). Spans carry NO secret values.
- [ ] Flags: `--otlp-endpoint` (controller and forkd; empty = tracing off). Wire Setup in both mains with a deferred shutdown.
- [ ] TDD: a daemon test using the in-memory recorder asserting Server.Fork produces a span with the expected name and a child engine-fork span; a controller envtest asserting a claim reconcile produces the reconcile span and (against a fake forkd that is also otel-instrumented) the trace context propagates (the forkd-side span shares the trace id). If full cross-process propagation is hard in envtest, at minimum assert the controller reconcile span and the forkd Server.Fork span each exist with correct attributes, and that the gRPC interceptor is installed (a propagation test over the in-process fake forkd).
- [ ] Commit `feat: OpenTelemetry tracing across the claim and fork path`.

### Task 2: toggleable structured audit log

**Files:** `internal/daemon/audit.go` + test, `internal/daemon/sandbox_api.go`, `cmd/forkd/main.go` + `cmd/sandbox-server/main.go`.

- [ ] `audit.go`: `type Auditor interface { Record(ev AuditEvent) }` with `AuditEvent{ SandboxID string; Op string; Detail string; Bytes int; Unix int64; OK bool }`; a `JSONAuditor` writing one JSON line per event to a configured io.Writer (stderr or a file), and a no-op auditor (default). The Detail field carries a SAFE summary: for exec the command string (commands are not secret values, but truncate to a bound); for files the path and a byte count, NEVER the content; never tokens or env secret values.
- [ ] SandboxAPI gains an `auditor Auditor` (default no-op); each handler records an event after the op (exec: command + exit code via OK; files: path + bytes). Wire alongside the existing touch().
- [ ] forkd + sandbox-server flags: `--audit-log` (path, or `-` for stderr; empty = off). Construct the JSONAuditor and pass to NewSandboxAPI.
- [ ] TDD: a SandboxAPI test with a recording auditor asserting exec and file ops each emit an AuditEvent with the right SandboxID/Op/OK and that file CONTENT and any token never appear in the recorded events (write a file with secret-looking content, assert the content is not in the audit record).
- [ ] Commit `feat: toggleable structured audit log of exec and file operations`.

### Task 3: the missing metrics

**Files:** `internal/controller/metrics.go` (new, controller-runtime registry), `internal/controller/sandboxclaim_controller.go`, `internal/controller/gc.go`, `internal/controller/sandboxpool_controller.go`, `internal/daemon/server.go` (if any forkd-side), test.

- [ ] Register (controller-runtime's metrics.Registry): `mitos_pending_claims` (gauge, claims waiting for a node), `mitos_orphan_sweeps_total` (counter, VMs reaped by GC), `mitos_claim_errors_total` (counter, labeled by pool and reason), and `mitos_pool_ready_snapshots` (gauge per pool) if not already exported. Bump them at the right sites: pending when selectNode finds no node (claim stays Pending); orphan_sweeps in the GC sweep; claim_errors on the fork-failed / secret-failed paths; ready_snapshots in the pool reconcile.
- [ ] TDD: a test that drives the relevant reconcile/GC path and asserts the metric value changed (use the prometheus testutil to read the metric). At least pending-claims (a claim with no registered node bumps it) and orphan-sweeps (a GC pass reaping an orphan bumps it).
- [ ] Commit `feat: pending-claims, orphan-sweep, and claim-error metrics`.

### Task 4: minimal `kubectl sandbox` plugin (ls / ps)

**Files:** Create `cmd/kubectl-sandbox/main.go` + an `internal/cli/sandboxtable` formatting helper + test.

- [ ] `kubectl-sandbox` binary: subcommands `ls` (list SandboxClaims across or in a namespace: NAME, POOL, PHASE, NODE, ENDPOINT, AGE) and `ps` (list a claim's forks, or all forks). Uses the k8s client (kubeconfig) to list the CRDs. A `tree`/`top` are noted as follow-ups. Output a clean aligned table; support `-n namespace` and `-A`.
- [ ] Factor the table formatting (claims -> rows -> aligned text) into a pure function and unit-test it against constructed SandboxClaim objects (no cluster): the formatter produces the expected columns and ages. The live k8s listing is a thin wrapper, not unit-tested (manual / envtest optional).
- [ ] Commit `feat: kubectl sandbox plugin with ls and ps`.

### Task 5: KVM CI trace assertion + docs + PR

**Files:** `.github/workflows/kvm-test.yaml` (optional trace assertion), `docs/observability.md` (new), `README.md` (Monitoring section), `ROADMAP.md`, full verification.

- [ ] Optional KVM CI: if feasible, run a forkd + a fork with `--otlp-endpoint` pointed at a file-exporter or a tiny collector and assert a span tree covering restore+exec is produced; if a full collector is too heavy, rely on the in-memory span tests for the trace structure and note that the cross-process production trace is exercised by the standard tests. Do not over-engineer the CI; the unit/envtest span assertions are the gate.
- [ ] `docs/observability.md`: the trace model (claim decision -> forkd gRPC -> engine restore -> first exec; how to enable via --otlp-endpoint; the propagation), the audit log (format, what is and is not logged, how to enable), the metrics (the full list now exported with their meaning), the kubectl sandbox plugin (ls/ps). State PROVEN (spans + audit + metrics asserted in CI) vs OPEN (full guest-telemetry vsock bridge, the end-to-end trace id stamped on pod/logs/Hubble/revisions which needs husk pods #18 and Workspace #21, Grafana dashboards + PrometheusRule alerts + runbooks for 1.0, OpenCost/Hubble layers).
- [ ] README Monitoring section: update the metrics table to include the newly exported metrics, add a one-line tracing + audit-log + kubectl-sandbox mention pointing at docs/observability.md. No unverified claims.
- [ ] ROADMAP section 8 (observability) and section 10 references: flip OTel tracing (claim/fork path), audit log, and the named metrics to done; leave the full guest-telemetry bridge, end-to-end trace-id-everywhere, Grafana/alerts, and OpenCost/Hubble open with notes.
- [ ] Full verification (build darwin + GOOS=linux, vet, lint both, all Go suites with envtest, Python suite, gofmt zero, dash grep zero, go 1.24 preserved, otel deps clean, YAML parse).
- [ ] Push `feat/observability`, PR `Observability: OTel tracing, audit log, metrics, kubectl sandbox` body Closes-or-advances #29, watch CI, dismiss guarded CodeQL alerts with justification if any, merge when green.

**Out of scope (remain open in #29):** the guest-telemetry vsock bridge (cpu steal, balloon, in-guest process table); the single trace id stamped across pod/metrics/logs/Hubble/workspace revisions (needs husk pods #18 + Workspace #21); Grafana dashboards + PrometheusRule alerts with runbook URLs + the conditions/reason-code catalogue (a 1.0 maturity item); OpenCost and Hubble layers (free via husk pods, #18); `kubectl sandbox top/exec/tree/logs` beyond ls/ps.
