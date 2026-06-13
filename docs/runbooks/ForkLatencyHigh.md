# Runbook: ForkLatencyHigh

## Signal

`histogram_quantile(0.99, sum by (le) (rate(mitos_fork_duration_seconds_bucket[5m]))) > 0.05`
sustained for 10m.

`mitos_fork_duration_seconds` is a forkd histogram (buckets up to 100ms) of
the time to fork a sandbox from a snapshot. This alert fires on p99 fork latency
over a cluster budget (50ms here).

## The bare-metal target vs this threshold

The `<=10ms` p99 fork is a bare-metal TARGET, not this alert threshold and not an
asserted SLO. Bare metal is a first-class target for this project, but a busy or
virtualized node will not hit 10ms, and paging on the target itself would be
noise. This alert deliberately uses a looser, cluster-specific budget. The
threshold is environment-tunable: set it from your cluster's observed p99
baseline, not from the bare-metal target.

## Likely causes

- Host pressure on the forkd node: CPU, memory, or IO contention slowing
  snapshot restore.
- Cold snapshots: the warm page set is not resident, so restore reads from disk.
- Storage latency on the snapshot backing volume.
- Virtualized / oversubscribed nodes where the 10ms target is unreachable by
  design (expected; tune the budget).

## Diagnosis

- `kubectl sandbox top` for forkd node CPU / memory / IO pressure.
- `kubectl sandbox ps` to see fork volume per node (is one node hot?).
- Metrics: `mitos_fork_duration_seconds_bucket` (the histogram; inspect p50
  vs p99 to see whether it is a tail or a shift), `mitos_active_sandboxes`
  (load correlation), `mitos_memory_shared_bytes` /
  `mitos_memory_unique_bytes` (CoW density: low sharing means more pages
  faulted per fork).
- forkd logs on the slow node.

## Remediation

- Relieve host pressure (reschedule noisy neighbors, add nodes).
- Keep snapshots warm so restore stays in memory.
- Move snapshot backing storage to faster media.
- If the node is virtualized and cannot reach the target, raise the
  environment-tunable budget to match its baseline; do not chase the bare-metal
  target on virtualized hardware.

## Escalation

If p99 latency rises on bare metal with no host-pressure cause, escalate to the
`internal/fork` / `internal/firecracker` on-call (snapshot restore path,
security-sensitive). A regression against the bare-metal target should carry a
`bench/` reproduction before any claim is made about it.
