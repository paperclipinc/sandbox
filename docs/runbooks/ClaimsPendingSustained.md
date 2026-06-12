# Runbook: ClaimsPendingSustained

## Signal

`sum(rate(agentrun_claim_pending_total[5m])) > 0.1` sustained for 15m.

`agentrun_claim_pending_total` is a controller counter that increments every
time a claim is requeued because no node had a ready snapshot (the claim stayed
Pending). A sustained rate is capacity exhaustion: the fleet cannot place new
claims. The threshold is environment-tunable.

## Likely causes

- Warm pools are drained: not enough ready snapshots to satisfy demand (see also
  the WarmPoolStarved alert).
- Nodes are full: no node has admission capacity before the pending deadline
  (the `NoCapacity` / `CapacityExhausted` claim reason).
- KVM holder nodes are missing, drained, or NotReady, so snapshots cannot land.
- A demand spike outran the pool's warm replica count.

## Diagnosis

- `kubectl sandbox ls` to see how many claims are Pending and in which pools.
- `kubectl sandbox ps` to see per-node occupancy and spot full or absent nodes.
- `kubectl sandbox top` for node-level capacity headroom.
- Metrics to check: `agentrun_claim_pending_total` (the rate driving this
  alert), `agentrun_pool_ready_snapshots{pool}` (which pools are starved),
  `agentrun_active_sandboxes` per node (are nodes saturated?).
- SandboxClaim `Ready` condition reason: `NoCapacity` / `CapacityExhausted`
  confirms admission pressure; `NoHuskPod` confirms warm-pool starvation. See
  `docs/conditions.md`.

## Remediation

- Scale the warm pool: raise the SandboxPool desired replica / snapshot count.
- Add KVM-labeled nodes (`agentrun.dev/kvm=true`) so forkd can schedule and
  snapshots can land.
- Recover any drained or NotReady holder nodes.
- If demand is structurally above capacity, plan fleet growth; the alert is a
  capacity signal, not a transient one.

## Escalation

If capacity is present but claims still pend (scheduler disagreement, snapshots
not landing on healthy nodes), escalate to the controller / `internal/daemon`
on-call. Capacity-truth bugs are in the placement path.
