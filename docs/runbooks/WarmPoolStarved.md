# Runbook: WarmPoolStarved

## Signal

`min by (pool) (mitos_pool_ready_snapshots) < 1` sustained for 10m.

`mitos_pool_ready_snapshots{pool}` is a controller gauge that mirrors
`SandboxPool.Status.ReadySnapshots`, set each pool reconcile. The desired-vs-ready
signal: a healthy pool holds this at its desired warm count. Near zero means the
warm pool is starved and the next claim into that pool cold-forks or pends. The
threshold (here `< 1`) is environment-tunable; set it to the pool's desired warm
count or a low-water mark.

## Likely causes

- Snapshot build is failing or stuck on the holder nodes (the `SnapshotsReady`
  pool condition is not True).
- Husk pods are not at the desired replica count (the `HuskPodsReady` condition).
- Claims are draining the warm pool faster than it refills (demand spike).
- No KVM-labeled node is available to hold the pool's snapshot.

## Diagnosis

- `kubectl sandbox ls` and inspect the SandboxPool object: compare
  `status.readySnapshots` to the desired count.
- Pool `Ready` condition reason: `SnapshotsReady`, `HuskPodsReady` (healthy) vs a
  pending/failed reason. See `docs/conditions.md`.
- `kubectl sandbox ps` / `kubectl sandbox top` to see whether holder nodes are
  present and have headroom.
- Metrics: `mitos_pool_ready_snapshots{pool}` (the gauge driving this alert),
  `mitos_claim_pending_total` (are claims pending as a result?),
  `mitos_claim_errors_total{reason="fork"}` (are snapshot builds erroring?).

## Remediation

- Restore snapshot building: check forkd and the engine on the holder nodes
  (KVM health, snapshot artifacts).
- Raise the pool's desired warm count if demand structurally exceeds it.
- Add or recover KVM holder nodes so the pool can hold its snapshots.

## Escalation

If snapshot builds fail with no obvious node or KVM cause, escalate to the
`internal/fork` / `internal/firecracker` on-call (snapshot build path,
security-sensitive).
