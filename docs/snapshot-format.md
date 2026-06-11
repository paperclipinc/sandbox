# Snapshot format and compatibility

A snapshot is an executable memory image plus device state. Restoring one is
equivalent to resuming a process: it is only safe on a host whose Firecracker
version, CPU model, and snapshot format match the host that produced it.
Restoring across an incompatible boundary is a crash or silent-corruption
hazard. This document defines the snapshot format version, what the manifest
records, the compatibility policy, the failure behavior, and the migration
policy.

Related: docs/snapshot-distribution.md (the content-addressed store and
verify-on-load), docs/fork-correctness.md (hazard 6), docs/threat-model.md
(snapshot integrity controls).

## Format version

`cas.CurrentSnapshotFormatVersion` is the snapshot format version this build
produces and can restore. It is currently `1`. The engine stamps it into every
manifest at template build and checks it on load.

The format version covers the on-disk snapshot layout and the restore contract:
how the memory file, the VM state file, and the manifest relate, and what
assumptions the engine makes when resuming. Bump it whenever any of those
change incompatibly. A bump is a deliberate, reviewed event, not an incidental
side effect of a refactor.

## What the manifest records

The producing environment is part of a snapshot's identity. The manifest
(`internal/cas.Manifest`) records these fields, all of which are part of the
content-addressed digest:

| Field | Meaning |
|---|---|
| `SnapshotFormatVersion` | The format version above (current = 1). |
| `VMMVersion` | The Firecracker (VMM) version that produced the snapshot. |
| `CPUModel` | The host CPU model the snapshot was captured on. |
| `KernelVersion` | The host kernel at capture (informational). |
| `ConfigHash` | A sha256 over the microvm machine config (vcpu count, memory size, kernel and rootfs identity) the snapshot was captured under. |

Because these fields are part of the digest, a snapshot built under a different
Firecracker or on a different CPU never collides with one built here, and the
recorded environment cannot be tampered with or downgraded without changing the
digest and failing the verify-on-load integrity check
(docs/snapshot-distribution.md).

The detected host environment is captured once at engine start
(`snapcompat.DetectEnvironment`): the Firecracker version from
`firecracker --version`, the CPU model from the first `model name` line of
`/proc/cpuinfo`, and the kernel from `uname -r`. A failure to detect any field
is surfaced rather than silently swallowed, so the engine can refuse to start
with an unknown environment instead of running blind.

## Compatibility policy

`snapcompat.Check(manifest, environment)` decides whether a snapshot may be
restored on this host. The policy, checked in order:

1. **Format version** must be in the set this build supports (currently the
   single value `cas.CurrentSnapshotFormatVersion`). A format-version 0 manifest
   predates the contract and is refused.
2. **Firecracker version** must match exactly. Firecracker does not guarantee
   cross-version snapshot restore.
3. **CPU model** must match exactly. Cross-CPU-model restore is unsafe without a
   CPU template.
4. **Kernel version** is informational. The guest kernel is baked into the
   snapshot image itself, so a recorded kernel mismatch usually just means a
   different snapshot was produced, not that this one is unsafe to restore here.
   The contract does not gate on it alone.

The first mismatch found is the one reported.

## Failure behavior

The compatibility check is part of the load gate. It runs **after** the
content-addressed digest verify and **before** any Firecracker launch, so an
incompatible snapshot never reaches a running VM. On a mismatch the engine
refuses to fork and returns an actionable error that names the mismatch and the
remediation: rebuild the template on this node, pin the node to the producing
Firecracker version, or schedule the fork onto a node with a matching CPU.

`--allow-incompatible-snapshots` (forkd) is a development escape hatch. It
downgrades the refusal to a loud one-time warning and proceeds. It is for
development only and must never be set for tenant workloads.

## Migration and deprecation policy

- The format version is bumped only for an incompatible change to the snapshot
  layout or restore contract.
- A bump requires rebuilding templates: a snapshot recorded under an old format
  version is refused on load by a build that no longer lists that version in its
  supported set. There is no in-place migration of an existing snapshot today;
  the producing pipeline rebuilds the template at the new version.
- The supported set is `cas.CurrentSnapshotFormatVersion` today (a single
  version). When a future build needs to restore more than one version during a
  transition window, it lists every version it supports in
  `Environment.FormatVersions`; the policy already checks set membership, so
  widening the window is a one-line change plus the restore code to honor it.
- A format-version migration tool (to upgrade recorded snapshots in place rather
  than rebuilding) is a future follow-up, tracked with the other format-freeze
  work.

## Proof

- Unit: `internal/snapcompat` checks each mismatch and the informational kernel
  case; `internal/fork/compat_test.go` proves the engine refuses an incompatible
  snapshot on load and that `--allow-incompatible-snapshots` overrides it.
- KVM CI (`.github/workflows/kvm-test.yaml`): records a manifest stamped with
  the runner's real producing environment and asserts it is compatible with the
  node, then rewrites the recorded Firecracker version and the format version and
  asserts each is refused with a message naming the mismatch. This proves the
  contract check on a real Firecracker-produced manifest. It is honestly not a
  live cross-Firecracker-version restore, which would need two Firecracker
  versions on the runner.

## Open

- Firecracker CPU templates, to allow cross-CPU-model restore within a defined
  CPU family (would relax the exact-CPU-model rule).
- Live cross-Firecracker-version restore testing (needs two Firecracker versions
  in CI).

Both are out of scope for the contract as shipped; the contract refuses them
today rather than risking an unsafe restore.
