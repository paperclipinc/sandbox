# Encryption at Rest with Crypto-Shredding Implementation Plan (issue #31)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Advance issue #31, the last format-freeze blocker. Encrypt snapshot and volume backing at rest with a per-scope key and make deletion of that key a crypto-shred: once the key is gone, the data is mathematically unrecoverable. The design must NOT sacrifice copy-on-write density, which rules out application-level encryption of the mem file (it would force a private decrypt per fork and destroy page-cache sharing). Instead the encryption is at the block layer (dm-crypt/LUKS): a per-template-scope LUKS container holds that template's snapshot and seed volumes; once opened and mounted, reads decrypt into the page cache, so `mmap(MAP_PRIVATE)` of the snapshot mem file still shares the decrypted pages across forks exactly as today. Per-scope containers (one key each) are what make per-scope crypto-shredding possible. Verified in KVM CI: the raw backing device holds ciphertext (the plaintext snapshot is absent), a fork restores and execs through the decrypted mount, and after a crypto-shred (LUKS keyslot erase plus key destruction) the data cannot be recovered.

**Scope of THIS PR (PR1):** the node-side mechanism. The key is held only in memory on the node while a container is open and zeroized on close; crypto-shred wipes the LUKS keyslots and destroys the in-memory key. Production key CUSTODY (the key living in a k8s Secret or a KMS, never persisted on the node data disk, distributed per scope, and the controller-driven shred lifecycle) is the explicit PR2 follow-up, documented here. PR1 proves encryption-at-rest, CoW-preservation, and the crypto-shred mechanism; PR2 hardens where the key lives. The scope is per-template for now (the directive says per-workspace; the Workspace primitive #21 is unbuilt, so the scope id is a parameter that becomes the workspace id when #21 lands).

**Honesty constraint:** the threat model is updated in the same PR. We state precisely: data at rest in an encrypted container is ciphertext; crypto-shred (cryptsetup luksErase plus key destruction) renders it unrecoverable; CoW sharing is preserved because encryption is below the page cache. We also state the PR1 limitation honestly: the key is held in node memory during operation and (in PR1) supplied from a node-reachable source, so PR1's crypto-shred guarantee holds against disk-recovery of a closed container but full key-custody hardening (key never on the node disk, per-scope KMS) is PR2. No claim that data is encrypted that a CI test does not show is ciphertext on the raw device.

**Architecture:** A new `internal/storecrypt` package manages per-scope LUKS containers: `Create(scopeID, key, sizeBytes)` allocates a backing file under `<dataDir>/enc/<scopeID>.img`, `cryptsetup luksFormat` with the key, `luksOpen` to `/dev/mapper/mitos-<scopeID>`, `mkfs.ext4`, and mounts it at the scope's plaintext mount point (the template dir). `Open`/`Close` (mount/umount + luksClose, zeroizing the in-memory key). `Shred(scopeID)` does `cryptsetup luksErase` (wipe all keyslots) then removes the backing file and destroys the key, so the ciphertext is unrecoverable. All cryptsetup/mount/losetup calls go through an injected `runner` so the package is unit-tested on darwin by asserting the argv; real execution happens only on the KVM runner. The engine, when encryption is enabled, creates a container per template, writes the snapshot and seed volumes inside the mounted dir, restores forks from it (CoW preserved), and crypto-shreds on template delete/GC.

**Context for the implementer:**
- Storage layout: snapshots at `<dataDir>/templates/<id>/{snapshot/{mem,vmstate},rootfs.ext4}`, seed volumes at `<dataDir>/templates/<id>/volumes/<name>.ext4`, per-sandbox at `<dataDir>/sandboxes/<id>/`, CAS at `<dataDir>/cas` (`internal/fork/engine.go`). The encrypted container for a template scope should mount AT or hold the template dir so the existing snapshot/volume paths work unchanged once mounted.
- `internal/fork/engine.go`: CreateTemplate writes the snapshot + seed volumes; Fork reads `<dataDir>/templates/<snapshotID>/snapshot/{mem,vmstate}` and mmaps mem (the CoW path); template lifecycle/GC. EngineOpts + NewEngine. The CoW mmap of mem must come from the MOUNTED (decrypted) path.
- `internal/volume`: seed volumes live under the template dir; they ride inside the same per-template container.
- cryptsetup/dm-crypt: needs root, loop devices, and /dev/mapper (present on the KVM runner, NOT on the kind/go-test runners, hence the injected-runner unit testing). LUKS2 default; key passed via stdin (`--key-file -`) so it is never an argv leak.
- KVM CI verify-on-load / integrity precedent in `.github/workflows/kvm-test.yaml` shows raw-byte assertions on snapshot files; mirror for the ciphertext assertion.
- Conventions: CLAUDE.md authoritative. No em/en dashes. TDD. Explicit-path git add. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Lint darwin + GOOS=linux. Do not regress go 1.24. The key is a secret VALUE: never logged, never in argv (use --key-file -/stdin), never in error messages.

---

### Task 1: `internal/storecrypt` LUKS container lifecycle

**Files:** Create `internal/storecrypt/container.go`, `internal/storecrypt/key.go`, and `_test.go`.

- [ ] `key.go`: `type Key []byte` (256-bit), `NewKey() (Key, error)` (crypto/rand), `(Key) Zeroize()`. The key is never logged or formatted; a String()/marshal returns a redacted placeholder. 
- [ ] `container.go`: `type Manager struct { root string; runner func(ctx, argv []string, stdin []byte) error; mountBase string }` (runner injected; stdin carries the key so it never hits argv). Methods:
  - `Create(ctx, scopeID string, key Key, sizeBytes int64, mountPoint string) error`: validate scopeID (allowlist `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`, no traversal); fallocate `<root>/enc/<scopeID>.img` to sizeBytes; `cryptsetup luksFormat --type luks2 --key-file - <img>` (key on stdin); `cryptsetup luksOpen --key-file - <img> mitos-<scopeID>`; `mkfs.ext4 /dev/mapper/mitos-<scopeID>`; `mount /dev/mapper/mitos-<scopeID> <mountPoint>`. 
  - `Open(ctx, scopeID, key, mountPoint)`: luksOpen + mount (for a container that already exists, e.g. after a restart).
  - `Close(ctx, scopeID, mountPoint)`: umount + `cryptsetup luksClose mitos-<scopeID>`; zeroize any held key.
  - `Shred(ctx, scopeID, mountPoint)`: best-effort umount + luksClose, then `cryptsetup luksErase --batch-mode <img>` (wipe keyslots) and remove `<img>`; the data is then unrecoverable. Idempotent/tolerant of an already-closed container.
  - The key is passed to cryptsetup ONLY via stdin (--key-file -). Never in argv.
- [ ] Tests (container_test.go, key_test.go) with a recording runner (captures argv + whether stdin carried the key, NEVER executes cryptsetup): Create issues luksFormat/luksOpen/mkfs/mount in order with the key on stdin and NOT in any argv; Close umounts + luksCloses; Shred issues luksErase + removes the img + tolerates a missing container; scopeID traversal (`../x`, `a/b`) is rejected before any command; NewKey returns 32 random bytes and Zeroize clears them; the Key never appears in a formatted/logged string. No real cryptsetup.
- [ ] Commit `feat: internal/storecrypt per-scope LUKS containers with crypto-shred`.

### Task 2: engine integration, encryption behind a flag

**Files:** `internal/fork/engine.go`, `cmd/forkd/main.go`, tests.

- [ ] EngineOpts: `EnableEncryption bool` and a key source (PR1: an injected `KeyProvider` interface `KeyFor(scopeID string) (storecrypt.Key, error)` with a node-local in-memory/tmpfs implementation for PR1; PR2 swaps in the k8s Secret/KMS provider). NewEngine constructs the storecrypt.Manager when enabled.
- [ ] CreateTemplate (when encryption enabled): generate/fetch the scope key, `storecrypt.Create` a container sized to the template footprint (snapshot mem+vmstate+rootfs+seed volumes plus headroom; compute a size with a floor), mounted at the template dir, THEN write the snapshot + seed volumes inside it as today. Record that the template is encrypted (a marker or in the manifest/metadata) so Fork knows to open the container.
- [ ] Fork (when the template is encrypted): ensure the container is open and mounted (Open with the scope key if not already mounted), then restore from the mounted path. The mem mmap (CoW) reads from the decrypted mount, so CoW sharing is preserved (document this in a comment: encryption is below the page cache). Keep the container open while forks of that template are live.
- [ ] Template delete / GC: `storecrypt.Shred(scopeID)` crypto-shreds the container (and the key). Terminate of individual forks does not shred the template container (other forks may share it); only template teardown shreds.
- [ ] forkd: `--enable-encryption` (default false; when off, behavior is exactly as today, plaintext on disk as before). Wire EngineOpts + the PR1 KeyProvider. Document --enable-encryption and the PR1 key-custody limitation in the flag help.
- [ ] TDD (no KVM, no real cryptsetup): seam the storecrypt.Manager behind an interface so the engine tests use a fake that records Create/Open/Shred calls and simulates a mounted dir (a temp dir): CreateTemplate with encryption enabled creates a container for the scope and writes the snapshot inside it; Fork opens the container (if not open) and restores from the mounted path; template delete shreds; encryption-disabled path is unchanged (no container). The fake mount is just a temp dir the engine writes into, so the snapshot read/write logic is exercised without dm-crypt.
- [ ] Commit `feat: encrypt template snapshots at rest in per-scope LUKS containers`.

### Task 3: KVM CI proof (real cryptsetup)

**Files:** `.github/workflows/kvm-test.yaml`, a small helper or extend an existing cmd.

- [ ] Ensure cryptsetup is installed on the KVM runner (apt-get install -y cryptsetup if not present). Add an encryption phase:
  1. Create a real per-scope LUKS container (via a small helper that drives storecrypt.Manager with a generated key, or direct cryptsetup in the step), write a known snapshot (the bench/agent snapshot, or a file with a recognizable plaintext marker) inside the mounted container, and unmount/close.
  2. CIPHERTEXT-AT-REST: read the raw backing `<scopeID>.img` and assert the plaintext marker / the snapshot's recognizable bytes are NOT present (grep the raw device for the marker -> must be absent), proving the data on disk is ciphertext.
  3. FORK-WORKS: reopen the container with the key, mount it, and fork+exec a sandbox whose snapshot lives in the container (drive the engine with --enable-encryption, or at minimum restore the snapshot from the mounted path and exec via the guest agent) -> succeeds, proving decryption + CoW restore work through the mount.
  4. CRYPTO-SHRED: run Shred (luksErase + remove img), then assert the container CANNOT be reopened with the original key (cryptsetup luksOpen fails) and the marker is still absent / the data is unrecoverable.
  Gate on: ciphertext-at-rest (marker absent on raw device), fork-works (exec succeeds through the decrypted mount), crypto-shred (reopen fails after erase). Distinguish a setup issue (no cryptsetup / no loop device) from a real assertion failure in the logs.
- [ ] Commit `ci: prove encryption at rest, CoW-preserved restore, and crypto-shred`.

### Task 4: threat model + docs + ROADMAP + PR

**Files:** `docs/threat-model.md`, `docs/encryption.md` (new), `ROADMAP.md`, full verification.

- [ ] `docs/encryption.md` (new): the design (per-scope LUKS container at the block layer; CoW preserved because encryption is below the page cache; the mem mmap reads decrypted pages shared across forks); the crypto-shred mechanism (luksErase wipes keyslots, key destruction, then the ciphertext is unrecoverable); the scope (per-template now, per-workspace when #21 lands); what is PROVEN in CI (ciphertext at rest, CoW-preserved restore, crypto-shred unrecoverable); and the PR1 vs PR2 split: PR1 holds the key in node memory during operation; PR2 hardens custody (key in a k8s Secret/KMS, never persisted on the node data disk, controller-driven per-scope shred). OPEN: key custody hardening (PR2), per-workspace scope (#21), key rotation, encrypted CAS chunks (vs the per-template container), HSM/KMS integration.
- [ ] `docs/threat-model.md`: add the encryption-at-rest row (data at rest is ciphertext in an encrypted container; crypto-shred via luksErase; the residual that PR1 key custody is node-memory, hardened in PR2; CoW preserved). Honest per-row status.
- [ ] ROADMAP / Foundations: mark encryption at rest + crypto-shredding (#31) as PARTIAL/done-for-the-mechanism with the PR2 key-custody hardening noted; all three format-freeze blockers (#31 mechanism, #32, #33) then addressed enough to discuss freezing the format, with #31 PR2 custody as the remaining hardening.
- [ ] Full verification (build darwin + GOOS=linux, vet, lint both, all Go suites with envtest, Python suite, gofmt zero, dash grep zero, go 1.24 preserved, YAML parse). The key must not appear in any log/argv (grep the storecrypt code: no `%s`/log of the key).
- [ ] Push `feat/encryption-at-rest`, PR `Encryption at rest: per-scope LUKS containers with crypto-shredding` body Closes-or-advances #31 (mechanism + crypto-shred + CoW-preserved, CI-proven; key custody hardening PR2), watch CI, dismiss guarded CodeQL alerts with justification if any, merge when green.

**Out of scope (PR2 and later follow-ups):** production key custody (key in a k8s Secret or KMS, never on the node data disk, distributed per scope, controller-driven shred lifecycle); per-workspace scope (Workspace #21); key rotation and re-encryption; encrypting the CAS chunk store (vs the per-template container); HSM/cloud-KMS integration; encrypted vsock/in-flight (separate from at-rest).
