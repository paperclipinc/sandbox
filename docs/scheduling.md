# Capacity-aware scheduling

This document describes how the controller decides which node forks a sandbox:
the per-node memory budget, the copy-on-write (CoW) aware cost projection, the
bin-packing policy that maximizes sharing, the overcommit factor and why it is
safe, and what happens to a claim when no node can take it. It also states what
is ENFORCED and tested versus what is OPEN.

The implementation is the `NodeRegistry` scheduler (`internal/controller/scheduler.go`
and `node_registry.go`, `SelectNode`) plus the claim reconciler's admission path
(`internal/controller/sandboxclaim_controller.go`). Per-node budgets and
per-template estimates arrive on forkd capacity heartbeats (Tasks 1-2); the
CoW-aware usage they carry is the metering report described in docs/metering.md.

## The admission model

### Node budget

A node's schedulable memory budget is:

```
budget = (host MemTotal - reserve) * overcommitFactor
```

`host MemTotal - reserve` is what forkd reports as `MemoryTotal` (the raw host
`MemTotal` minus a fixed reserve held back for the host kernel, forkd itself,
and page cache). The controller never sees the raw figure; it sees the already
reserved number. `overcommitFactor` (default 1.0) is applied controller-side
when projecting headroom.

A node that reports `MemoryTotal` 0 has an UNKNOWN budget: forkd could not read
meminfo (darwin/dev) or the mock engine has no total set. Such a node is treated
as effectively unlimited so dev and mock paths keep scheduling. Every real
forkd on Linux reports a budget.

### CoW-aware usage

The node's `MemoryUsed` is the CoW-aware physical footprint from docs/metering.md,
not the naive sum of every fork's resident set. Firecracker forks restore the
same template snapshot with `MAP_PRIVATE`, so N forks of one template share the
template's page set physically ONCE; only each fork's private-dirty pages are
unique. Counting the shared region once is what makes overcommit safe (below).

### Per-template marginal cost: cold vs warm

The scheduler never charges a whole VM for a fork. It projects the MARGINAL
memory a fork of template `T` would add to a candidate node, and the projection
depends on whether the node is already warm for `T`:

