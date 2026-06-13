# Runbook: ClaimErrorRateHigh

## Signal

`sum by (pool, reason) (rate(mitos_claim_errors_total[5m])) > 0.05`
sustained for 10m.

`mitos_claim_errors_total{pool,reason}` is a controller counter of TERMINAL
claim failures. The `reason` label is a fixed, coarse code (never error text):
`fork`, `secret`, `volume`, `token`. A sustained nonzero rate means claims are
failing for a structural reason, not transient requeues. The threshold is
environment-tunable; set it from the observed baseline.

## Likely causes

- `reason="fork"`: forkd or the engine is rejecting forks (KVM unavailable on
  the holder node, snapshot missing or corrupt, restore failure).
- `reason="secret"`: a referenced Secret is missing, or secret inheritance was
  not opted into on a fork (see the `SecretInheritanceDenied` reason in
  `docs/conditions.md`).
- `reason="volume"`: a referenced volume seed is missing or unreadable.
- `reason="token"`: attenuated-token minting or validation is failing.

## Diagnosis

- Identify the failing pool and reason from the alert labels.
- `kubectl sandbox ls` to list claims and their phase; look for claims stuck off
  Ready in the named pool.
- `kubectl sandbox ps` for the per-node sandbox view to spot a node whose forks
  fail.
- Check the SandboxClaim `Ready` condition reason against the catalogue in
  `docs/conditions.md` (for example `ActivateFailed`, `CapacityExhausted`,
  `NoHuskPod`, `SecretInheritanceDenied`).
- Metrics to corroborate: `mitos_claim_errors_total` (split by reason),
  `mitos_pool_ready_snapshots{pool}` (is the pool also starved?),
  `mitos_fork_duration_seconds_bucket` (are forks slow or failing?).
- Controller and forkd logs for the named pool and node (no secret values are
  logged; reasons and counts only).

## Remediation

- `reason="fork"`: verify KVM is healthy on the holder nodes; rebuild the pool
  snapshot if it is corrupt.
- `reason="secret"`: restore the missing Secret, or set explicit secret
  inheritance opt-in on the fork.
- `reason="volume"`: restore or re-seed the volume.
- `reason="token"`: check the token-minting path and PKI material.

## Escalation

If the reason is `fork` or `token` and the cause is not an obvious missing
resource, escalate to the on-call owner for `internal/fork` /
`internal/firecracker` (fork path) or the token/attenuation owner. These are
security-sensitive paths; do not hot-patch without the named reviewer.
