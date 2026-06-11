# Encryption at rest (#31)

Status: mechanism done and CI-proven; key-custody hardening (PR2) is open.

This document describes how Paperclip encrypts template snapshots and volumes at
rest, why copy-on-write (CoW) page sharing across forks is preserved, how
erasure becomes crypto-shredding, and the PR1 vs PR2 split. It is the design
reference behind the threat-model row "Encryption at rest + crypto-shredding"
in `docs/threat-model.md`.

Encryption is opt-in: forkd takes `--enable-encryption` (default off). With the
flag off the behavior is exactly as before, plaintext snapshots on disk.

## Design: a per-scope LUKS container at the block layer

A scope is the unit that gets its own key and its own encrypted container. In
PR1 the scope is a template; when Workspace (#21) lands the scope becomes a
workspace, so erasing a workspace crypto-shreds everything built under it.

Each scope gets its own LUKS2 container (`internal/storecrypt`):

1. `Create` fallocates a sparse image file at `<dataDir>/enc/<scopeID>.img`,
   `cryptsetup luksFormat`s it as LUKS2, `luksOpen`s it to a dm-crypt device at
   `/dev/mapper/agentrun-<scopeID>`, makes an ext4 filesystem on that device,
   and mounts it at the scope's data directory.
2. The engine then builds the template snapshot (mem, vmstate, rootfs) and any
   seed volumes INSIDE that mounted directory. Everything written there goes
   through dm-crypt, so the bytes that land in `<scopeID>.img` are ciphertext.
3. `Open` reattaches an existing container (`luksOpen` + mount), e.g. to fork
   from a template whose container is not currently open.
4. `Close` unmounts and `luksClose`s the device.
5. `Shred` crypto-shreds the container (see below).

The scope id is validated against `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$` before any
image file is created or any cryptsetup command is built, so a scope id can
never introduce a `..` segment or escape the image directory or the mapper
namespace.

### The key never appears in argv or a log

`cryptsetup` reads the key from `--key-file -`, i.e. the child process stdin, so
the key is never on a command line visible to other users via `/proc`. The
`storecrypt.Key` type redacts itself (`String`, `MarshalText` return a fixed
placeholder), so a stray `%v`/`%s`/log/JSON of a key cannot leak its bytes. Only
key lengths and operation names are ever logged.

## CoW is preserved because encryption is below the page cache

The performance reason snapshot-fork is fast is that many forks of one template
`mmap(MAP_PRIVATE)` the same mem file: the restored read-only page set is
shared across forks and only divergent (written) pages become per-fork private
copies. Naively encrypting each fork's view would break that sharing.

dm-crypt encrypts at the BLOCK layer, below the page cache. Once a container is
open and mounted, the kernel page cache holds DECRYPTED pages for the files in
it. The mem file the forks mmap is a file on that decrypted filesystem, so all
forks share the SAME decrypted pages in the page cache exactly as in the
plaintext case. There is no per-fork decryption copy: encryption/decryption
happens once at the block boundary, and CoW page sharing across forks is
preserved unchanged.

This is why the container is kept open across forks: it is opened once (at build
time, or lazily on first fork after a restart) and only closed/shredded at scope
teardown, so the hot fork path never pays an open+mount and never re-decrypts.

## Crypto-shredding: erasure by destroying the key material

Deleting a scope does not need to overwrite the (potentially large) ciphertext.
`Shred` runs `cryptsetup luksErase` on the image, which wipes the LUKS keyslots,
and then removes the image file. After the keyslots are erased the master key
is gone, so the remaining ciphertext is unrecoverable even by someone who still
holds the passphrase/key: there is no keyslot left that can derive the master
key, so the ciphertext can no longer be decrypted. This is crypto-shredding:
fast, constant-time erasure independent of the data size, which is exactly the
property a per-workspace erasure (#21) needs.

`Shred` is idempotent (a missing image or an already-closed device is not an
error), so repeated GC of the same scope is safe.

## Engine integration

Behind `--enable-encryption`, forkd injects an `InMemoryKeyProvider` (PR1) and
the engine builds the real `storecrypt.Manager` (`storecrypt.DefaultRunner`):

- `CreateTemplate` calls `createTemplateContainer` BEFORE building the snapshot,
  sizes the container to the template footprint, mounts it at the template dir,
  and writes an `.encrypted` marker inside the (encrypted) container.
- `Fork` calls `ensureTemplateOpen`, which opens+mounts the container if it is
  not already open, then restores from the decrypted mount as usual.
- `DeleteTemplate` calls `shredTemplateContainer`, which crypto-shreds the
  container. Individual fork teardown never shreds, because sibling forks may
  share the open container; only template teardown does.

The container manager is a narrow seam (`containerManager`) so engine unit tests
inject a fake using a plain directory as the "mount" (the snapshot write/read
logic runs without dm-crypt), while production uses the real cryptsetup-backed
`storecrypt.Manager`.

## What is PROVEN in CI

A KVM CI phase (`.github/workflows/kvm-test.yaml`) drives the REAL
`storecrypt.Manager` through `cmd/crypt-smoke` (which uses the production
`DefaultRunner`, so the actual package code path runs, not a hand-rolled
cryptsetup script) on real `cryptsetup`:

1. **Ciphertext at rest.** A control read finds a unique plaintext marker in the
   decrypted mount while the container is open (proving the marker string is
   findable, so the grep is sound), but a grep of the raw backing
   `<scopeID>.img` after close finds it ZERO times. The bytes on disk are
   ciphertext.
2. **Decrypt/restore works.** Reopen the container with the key, mount it, and
   the marker reads back intact. The full engine fork-through-encryption path is
   covered by the `internal/fork` unit tests plus this decrypt-roundtrip on the
   real block layer.
3. **Crypto-shred is unrecoverable.** After `Shred` (luksErase + image removal),
   reopening with the ORIGINAL key fails and the image is gone.

Setup problems (no cryptsetup, no loop/device-mapper) are logged distinctly as
`ENCRYPTION-SETUP-FLAKE` and still fail the job, separate from a real
`ENCRYPTION-ASSERTION-FAILED`.

## PR1 vs PR2 split

- **PR1 (this work):** the mechanism. Per-scope LUKS containers, CoW-preserving
  restore through the decrypted mount, crypto-shred on delete, engine wiring
  behind `--enable-encryption`, KVM CI proof. The key is held in NODE MEMORY by
  `InMemoryKeyProvider`: generated per scope, NOT escrowed, and lost on restart
  (an existing encrypted template can then no longer be opened). This is a
  deliberate placeholder.
- **PR2 (open):** key custody hardening. Move the key off the node data disk
  into a Kubernetes Secret or a KMS, distribute it per scope, and drive the
  per-scope shred lifecycle from the controller so erasure is an
  intent-driven, auditable operation rather than a node-local one. The key is
  never persisted on the node data disk.

## Open follow-ups

- Key custody hardening to a k8s Secret / KMS (PR2, above).
- Per-workspace scope (Workspace #21): make the scope a workspace so erasing a
  workspace crypto-shreds all its templates and volumes.
- Key rotation and re-encryption.
- Encrypting the CAS chunk store (the content-addressed snapshot store) rather
  than only the per-template container.
- HSM / cloud-KMS integration.
- In-flight encryption of the vsock/control channels is a separate concern from
  at-rest (tracked elsewhere), not covered here.