- **Warm node** (holds `T`'s snapshot and already runs at least one fork of it):
  the shared set is already resident, so the marginal cost is only the average
  per-fork unique footprint, `AvgForkUniqueBytes`.
- **Cold node** (does not run `T` yet): it pays the shared set once plus the
  per-fork unique footprint, `SharedOnceBytes + AvgForkUniqueBytes`.

Both estimates come from the node's own per-template capacity record when it has
one, then from any node that has forked `T`, then from a configured default
(256 MiB shared, 8 MiB unique) so an unknown template is never treated as free.
A node admits a fork only when its projected cost fits its remaining budget:

```
projectedCost(node, T) <= budget(node) - MemoryUsed(node)
```

## The bin-packing policy

The scheduler PACKS rather than spreads. Among the admitted nodes, `SelectNode`
ranks by pack tier for the requested snapshot:

1. **Warm** (tier 2): holds the snapshot and already runs forks of it. Reuses
   the resident CoW set, so an extra fork costs only its unique pages. Densest
   warm holder (most existing forks) wins, so sharing compounds on one node.
2. **Holder** (tier 1): holds the snapshot but has no recorded forks yet. Still
   cheaper than a cold start (no snapshot fetch), but the shared set is not yet
   amortized.
3. **Cold** (tier 0): does not hold the snapshot. Among cold candidates the
   scheduler spreads to the node with the MOST free memory, so cold starts do
   not pile onto one node and a known-budget node is preferred over an
   unknown-budget dev node. Ties break deterministically by node name.

This reverses the old load-spreading behavior: spreading forks of one template
across nodes would replicate the shared template set on each node and waste the
CoW win. Packing keeps the shared set resident once and pays only unique pages
per additional fork. A preferred node (the claim's `spec.nodeName`) is honored
only when it is healthy AND admits the fork, so a full preferred node does not
pin a claim.

## The overcommit factor and why it is safe

`overcommitFactor` scales the budget: a factor above 1 lets a node admit forks
whose summed naive footprint exceeds physical RAM. This is safe BECAUSE the
metering is CoW-aware. The physical cost of N forks of one template is the
template's shared set (resident once) plus N small unique sets, which is far
below N whole VMs. The factor leans on that measured sharing, not on a hope:
`MemoryUsed` already reflects the real resident footprint, so the budget check
is against physical truth, not a worst-case sum.

The factor is a deliberate dial, not a default optimization. At 1.0 there is no
overcommit. Raising it packs more sandboxes per node by trusting the sharing
assumption; if that assumption breaks (forks dirty most of their pages, or many
distinct templates land on one node), the node can be driven into reclaim or
OOM. Raise it only with the metering in front of you proving the physical
footprint on your workload.

## Pending, backpressure, and bounded failure

When no admitted node exists, `SelectNode` returns `ErrNoCapacity` (distinct
from an empty registry or no healthy nodes, which are placement preconditions).
The claim reconciler then:

1. Sets the claim `Phase = Pending` and records a `Ready=False` condition with
   reason `NoCapacity`: the cause ("no node has memory capacity under the
   overcommit policy") and the remediation ("the claim will retry; scale out
   nodes or raise the overcommit factor").
2. Stamps the first-pending instant on a durable annotation
   (`mitos.run/capacity-pending-since`) and requeues with a bounded backoff.
   The annotation, not a condition timestamp, is the deadline anchor: it changes
   only when the claim enters or leaves the capacity-pending state.
3. Bumps `mitos_claim_pending_total`. This counter is the backpressure
   signal a cluster-autoscaler or capacity-planning dashboard reads.

A claim that becomes admittable before the deadline (a node frees memory or a
new node joins) proceeds to `Restoring` as usual, and the pending stamp is
cleared so a later shortage starts a fresh clock.

A claim that stays capacity-pending longer than `--max-pending-duration`
(default 5m) FAILS cleanly: `Phase = Failed`, a `Ready=False` condition with
reason `CapacityExhausted` and an actionable message, `FinishedAt` stamped for
GC, and `mitos_claim_errors_total{reason="capacity"}` bumped. Failing after a
bounded wait is the boring, honest behavior: the claim does not hang forever and
the controller never forces a placement that would OOM a node.

Live and standard forks (`SandboxFork`) are NOT placed by this scheduler. A
`ForkRunning` copies the source VM's already-resident guest memory in place, so
a fork is pinned to the source sandbox's node by construction; the node's own
admission still guards it at the forkd layer.

## Enforced and tested vs open

ENFORCED and tested:

- Capacity-aware admission (the budget check and cold-vs-warm projection):
  unit-tested in `internal/controller/scheduler_test.go`.
- CoW bin-packing (pack warm holders dense, spread cold starts): unit-tested in
  the scheduler suite.
- Pending with backpressure and the `NoCapacity` condition; freeing capacity
  drives the claim to Ready: envtest in `internal/controller`.
- Bounded failure past `--max-pending-duration` with the `CapacityExhausted`
  condition and the capacity error metric: envtest in `internal/controller`.

OPEN (not implemented, do not assume):

- NUMA-aware vCPU/memory pinning.
- Hugepage-backed guest memory.
- KSM (kernel same-page merging) tuning for cross-template sharing.
- Multi-resource bin-packing: disk, CPU, and the cold-start
  snapshot-distribution cost of fetching a template to a cold node (ties into
  snapshot distribution, #14). Today only memory is packed.
- Preemption / eviction under pressure, and predictive prewarming.
- The MEASURED bare-metal density curve on the pinned reference node. The
  density numbers are a TARGET until they are run on that hardware and recorded
  in `bench/`; they are never fabricated (see the no-unverified-claims rule).
