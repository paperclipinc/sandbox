# Volume Fork Policies Implementation Plan (issue #11)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `forkPolicy` real for the two policies that matter most and appear in every README example: `Fresh` (a new empty volume, scratch space) and `Snapshot` (a CoW copy of the working directory, cheap and independent per fork). Volumes become real block devices attached to the Firecracker VM and mounted in the guest at their `mountPath`. Snapshot uses reflink (cp --reflink, works on btrfs and xfs) for instant CoW so a write in a fork does not touch the source. Verified in KVM CI on a loopback reflink-capable filesystem: a Fresh volume round-trips a write, and a Snapshot fork proves CoW independence from its source. `Share` (read-only shared attach) and `Clone` (full copy) get a working-but-partial treatment and the harder cases are documented open.

**Honesty constraint:** today the volume handlers are no-ops and the claim reconciler discards the prepared volumes (`_ = volumes`); the README volume table is currently fiction. This PR makes Fresh and Snapshot genuinely work end to end (proven in CI) and the docs state precisely which policies are enforced and which remain partial.

**Architecture:** The node (forkd) owns volume backings under `<dataDir>/sandboxes/<id>/volumes/<name>.ext4`. A new `internal/volume` package creates them per policy: Fresh formats a new empty ext4 of the template size; Snapshot reflink-copies the source volume backing (instant CoW) and falls back to a full copy with a logged warning when the filesystem lacks reflink. Because Firecracker bakes its device model at snapshot time, the TEMPLATE is booted with one placeholder volume drive per template volume so the snapshot has the block devices; each fork prepares its own backing and rebinds the drive on restore (investigate the exact Firecracker v1.15 mechanism: drive override on `/snapshot/load` if available, else `PATCH /drives/{id}` after resume). The guest agent receives a volume mount table (drive node to mountPath, read-only flag) via the existing post-restore vsock handshake and mounts each device. Teardown removes the backings and frees nothing else.

**Context for the implementer:**
- API: `api/v1alpha1/types.go` SandboxVolume{Name, Size, Source, ReadOnly, MountPath, ForkPolicy}; ForkPolicy consts Fresh/Share/Clone/Snapshot. The claim reconciler computes `prepareVolumes` then discards it (`sandboxclaim_controller.go:104 _ = volumes`); same in the fork reconciler. `internal/workspace/policies.go` has the no-op handlers (controller-side host-path guesses); this PR moves the real backend work to forkd-side `internal/volume` and keeps the controller responsible only for passing the policy + volume spec to forkd.
- Firecracker: `internal/firecracker/client.go` has `AddDrive(driveID, path, readOnly, rootDevice)` and the HTTP client; add `PatchDrive(driveID, path)` (PATCH /drives/{id}) and/or a drive override on LoadSnapshot per the investigation. `internal/firecracker/template.go` CreateTemplate boots+snapshots; it must add placeholder volume drives. `internal/fork/engine.go` Fork restores and would rebind drives + prepare backings.
- Guest agent: `guest/agent/main.go` handles vsock messages (configure, notify_forked). Add a volume-mount step: given a mount table, `mount /dev/vdX <mountPath>` (mkdir -p the mountPath first; the volume drives enumerate after the rootfs as /dev/vdb, /dev/vdc... in attach order). `internal/vsock` carries the table.
- Proto: `proto/forkd.proto` ForkRequest has NO volume field; add repeated VolumeMount. Regen with `make proto`.
- Conventions: CLAUDE.md authoritative. No em/en dashes. TDD. Explicit-path git add. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Lint darwin + GOOS=linux. ids/paths safe to log.

---

### Task 1: `internal/volume` node-side backend (Fresh, Snapshot/reflink)

**Files:** Create `internal/volume/backend.go`, `internal/volume/reflink.go`, and `_test.go`.

