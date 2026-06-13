# Failure and GC Semantics Implementation Plan (issues #12, #13)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close issue #13 (claim lifecycle: timeout and idle reaping, finalizers, orphan sweep) and most of epic #12 (NodeLost, controller-restart reconciliation, etcd TTL hygiene). Every component gets a defined, tested answer to crash, node loss, expiry, and capacity. Verified in envtest with fake forkd nodes; no KVM required.

**Architecture:** One new primitive underpins everything: `ListSandboxes`, a forkd gRPC RPC returning every VM forkd actually holds with its uptime and last-activity time. forkd tracks last activity by stamping a timestamp on every exec and file call through the SandboxAPI. On top of `ListSandboxes`:
- A finalizer on SandboxClaim and SandboxFork guarantees the backing VM is reaped via forkd Terminate before the object is removed, regardless of how deletion was triggered.
- maxLifetime and idleTimeout drive claims to a terminal `Terminated` phase (forkd Terminate, typed condition, FinishedAt stamped), never a hung Ready.
- A periodic GC reconciler reconciles desired (live claims and forks) against actual (forkd ListSandboxes across nodes): orphan VMs with no backing object are terminated; this also reconstructs state after a controller restart.
- Claims whose node left the registry transition to a terminal `NodeLost` condition within a bounded time.
- Finished claims and forks are deleted after a configurable TTL so etcd does not bloat.

**Tech Stack:** Go, controller-runtime + envtest, protobuf regen, finalizers and owner references.

