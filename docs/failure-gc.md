# Failure and GC semantics

This document enumerates the failure and garbage-collection guarantees the
control plane provides today, the test that proves each one, and the time bound
within which it holds. It also states what remains open and points to the
tracking epic.

Two control loops cooperate:

- the SandboxClaim reconciler (`internal/controller/sandboxclaim_controller.go`),
  event-driven per claim, owns the finalizer reap and the lifetime/idle reap;
- the GarbageCollector (`internal/controller/gc.go`), a periodic Runnable that
  runs every `Interval` (default 30s), owns NodeLost, the orphan sweep, and TTL.

Tunables and their defaults (see `gc.go`):

- `Interval`: 30s. The period between GC passes; the bound on NodeLost and the
  orphan sweep.
- `OrphanGrace`: 60s. Minimum uptime before a backing-less VM is swept, so a
  just-forked VM whose claim status has not landed is never killed.
- `DefaultTTLSeconds`: 600s. TTL for a finished claim that does not set
  `spec.ttlSecondsAfterFinished`.
- finalizer terminate RPC timeout: 10s (`terminateOnNode` in `finalizer.go`).

## Guarantees

### Finalizer reap: a claim never disappears without its VM being reaped

Every claim acquires the `mitos.run/forkd-terminate` finalizer before it
acquires a VM. On delete the reconciler calls forkd `Terminate` on the claim's
node, then removes the finalizer. The RPC is bounded at 10s and tolerant: a node
that has left the registry, is unhealthy, cannot be dialed, or answers
`NotFound`, `Unavailable`, or `DeadlineExceeded` is treated as already
terminated, so a delete never wedges on an unreachable node. Any other error is
returned so a genuinely-reachable forkd that rejects the call is retried.

- Bound: the backing VM is reaped before the object is removed; the reap RPC is
  bounded at 10s.
- Proving tests: `TestClaimDeleteReapsBackingVM`,
  `TestClaimDeleteWithGoneNodeCompletes`,
  `TestClaimDeleteWithUnreachableForkdCompletes`.

### maxLifetime: a Ready claim is reaped at its wall-clock deadline

A Ready claim with `spec.timeout` set reaches the terminal `Terminated` phase
once `StartedAt + timeout` passes. The reaper terminates the VM, stamps
`FinishedAt`, and sets a `Terminated` condition with reason `MaxLifetimeExceeded`.
maxLifetime does not depend on a reachable forkd for the decision.

- Bound: terminal within a reconcile after the deadline.
- Proving test: `TestClaimMaxLifetimeReaped`.

### idleTimeout: an inactive Ready claim is reaped

A Ready claim with `spec.idleTimeout` set is reaped once it has been idle past
the timeout, measured from the later of `StartedAt` and last activity. Activity
comes from forkd via the `ListSandboxes` primitive, which reports each sandbox's
last exec or file activity. A claim kept active is not reaped within the window;
an unreachable node defers the decision (requeue) rather than reaping blindly.
Reason on the `Terminated` condition is `IdleTimeout`.

- Bound: terminal within a reconcile after the idle deadline, given a reachable
  forkd.
- Proving tests: `TestClaimIdleTimeoutReaped`,
  `TestClaimIdleTimeoutNotReapedWhenActive`.

### Orphan sweep: a backing-less VM is reaped, with a live-claim-by-name net

Each pass, the GC lists sandboxes on every healthy node and terminates any whose
id is in neither the per-node desired-alive set (Ready claims and Ready fork
children, keyed by node and id) nor the node-independent liveID set, and whose
uptime exceeds `OrphanGrace`.

The liveID net is the safety valve. The controller uses `claim.Name` AS the
sandbox id (the claim reconciler forks with `claim.Name` and forkd echoes it
back, so `status.SandboxID == claim.Name` once Ready). So the liveID set is
every non-terminal claim by name UNION every non-terminal fork child by its
explicit `SandboxID`. A VM whose claim is wedged in `Restoring` or `Pending`
past the grace, and never wrote its status, is still recognized by name and left
alive. A VM becomes a sweep candidate only once its claim object is gone (or its
node is lost). This is a deliberate bound: a claim wedged in a non-terminal phase
keeps its VM alive by design.

- Bound: a genuine orphan (no backing object) is reaped within one `Interval`
  once its uptime exceeds `OrphanGrace`.
- Proving tests: `TestGCSweepsOrphanVMs` (orphan past grace swept; fresh orphan
  and backed VM left alone), `TestGCLiveClaimByNameNotSwept` (live claim's VM by
  name not swept while the claim exists; swept after the claim is deleted).

### Controller-restart reconciliation: desired state is rebuilt from CRDs

The GC holds no in-memory desired state. Each pass rebuilds the desired-alive and
liveID sets purely from CRD state (claims and forks) and reconciles them against
forkd-reported actual VMs. After a controller restart the first pass therefore
sweeps any VM not accounted for and leaves every backed VM alone, with no
bootstrap window where state is lost.

- Bound: reconciled within one `Interval` of the restarted controller starting.
- Proving test: covered structurally by the orphan-sweep tests, which drive a
  fresh `GarbageCollector` with no prior state against live forkd VMs
  (`TestGCSweepsOrphanVMs`, `TestGCLiveClaimByNameNotSwept`).