- [ ] `type Spec{ Name string; SizeMB int; MountPath string; ReadOnly bool; Policy ForkPolicy }` and `type Prepared{ Name string; HostPath string; MountPath string; ReadOnly bool }`. `type Backend struct{ root string; runner func(argv []string) error }` (injectable runner for tests).
- [ ] `Fresh(spec, sandboxID) (Prepared, error)`: path `<root>/sandboxes/<id>/volumes/<name>.ext4`; `mkfs.ext4 -F -q <path> <sizeMB>M` via runner (after truncating/creating the file). 
- [ ] `Snapshot(spec, sandboxID, sourcePath) (Prepared, error)`: reflink-copy sourcePath to the per-fork path via `cp --reflink=auto <src> <dst>` (auto falls back to a full copy on non-reflink filesystems; detect and log a warning when a reflink could not be made by checking with `--reflink=always` first and falling back). Document that reflink gives instant CoW on btrfs/xfs and a full copy elsewhere.
- [ ] `Share(spec, sandboxID, sourcePath) (Prepared, error)`: returns the SOURCE path with ReadOnly true (all forks attach the same backing read-only; no copy). `Clone(spec, sandboxID, sourcePath)`: full copy (cp) for now. 
- [ ] `Cleanup(sandboxID) error`: remove `<root>/sandboxes/<id>/volumes`.
- [ ] `DefaultSizeMB` and a parse of the API `Size` string (e.g. "5Gi") to MB (reuse k8s resource.Quantity parsing).
- [ ] TDD with a recording runner: Fresh issues the mkfs argv with the right size and path; Snapshot issues cp --reflink and returns the per-fork path distinct from the source; Share returns the source path read-only with no copy; size parsing ("5Gi" -> 5120, default when empty); Cleanup removes the dir. No real mkfs/cp in unit tests (recording runner).
- [ ] Commit `feat: internal/volume node backend with Fresh and reflink Snapshot policies`.

### Task 2: proto VolumeMount + controller plumbs template volumes to forkd

**Files:** `proto/forkd.proto` (+ regen), `internal/daemon/grpc_service.go`, `internal/daemon/server.go`, `internal/fork/engine.go` (ForkOpts gains volumes), `internal/controller/forkd_client.go`, `internal/controller/sandboxclaim_controller.go` + `sandboxfork_controller.go`, tests.

- [ ] Proto: `ForkRequest` gains `repeated VolumeMount volumes = 7;` with `message VolumeMount{ string name=1; string mount_path=2; bool read_only=3; string fork_policy=4; string size=5; }`. Regen, commit generated code. (The backing source for Snapshot is the template volume, resolved node-side; the controller passes the policy + spec, forkd does the backend work.)
- [ ] Controller: stop discarding volumes. The claim reconciler resolves template.Spec.Volumes with VolumeOverrides into the Fork RPC VolumeMounts (name, mountPath, readOnly, policy, size) and passes them through forkOnNode. Same for the fork reconciler. Remove the `_ = volumes` discard; the workspace.prepareVolumes host-path guessing is superseded by forkd-side preparation, so simplify the controller to just translate spec->RPC (keep or remove internal/workspace as appropriate; if removed, delete its now-dead code, else leave a thin policy-to-RPC mapping).
- [ ] forkd: ForkOpts gains `Volumes []volume.Spec`; Server.Fork parses the proto VolumeMounts into volume.Specs and passes them to the engine. (Actual attach is Task 3.)
- [ ] TDD (envtest): a claim from a template with a Fresh volume and a Snapshot volume reaches Ready and the fake forkd recorded the VolumeMounts (name, mountPath, policy). Extend the fake to record the last fork's volumes.
- [ ] Commit `feat: plumb template volumes and fork policies through to forkd`.

### Task 3: Firecracker drive attach, placeholder drives at template, rebind on fork

**Files:** `internal/firecracker/client.go` (PatchDrive / load override), `internal/firecracker/template.go`, `internal/fork/engine.go`, `cmd/forkd/main.go`, tests.