**Context for the implementer:**
- Claim reconciler: `internal/controller/sandboxclaim_controller.go` (reconcileTimeout currently only flips phase, never reaps; `setCondition` helper in `sandboxpool_controller.go`; `conditionStatus`). Fork reconciler: `internal/controller/sandboxfork_controller.go`. Forkd client: `internal/controller/forkd_client.go` (forkOnNode pattern, GetConnection). Registry: `internal/controller/node_registry.go` (NodeInfo, GetNode, ListNodes, NodesWithTemplate, isHealthy, PruneStale). Discovery: `internal/controller/forkd_discovery.go`.
- forkd: `internal/daemon/server.go` (Server.Fork/ForkRunning/Terminate), `internal/daemon/grpc_service.go` (RPC handlers, validateSandboxID guard), `internal/daemon/sandbox_api.go` (handleExec, handleReadFile, etc, all route via getAgent), engine `internal/fork/engine.go` (e.sandboxes map keyed by ID, Sandbox struct with CreatedAt), `internal/fork/mock.go` (MockEngine.sandboxes).
- Proto: `proto/forkd.proto`; regen `make proto` (PATH may need `$(go env GOPATH)/bin`); commit generated code.
- API types: `api/v1alpha1/types.go` (SandboxClaimSpec.Timeout exists; SandboxPhase consts; SandboxClaimStatus; SandboxForkSpec/Status). Regen deepcopy + CRDs: `~/go/bin/controller-gen object paths=./api/...` and `~/go/bin/controller-gen crd paths=./api/... output:crd:artifacts:config=deploy/crds`.
- Test helper: `StartFakeForkdNode` in `internal/controller/testsupport_test.go` (wires a real daemon Server with MockEngine + httptest HTTP API). Suite: `internal/controller/suite_test.go` (testRegistry). Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -count=1 -race`.
- Conventions: CLAUDE.md authoritative. No em or en dashes. TDD. Explicit-path git add. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer. Lint darwin and GOOS=linux. Secret values and tokens never logged.

---

### Task 1: forkd activity tracking + ListSandboxes RPC

**Files:** `proto/forkd.proto` (+ regen), `internal/daemon/sandbox_api.go`, `internal/fork/engine.go`, `internal/fork/mock.go`, `internal/daemon/interface.go`, `internal/daemon/grpc_service.go`, tests in `internal/daemon/` and `internal/fork/`.

- [ ] Proto: add to the service `rpc ListSandboxes(ListSandboxesRequest) returns (ListSandboxesResponse);` with
```proto
message ListSandboxesRequest {}
message ListSandboxesResponse { repeated SandboxInfo sandboxes = 1; }
message SandboxInfo {
  string sandbox_id = 1;
  int64 created_at_unix = 2;
  int64 last_activity_unix = 3;  // zero when never accessed
  int64 uptime_seconds = 4;
}
```
Regen, commit generated code with this task.
- [ ] Activity tracking: `SandboxAPI` gains `lastActivity map[string]time.Time` guarded by its mutex and a `touch(sandboxID string)` called at the top of every exec and file handler (handleExec, handleReadFile, handleWriteFile, handleListDir, handleMkdir, handleRemove). Expose `LastActivity(sandboxID string) (time.Time, bool)`. UnregisterSandbox clears it. Use an injectable clock (`api.now func() time.Time`, defaulting to time.Now, settable in tests) so tests are deterministic.
- [ ] Engine: `ForkEngine` interface gains `ListSandboxes() []SandboxRecord` where `SandboxRecord{ ID string; CreatedAt time.Time }`. Real engine returns from e.sandboxes; mock from its map. (Last-activity lives in the SandboxAPI, not the engine, because the engine does not see exec traffic; the Server merges the two.)
- [ ] Server: `Server.ListSandboxes()` merges engine records with SandboxAPI last-activity into the proto response; grpc_service ListSandboxes handler calls it. No auth change (the controller-identity interceptor already guards it under TLS).
- [ ] TDD: daemon test that two forks then exec on one updates only that one's last-activity and ListSandboxes reports both with correct created/activity; mock-engine fork test that ListSandboxes returns the live set.
- [ ] Commit `feat: forkd activity tracking and ListSandboxes RPC`.

### Task 2: claim finalizer reaps the VM on delete

**Files:** `internal/controller/sandboxclaim_controller.go`, `internal/controller/finalizer.go` (new shared helper), test `internal/controller/claim_finalizer_test.go`.

- [ ] Constant `FinalizerTerminate = "mitos.run/forkd-terminate"`. On reconcile of a non-deleting claim that has reached at least Restoring, ensure the finalizer is present (controllerutil.AddFinalizer + Update). When `DeletionTimestamp` is set: if the claim has a Node and SandboxID, call forkd Terminate on that node (tolerate NotFound and node-gone as success: the VM is already gone), then RemoveFinalizer + Update and return. A `terminateOnNode(ctx, nodeName, sandboxID)` helper in finalizer.go gets the connection via NodeRegistry and calls forkdpb Terminate; treat `codes.NotFound` and a missing node as already-terminated (no error).
- [ ] Reconcile early-branch: `if !claim.DeletionTimestamp.IsZero() { return r.reconcileDelete(ctx, &claim) }` placed before the normal flow.
- [ ] TDD (envtest): create claim, drive to Ready against StartFakeForkdNode (records Terminate calls; extend the fake to record terminated sandbox IDs if not already), delete the claim, assert the fake forkd received Terminate for the sandbox ID and the object is actually gone (finalizer removed). Second test: deleting a claim whose node has been unregistered still completes deletion (no hang) and is treated as already-terminated.
- [ ] Commit `feat: claim finalizer reaps the backing VM on delete`.

### Task 3: maxLifetime and idleTimeout drive claims to terminal Terminated

**Files:** `api/v1alpha1/types.go` (+ regen), `internal/controller/sandboxclaim_controller.go`, test `internal/controller/claim_lifecycle_test.go`.

- [ ] API: add `IdleTimeout *metav1.Duration json:"idleTimeout,omitempty"` to SandboxClaimSpec (Timeout already exists; treat it as maxLifetime). Add phase const `SandboxTerminated SandboxPhase = "Terminated"` and status fields `FinishedAt *metav1.Time json:"finishedAt,omitempty"` and keep Conditions. Regen deepcopy + CRDs.
- [ ] Replace reconcileTimeout with `reconcileLifetime` for Ready claims: compute maxLifetime deadline from StartedAt+Timeout, and idle deadline from last-activity+IdleTimeout. Last-activity comes from forkd: query the claim's node via a new `sandboxActivity(ctx, node, sandboxID) (created, lastActivity time.Time, ok bool)` helper using ListSandboxes (filter to the id). If maxLifetime exceeded: terminate (forkd Terminate via the finalizer helper), set phase Terminated, condition Type=Terminated Reason=MaxLifetimeExceeded, stamp FinishedAt. If idle (now - max(lastActivity, StartedAt) > IdleTimeout): same with Reason=IdleTimeout. Otherwise requeue at the nearest deadline. A Terminated claim keeps its finalizer cleared path: terminate directly here AND keep the object (do not delete it; TTL task removes it). Ensure idempotency: a claim already Terminated returns without re-terminating.
- [ ] Important: terminating here must also remove the finalizer or the object would block deletion later; simpler design: lifetime expiry sets phase Terminated and calls forkd Terminate directly, leaving the finalizer in place (harmless: on eventual delete, terminateOnNode tolerates NotFound). Confirm the finalizer NotFound-tolerance from Task 2 makes this safe and add a test.
- [ ] TDD (envtest): claim with Timeout=2s reaches Ready, then within a few seconds becomes Terminated with MaxLifetimeExceeded and the fake forkd saw Terminate. Idle test: claim with IdleTimeout=2s and no exec activity becomes Terminated with IdleTimeout; a claim kept active (the test stamps recent activity on the fake forkd via a settable clock or a recorded touch) does NOT get reaped. Use the fake forkd's MockEngine plus the SandboxAPI activity seam; you may need StartFakeForkdNode to expose a way to set last-activity for a sandbox id (add `SetSandboxActivity(id string, t time.Time)` to the helper, wired to the SandboxAPI clock/map).
- [ ] Commit `feat: maxLifetime and idleTimeout reap claims to a terminal Terminated phase`.

### Task 4: GC reconciler, orphan sweep and controller-restart reconciliation

**Files:** `internal/controller/gc.go` (new, a manager Runnable like ForkdDiscovery), `internal/controller/gc_test.go`, wire into `cmd/controller/main.go`.

- [ ] `GarbageCollector` Runnable: every interval (default 30s, configurable) lists all SandboxClaims and SandboxForks in all namespaces, builds the desired set of (node, sandboxID) it expects alive (Ready claims and Ready fork children, by status.Node + status.SandboxID), then for each registered healthy node calls ListSandboxes and terminates any forkd sandbox whose id is not in the desired set AND whose uptime exceeds a grace period (default 60s, so freshly-forked-not-yet-status-written VMs are not killed). Log every orphan kill with node and id (ids are safe to log). This is also controller-restart reconciliation: after a restart the desired set is rebuilt from CRD state and the diff against forkd actuals is reconciled with zero orphaned VMs.
- [ ] TDD (envtest): register a fake forkd, fork a sandbox directly on its engine that has NO backing claim and an uptime past the grace (the fake helper needs a way to inject a sandbox with a backbacked age; add `InjectOrphanSandbox(id string, age time.Duration)` to StartFakeForkdNode wiring the MockEngine + activity), run one GC pass (call gc.runOnce(ctx) directly rather than the ticker), assert the orphan was Terminated and a sandbox WITH a backing Ready claim was not.
- [ ] Wire `mgr.Add(&controller.GarbageCollector{Client, Registry, ...})` in main.go.
- [ ] Commit `feat: GC reconciler terminates orphan VMs and reconciles after controller restart`.

### Task 5: NodeLost transition

**Files:** `internal/controller/sandboxclaim_controller.go` or fold into the GC reconciler, test `internal/controller/nodelost_test.go`.

- [ ] In the GC pass (it already iterates claims and knows registry health), for each Ready claim whose status.Node is not a currently-healthy registered node, set a typed condition Type=Ready Status=False Reason=NodeLost plus phase=Failed (or a dedicated `SandboxNodeLost` phase const; reuse SandboxFailed with the NodeLost reason to avoid phase sprawl, decide and document) and stamp FinishedAt. Bounded time = the GC interval. Do not terminate (the node is gone; nothing to call). Pools rebuilding replicas elsewhere is out of scope for this PR (note it in ROADMAP as still open).
- [ ] TDD (envtest): claim Ready on a fake node, unregister the node (registry.Unregister), run a GC pass, assert the claim transitions to NodeLost within the pass; a claim on a still-healthy node is untouched.
- [ ] Commit `feat: claims on lost nodes transition to a terminal NodeLost condition`.

### Task 6: etcd TTL hygiene for finished objects

**Files:** `api/v1alpha1/types.go` (pool/claim TTL field), `internal/controller/gc.go`, test.

- [ ] Add `TTLSecondsAfterFinished *int32 json:"ttlSecondsAfterFinished,omitempty"` to SandboxClaimSpec (Job-like; default via a controller flag when unset, e.g. 600s). In the GC pass, delete claims and forks that are in a terminal phase (Terminated, Failed/NodeLost) whose FinishedAt is older than the TTL. Deletion triggers the finalizer (already NotFound-tolerant). Regen CRDs.
- [ ] TDD (envtest): a Terminated claim with FinishedAt in the past and a short TTL is deleted by a GC pass; one within TTL survives.
- [ ] Commit `feat: TTL cleanup of finished claims and forks for etcd hygiene`.

### Task 7: docs, RBAC, verification, PR

**Files:** `deploy/controller/deployment.yaml` (RBAC: claims/forks delete already present? verify; add if missing), `ROADMAP.md`, `docs/threat-model.md` (only if surface moved; it does not materially), `docs/failure-gc.md` (new, short: the model and its tests), full verification.

- [ ] RBAC: the controller ClusterRole already has delete on the CRDs; confirm and add update on finalizers subresource if your controller-runtime version needs it (it patches metadata.finalizers via the main resource update, usually fine; verify the envtest passes which proves RBAC is sufficient in-process, but the deployed ClusterRole must still allow the operations the controller performs: delete on sandboxclaims/sandboxforks, already granted).
- [ ] `docs/failure-gc.md`: enumerate each guarantee (finalizer reap, maxLifetime, idleTimeout, orphan sweep, controller-restart reconciliation, NodeLost, TTL) with the test that proves it and the bounded time. State what is still open (forkd-crash supervision of running VMs, node-loss pool replica rebuild, saturation queueing, status-update rate limiting) and point to epic #12.
- [ ] ROADMAP section 2: flip the implemented lines (orphan sweeps, claim TTLs, NodeLost, controller-restart reconciliation) to done; leave forkd-crash supervision, pool replica rebuild, and saturation as open with notes.
- [ ] Full verification: build, vet, lint darwin+linux, all Go suites with envtest, Python suite, gofmt zero, dash grep zero, CRD/deepcopy regenerated and committed, YAML parses.
- [ ] Push `feat/failure-gc`, PR `Failure and GC: finalizers, lifetime and idle reaping, orphan sweep, NodeLost` body Closes #13 and references #12 (partial), watch CI, merge when green per the standing workflow.

**Out of scope (remain open in #12):** forkd-crash supervision (forkd reaping its own orphan FC processes on restart needs a forkd-local state file; separate work), pool replica rebuild after node loss, saturation queue-with-deadline, status-update rate limiting/batching, chaos CI suite (kill -9 components). Note each in ROADMAP and docs/failure-gc.md.
