# ADR 0005: raw-forkd is not for untrusted multi-tenant

Status: accepted (2026-06-15)
Issue: #18 (W1 husk pods, husk-as-default), #30 (residual ADRs). Related:
docs/threat-model.md section 0 (the build-vs-run split, the per-axis tally),
section 1 (jailer row), section 2 (shared-rootfs row), section 3 (forkd
capability-minimization row), section 6 (fork-correctness reseed row); the code is
`cmd/forkd`, `internal/fork/engine.go`, `deploy/daemon/daemonset.yaml`,
`internal/controller` (the `--enable-husk-pods` / `--enable-raw-forkd` gating);
docs/husk-pods.md.

## Context

mitos has two per-sandbox execution engines:

- The HUSK pod (`cmd/husk-stub`): the default. One unprivileged, capability-
  dropped, PSA-restricted-minus-two pod per VM (ADR 0003).
- raw-forkd (`--enable-raw-forkd`): the older engine, where forkd itself forks a
  VM per claim. As SHIPPED, forkd is a root DaemonSet pod with `privileged: true`
  and `/dev/kvm`, and the jailer is DISABLED in the shipped DaemonSet
  (`deploy/daemon/daemonset.yaml`: the `--jailer`/`--chroot-base`/`--uid-range`
  flags are commented out). So a guest escape from a raw-forkd VM lands as ROOT in
  a PRIVILEGED container with `/dev/kvm` and a hostPath to the node data dir:
  materially full node compromise (docs/threat-model.md section 3, forkd
  capability-minimization row, status open/high).

The control gating moved: pod-native execution is now the DEFAULT (the controller
runs `--enable-husk-pods` by default; `--enable-raw-forkd` and `--mock` select
the fork-per-claim fallback) (docs/threat-model.md section 0; ROADMAP.md W1).
That gating change is what makes this decision recordable: raw-forkd is no longer
the default tenant surface, it is an opt-in fallback the operator must explicitly
enable.

raw-forkd carries several hazards the husk path has fixed or does not have:

- It runs `privileged: true` with the jailer disabled as shipped, so a VMM
  compromise is forkd's root (docs/threat-model.md section 1 jailer row, section
  3 forkd row).
- All forks of one template on a node share a SINGLE writable rootfs inode (a
  cross-fork filesystem read/write channel and corruption vector); the husk path
  fixed this with a per-pod reflink rootfs clone rebound via `PatchDrive`, the
  raw-forkd path did NOT (docs/threat-model.md section 2, status open/critical for
  raw-forkd).
- Fork-correctness is NOT fail-closed on raw-forkd: a guest that connected but
  silently did not reseed its CRNG serves duplicate keys/tokens/UUIDs across
  forks, because the reseed response is discarded; the husk path IS fail-closed
  (docs/threat-model.md section 6, docs/fork-correctness.md row 1, status
  open/critical for raw-forkd).

## Decision: the husk pod is the default tenant runner; raw-forkd is an opt-in privileged fallback, not for untrusted multi-tenant use

We record that raw-forkd is NOT a safe runner for untrusted multi-tenant
workloads, and that the husk pod is the engine to use for that case. Concretely:

- The DEFAULT is husk pods (`--enable-husk-pods` default on). raw-forkd is
  reachable only by explicitly setting `--enable-raw-forkd` (or `--mock`, the
  dev/no-KVM path, which implies it). The operator must opt INTO raw-forkd; it is
  not what ships enabled (docs/threat-model.md section 0; docs/husk-pods.md).
- raw-forkd is documented as the fallback engine for cases that do NOT need the
  untrusted-multi-tenant posture (single-tenant, trusted-workload, or dev/no-KVM
  use), or as a transitional path. Its known open/critical and open/high hazards
  (privileged-no-jailer DaemonSet, shared writable rootfs inode, non-fail-closed
  fork reseed) are exactly why it is excluded from the untrusted-multi-tenant
  recommendation (docs/threat-model.md section 0 must-fix-first set, sections 1,
  2, 3, 6).
- forkd-the-BUILDER stays privileged regardless of engine: building a template
  snapshot needs `/dev/kvm` and the jailer, so forkd remains the privileged
  per-node BUILDER even when husk pods are the runner. The build path runs once
  per node per template, not per sandbox, so the privileged surface is amortized
  and confined to building, not to executing tenant code (docs/threat-model.md
  section 0, the per-axis tally). This ADR distinguishes forkd-the-builder (a
  smaller, amortized control-plane surface that stays) from raw-forkd-the-runner
  (the per-sandbox engine the husk default replaces).

## Why not keep raw-forkd as a co-equal runner

- On the privilege and capabilities axes the husk model is strictly better: an
  unprivileged, drop-ALL, no-escalation, PSA-restricted-minus-two pod versus a
  root privileged container (docs/threat-model.md section 0 per-axis tally).
  Offering raw-forkd as a co-equal default would re-expose the worse surface as a
  silent option.
- raw-forkd's shared-rootfs and non-fail-closed-reseed hazards are
  cross-tenant-affecting (a fork reads/writes a sibling's filesystem; duplicate
  CRNG output across forks). These are not acceptable under an untrusted-multi-
  tenant claim, and fixing them on raw-forkd is tracked but not done
  (docs/threat-model.md sections 2 and 6 "required fix" notes).

## Consequences

- The honest claim is: the untrusted-multi-tenant tenant execution surface is the
  husk pod; raw-forkd is an opt-in fallback that is not for untrusted multi-tenant
  use. docs/compliance-claims.md and the README must not present raw-forkd as a
  safe multi-tenant runner.
- An operator who enables `--enable-raw-forkd` is opting into the documented
  open/critical and open/high hazards above; the gating makes that an explicit
  choice, not a default.
- Lifting raw-forkd to a safe runner is tracked, not abandoned: enable the jailer
  in-pod and drop `privileged` (issue #2 Task 5; docs/threat-model.md section 1
  and section 3), add the per-fork rootfs clone + `PatchDrive` mirroring the husk
  fix (section 2), and make the fork reseed fail-closed on raw-forkd as it is on
  husk (section 6). Until all three close, the not-for-untrusted-multi-tenant
  statement stands.
- forkd-the-builder's residual privilege is accepted separately and is out of
  scope for this ADR; a builder redesign that removes its privilege is tracked but
  not planned here (docs/threat-model.md section 0 accepted-residuals list).