### Node health: liveness, not just last-seen

A node is schedulable only while it is healthy. Health requires BOTH a recent
heartbeat (the 2-minute last-seen TTL on `LastHeartbeat`) AND a live forkd: a
node whose forkd liveness probe (the discovery `GetCapacity` call, every 15s)
fails `probeFailureThreshold` (3) times in a row is marked unhealthy and dropped
from `SelectNode`, even with a fresh heartbeat. This closes the gap where a pod
stays `Running` while forkd is hung or the host is dead: previously such a node
stayed healthy and schedulable for the full 2-minute TTL on stale capacity. The
threshold absorbs a transient single-probe blip (no flapping); at the 15s
interval, 3 failures is roughly 45s before the node leaves the schedulable set,
well inside the heartbeat TTL.

- Bound: roughly `probeFailureThreshold * discovery interval` (about 45s) before
  a hung forkd's node is dropped from scheduling.
- Proving tests: `TestNodeUnhealthyAfterProbeFailureThreshold`,
  `TestSyncPodsDropsNodeOnRepeatedProbeFailure`.

### NodeLost: a raw-forkd claim on a lost node reaches a terminal phase

In RAW-FORKD mode, a Ready claim whose node is no longer a healthy registered
node is transitioned to the terminal `Failed` phase with a `NodeLost` reason and
`FinishedAt` stamped. The node is gone, so there is nothing to terminate; the GC
only stamps state. The ephemeral VM died with the node and there is no recovery,
so failing the claim (and letting the TTL pass reap it) is correct. The orphan
sweep and NodeLost never fight: the sweep visits only healthy nodes, so a claim
on a lost node is never swept. A claim on a still-healthy node is untouched.

In HUSK mode, `markNodeLost` is a no-op: a Ready husk-backed claim recovers from
node loss by RE-PENDING onto a replacement dormant slot (owned by
`checkHuskPodLost` and the husk pod watch, which the warm pool self-heals). The
GC must not race that re-pend into a terminal `Failed`, so it skips the
node-lost-fail entirely in husk mode. The GC carries `EnableHuskPods` from the
controller run mode to make this decision.

- Bound: raw mode fails within one `Interval` of the node going unhealthy or
  leaving the registry; husk mode re-pends on the pod event (or the claim's own
  requeue).
- Husk hard-node-loss latency is cluster-dependent, not a mitos GC interval. Husk
  node-loss recovery fires immediately on a pod delete or `DeletionTimestamp`
  event. But a HARD host loss where the pod object lingers `Running` with no
  `DeletionTimestamp` is bounded by the cluster's own unreachable-pod eviction
  setting (the `node.kubernetes.io/unreachable` taint toleration, k8s default
  about 5 minutes), since no pod event fires until the cluster evicts the pod.
  Operators wanting faster husk node-loss recovery should tune the unreachable
  toleration or the pod-eviction timeout; mitos cannot shorten it.
- Proving tests: `TestGCMarksNodeLost`, `TestGCLeavesHealthyNodeClaim`,
  `TestGCInHuskModeDoesNotFailNodeLostClaim`.

### TTL hygiene: finished objects are deleted, including early-failed claims

A claim in a terminal phase (`Terminated` or `Failed`) whose `FinishedAt` is
older than its effective TTL (`spec.ttlSecondsAfterFinished`, else
`DefaultTTLSeconds`) is deleted, which triggers the finalizer reap. A claim with
no `FinishedAt` is skipped, and a recently-finished claim survives until its TTL.

Crucially, the reconciler's early-failure paths (volume preparation, secret
resolution, token minting, fork, token-secret write) stamp `FinishedAt` when
they set `Failed`, so an early-failed claim is TTL-eligible instead of leaking in
etcd forever.

- Bound: deleted within one `Interval` after `FinishedAt + TTL`.
- Proving tests: `TestGCTTLDeletesExpiredFinishedClaim`,
  `TestGCTTLKeepsRecentFinishedClaim`, `TestGCTTLsEarlyFailedClaim`.

## Known bounds and open items

By design, a VM is reaped only once its claim object is gone or its node is lost.
A claim wedged in a non-terminal phase keeps its VM alive (the liveID net). This
trades a possible leak of a wedged claim's VM for never killing a live VM whose
status simply has not landed; the wedged claim is itself observable and
deletable, at which point its VM is swept.

The following remain OPEN and are tracked in epic #12:

- forkd-crash supervision of running VMs: a restarted forkd reaping its own
  pre-crash Firecracker processes. This needs forkd-local state so forkd can
  recognize VMs it owned before the crash; it is separate from the controller's
  orphan sweep, which only reaps VMs forkd still reports.
- pool replica rebuild after node loss: NodeLost fails the claims on a dead node
  within the GC interval, but pools do not yet rebuild the lost replicas
  elsewhere.
- saturation behavior: queue-with-deadline then a typed fail-fast condition when
  capacity is exhausted.
- status-update rate-limiting and batching: status writes are not yet
  rate-limited or batched.
- chaos CI suite: kill -9 of components under load is not yet exercised in CI.
