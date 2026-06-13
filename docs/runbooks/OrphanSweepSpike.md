# Runbook: OrphanSweepSpike

## Signal

`sum(rate(mitos_orphan_sweeps_total[15m])) > 0` sustained for 15m.

`mitos_orphan_sweeps_total` is a controller counter incremented once per
forkd VM reaped by the garbage collector's orphan sweep (a VM with no owning
claim). Steady reaping is a symptom: VMs are leaking and the GC is cleaning them
up. The threshold is environment-tunable; a brief blip after a controller
restart is expected, sustained reaping is not.

## Likely causes

- Controller / forkd state disagreement: claims deleted while their VM lived on.
- Claims crashing or timing out after the VM was created but before the owner
  reference was set.
- Node churn (drain, eviction) leaving VMs behind that the GC then reaps.
- A bug in the claim teardown path leaking VMs.

## Diagnosis

- `kubectl sandbox ps` to compare live VMs per node against known claims; orphans
  are VMs with no matching SandboxClaim.
- `kubectl sandbox ls` to confirm the claim inventory.
- `kubectl sandbox top` for node churn / pressure correlation.
- Metrics: `mitos_orphan_sweeps_total` (the rate driving this alert),
  `mitos_active_sandboxes` per node (does it match expected claims?),
  `mitos_claim_errors_total` (are claims failing mid-create and leaking?).
- Controller GC logs and forkd logs around the sweep times (counts only, no
  secrets).

## Remediation

- If correlated with node drains/evictions, the sweep is doing its job; confirm
  no VMs persist on the lost nodes.
- If correlated with claim crashes, fix the claim teardown / owner-reference
  path so VMs are torn down with their claim.
- A persistent nonzero rate with no node churn indicates a leak bug: capture the
  orphan VM ids and node and open an issue.

## Escalation

Sustained reaping with no node-churn explanation is a fork-lifecycle leak.
Escalate to the `internal/daemon` / `internal/fork` on-call (VM lifecycle,
security-sensitive); leaked VMs are a resource and isolation concern.