- [ ] CRITICAL FIRST: determine the Firecracker v1.15 mechanism for giving each fork its own volume backing. Two candidates: (a) a drive override on `PUT /snapshot/load` (analogous to network_overrides), if v1.15 supports it; (b) `PATCH /drives/{drive_id}` with a new `path_on_host` after restore+resume. Verify which works (PATCH /drives path update is long-supported; confirm it works on a restored VM before the guest mounts). Implement the working one. Document the decision and confidence; escalate (NEEDS_CONTEXT) if genuinely ambiguous rather than guessing.
- [ ] Template build (template.go CreateTemplate): for each template volume, create a placeholder ext4 backing (empty, correct size) and AddDrive it (read-only false, not root) BEFORE InstanceStart, so the snapshot bakes the block devices. The guest does NOT mount them at build time. Snapshot as today. Record the drive ids and their order (drive id -> volume name) so forks can rebind.
- [ ] Fork (engine.go): for each volume, prepare its backing via internal/volume per policy (Fresh -> new ext4; Snapshot -> reflink of the template volume's backing; the source for Snapshot is the template's placeholder-or-seed backing, so the template volume should be SEEDED with the volume's initial content at build time if any, else empty), then rebind the fork's drive to that backing (load override or PATCH). On Terminate, volume.Cleanup.
- [ ] forkd flags: `--enable-volumes` (default false until proven). EngineOpts plumbed with the volume.Backend.
- [ ] TDD (no KVM): a test seaming the FC client and the volume backend asserting Fork prepares a distinct backing per volume per fork and rebinds the right drive id; networking/cas-style fake. Distinct backings per fork (two forks do not share a Fresh backing; Snapshot forks get distinct reflink copies).
- [ ] Commit `feat: attach volume drives, placeholder at snapshot, rebind per fork`.

### Task 4: guest mounts the volume drives

**Files:** `internal/vsock/protocol.go` + `client.go`, `guest/agent/main.go`, `internal/daemon/server.go` + `sandbox_api.go`, tests.

- [ ] Extend the post-restore handshake (Configure or NotifyForked) with a volume mount table: `[]VolumeMountEntry{ Device string (e.g. /dev/vdb); MountPath string; ReadOnly bool }`. forkd computes the device names from the drive attach order (rootfs is /dev/vda, volumes follow as vdb, vdc...) and delivers the table after rebinding the drives.
- [ ] Guest agent: on receipt, for each entry mkdir -p MountPath and `mount [-o ro] <device> <mountPath>` (syscall.Mount); log counts, surface mount failures. Make it idempotent (skip if already mounted).
- [ ] Client method + a fake-agent test asserting the agent received and applied the table (record the calls).
- [ ] Commit `feat: guest mounts attached volume drives at their mount paths`.

### Task 5: KVM CI proof + docs + PR

**Files:** `.github/workflows/kvm-test.yaml`, `docs/volumes.md` (new), `README.md` (volume table honesty), `ROADMAP.md`, full verification.

- [ ] KVM CI phase (on a loopback reflink-capable fs, e.g. create a btrfs or xfs-with-reflink loop file mounted at the volume root so reflink works on the runner): build a template with a Fresh volume and a Snapshot volume (seed the snapshot source with a known file at template build), fork TWO sandboxes. Assert: (Fresh) guest writes a file to the Fresh volume and reads it back; (Snapshot) each fork sees the seeded file, writes a fork-unique file, and the SOURCE backing is unchanged and the two forks do not see each other's writes (CoW independence). Drive this via a small helper (extend cmd/tmpl-smoke or a new cmd/vol-smoke that uses the engine with --enable-volumes). Gate on success.
- [ ] `docs/volumes.md`: the per-policy backend (Fresh new ext4, Snapshot reflink CoW, Share read-only shared attach, Clone full copy), the placeholder-drive-at-snapshot + rebind-on-fork mechanism, guest mounting, what is ENFORCED and CI-proven (Fresh round-trip, Snapshot CoW independence) vs PARTIAL/OPEN (Share/Clone tested less; external volume sources S3/GCS/PVC/Git not yet materialized; reflink requires a reflink-capable fs and falls back to a full copy with a warning otherwise; per-fork size changes; CSI integration).
- [ ] README volume table: update so it states which policies are enforced (Fresh, Snapshot CoW) and which are partial, rather than implying all four fully work. Keep the change scoped to the volume table.
- [ ] ROADMAP section 0: flip "Volume fork policies actually attach volumes to VMs" to done for Fresh+Snapshot with the partials noted.
- [ ] Full verification (build darwin + GOOS=linux, vet, lint both, all Go suites with envtest, Python suite, gofmt zero, dash grep zero, YAML parse, proto/CRD regen committed).
- [ ] Push `feat/volume-policies`, PR `Volume fork policies: Fresh and CoW Snapshot volumes attached and mounted` body Closes-or-advances #11 (Fresh+Snapshot done, Share/Clone partial, external sources open), watch CI, dismiss guarded CodeQL alerts with justification if any, merge when green.

**Out of scope (follow-ups):** external volume sources (S3/GCS/PVC/Git materialization); CSI VolumeSnapshot/clone integration for Clone; per-fork resize; virtio-fs shared directories (vs block drives); reflink on filesystems without it (documented full-copy fallback); Share concurrent-writer safety (read-only only for now).
