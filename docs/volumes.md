# Volumes and fork policies

Volumes are real block devices attached to a sandbox's Firecracker microVM and
mounted in the guest at their `mountPath`. Each volume declares a `forkPolicy`
that decides how its backing image is prepared for every fork. This document
states which policies are enforced and CI-proven and which remain partial or
open, per the no-unverified-claims rule.

Volumes are opt-in: forkd attaches and mounts them only when started with
`--enable-volumes`. With volumes disabled the engine behaves exactly as before
(no drives attached, no backings prepared).

## Per-policy backend

The node (forkd) owns the volume backings under its data dir. The
`internal/volume` backend prepares one backing file per fork per policy:

| Policy     | Backend preparation                                            | Read-only at drive | Per-fork backing |
| ---------- | -------------------------------------------------------------- | ------------------ | ---------------- |
| `Fresh`    | format a new empty ext4 of the volume size                     | no                 | yes (distinct)   |
| `Snapshot` | `cp --reflink=always` of the template seed (instant CoW)       | no                 | yes (distinct)   |
| `Share`    | attach the template seed with no copy                          | yes (forced)       | no (shared seed) |
| `Clone`    | full byte-for-byte `cp` of the template seed                   | no                 | yes (distinct)   |

`Snapshot` reflink gives instant copy-on-write on reflink-capable filesystems
(btrfs and xfs with reflink=1). On a filesystem without reflink the backend logs
a warning and falls back to `cp --reflink=auto`, which performs a full copy
(correct, but not instant and not space-shared).

A volume's drive is read-only at the BLOCK-DEVICE level iff `spec.readOnly` is
true OR the resolved policy is `Share`. `Share` attaches the same seed backing to
every fork, so a writable drive would let one fork corrupt or leak into the seed
and every sibling fork; the drive is therefore baked read-only. `Fresh`,
`Snapshot`, and `Clone` forks each get their own backing and stay writable unless
explicitly marked read-only.

## Placeholder drive at snapshot, PATCH rebind on fork

Firecracker bakes its device model at snapshot time and cannot add a drive on
restore. So the TEMPLATE build attaches one PLACEHOLDER volume drive per template
volume (drive id = volume name) BEFORE the instance starts, and the snapshot
bakes those block devices. The placeholder backing is the template seed image at
`<dataDir>/templates/<id>/volumes/<name>.ext4` (an empty ext4 of the volume size,
or a seeded image). The `is_read_only` flag is baked from the resolved policy at
this point, because `PATCH /drives` cannot flip it later.

On each fork, the engine:

1. prepares this fork's own backing per the policy (Fresh -> new ext4; Snapshot
   -> reflink of the seed; Share -> the seed read-only; Clone -> full copy);
2. loads the snapshot (network overrides remap the NIC, as for networking);
3. rebinds each baked placeholder drive to this fork's backing with
   `PATCH /drives/{drive_id}` (updating `path_on_host`), so every fork points its
   drive at its OWN backing before the guest mounts it.

`PATCH /drives` path update is supported on a restored and resumed VM in
Firecracker v1.15 (the pinned CI version) and is the mechanism used here.

## Guest mounting and the device-order convention

After the drives are rebound, forkd delivers a per-fork volume mount table to the
guest agent in the post-restore `notify_forked` vsock message
(`[]VolumeMountEntry{Device, MountPath, ReadOnly}`). The guest agent, for each
entry, `mkdir -p`s the mount path and mounts the device ext4 (read-only with
`MS_RDONLY` when `ReadOnly`). It is idempotent: an already-mounted path (checked
against `/proc/mounts`) is skipped, so a re-delivered notification does not
double-mount. Per-entry failures are logged and the rest still mount.

Device names follow Firecracker's drive attach order: the rootfs is `/dev/vda`
and the i-th volume drive (0-based, in template volume order) is `/dev/vd{b+i}`.
So two volumes attach as `/dev/vdb` and `/dev/vdc`. The engine computes these
names from the rebind order and the controller does not need to know them.

## Enforced and CI-proven

The KVM CI (`.github/workflows/kvm-test.yaml`, "Per-fork volumes" phase) runs on
a btrfs loopback filesystem (reflink-capable) and drives the real engine via
`cmd/vol-smoke` with `--enable-volumes`. It builds a template with a `Fresh` and
a `Snapshot` volume (the Snapshot seed pre-written host-side with `/seeded.txt`),
forks TWO sandboxes, and asserts over the guest agent:

- Fresh round-trip: fork1 writes a file to its `Fresh` volume mount path and
  reads it back, proving the volume is mounted writable.
- Snapshot CoW independence: both forks see the seeded file; fork1 writes a
  fork-unique file to its `Snapshot` volume; fork2 does NOT see it AND the
  template seed image on the host is byte-for-byte unchanged, proving the reflink
  copies diverge (true copy-on-write, no shared backing).
- Read-only Share: a write to the read-only `Share` volume is refused in the
  guest, proving the drive is baked read-only.

## Partial and open

- `Share` and `Clone` are exercised less than `Fresh`/`Snapshot`; `Share` is
  read-only only (concurrent-writer safety is out of scope).
- External volume sources (`source.s3`, `source.gcs`, `source.pvc`,
  `source.git`) are NOT yet materialized into backings; only the node-prepared
  ext4 backings above exist today.
- CSI VolumeSnapshot / clone integration for `Clone` is open; `Clone` is a host
  full copy for now.
- `Snapshot` reflink requires a reflink-capable filesystem (btrfs, xfs with
  reflink=1). On other filesystems it falls back to a full copy with a logged
  warning.
- virtio-fs shared directories (versus block drives) are out of scope.
- Per-fork volume resize is open.
