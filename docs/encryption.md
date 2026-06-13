# Encryption at rest (#31)

Status: mechanism done and CI-proven (PR1); key-custody hardening done (PR2).
Issue #31 is addressed: mechanism in PR1, custody in PR2.

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
   `/dev/mapper/mitos-<scopeID>`, makes an ext4 filesystem on that device,
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

Behind `--enable-encryption`, forkd wires a `RequestKeyProvider` (PR2) and the
engine builds the real `storecrypt.Manager` (`storecrypt.DefaultRunner`):

- `CreateTemplate` calls `createTemplateContainer` BEFORE building the snapshot,
  sizes the container to the template footprint, mounts it at the template dir,
  and writes an `.encrypted` marker inside the (encrypted) container. The
  controller delivers the key in the `CreateTemplateRequest.EncryptionKey` field;
  grpc_service stashes it into the `RequestKeyProvider` for the duration of the
  call and forgets it afterwards.
- `Fork` calls `ensureTemplateOpen`, which opens+mounts the container if it is
  not already open, then restores from the decrypted mount as usual. The
  controller delivers the same key in `ForkRequest.EncryptionKey`; grpc_service
  stashes and forgets it exactly as above.
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

## Key custody (PR2)

The controller owns the per-template encryption key. The key never touches the
node data disk.

### Key lifecycle

1. When `SandboxTemplate.Spec.Encrypted` is true, the pool reconciler calls
   `EnsureEncKey` (`internal/controller/enc_key_secret.go`). This creates a
   `<templateID>-enc-key` Secret in the template's namespace on the first call
   and reads it back idempotently afterwards. The Secret holds a 32-byte key
   generated with `crypto/rand` and is typed `Opaque`. Only the Secret name
   appears in controller logs; key bytes are never logged, never in event
   messages, and never in CRD status or conditions.
2. The Secret is owner-referenced to the `SandboxTemplate` object with
   `SetControllerReference`. Kubernetes garbage collection deletes the Secret
   automatically when the template is deleted, performing the crypto-shred at
   the Kubernetes level.
3. The key is delivered to forkd inside the mTLS-protected gRPC request:
   `CreateTemplateRequest.EncryptionKey` for template builds and
   `ForkRequest.EncryptionKey` for forks. The RPC channel is TLS 1.3 with
   mutual certificate authentication (see `internal/pki` and the threat model
   §3), so the key is never in plaintext outside the controller and etcd.
4. forkd's grpc_service stashes the key into a `RequestKeyProvider`
   (`internal/fork/encryption.go`) before invoking the engine and calls
   `ForgetKey` (zeroize + delete) in a deferred call after the RPC returns.
   The key is in node memory only for the duration of the RPC. A
   `RequestKeyProvider` fails closed: if no key is stashed for a scope and
   encryption is enabled, `KeyFor` returns an error and the operation is
   refused rather than running unencrypted.
5. While a template's container is open (across forks, for the lifetime of the
   forkd process), the key is held in node memory by the `RequestKeyProvider`.
   If forkd restarts, the next `CreateTemplate` or `Fork` RPC re-delivers the
   key from the controller.

### Trust boundary

- The controller and etcd are trusted with the key. The cluster MUST encrypt
  etcd at rest (e.g. via a KMS provider configured in the kube-apiserver's
  `EncryptionConfiguration`) for this to be meaningful; without etcd encryption
  the Secret data is plaintext in the backing store. This assumption is stated
  explicitly and is the operator's responsibility.
- The node data disk is NOT trusted. The key is never written there; only
  ciphertext (`<scope>.img`) and the LUKS container structure (which is
  meaningless without a key) are stored on disk.
- The controller itself is trusted. A compromised controller can read the Secret
  and deliver the key to any forkd. The controller's RBAC and the cluster's
  admin boundary are the trust anchors here.

### Crypto-shred lifecycle

Deleting a `SandboxTemplate`:
1. Kubernetes GC deletes the `<templateID>-enc-key` Secret via the owner
   reference. The escrowed key is now gone.
2. The LUKS keyslots on the node are wiped by `luksErase` when forkd runs
   `shredTemplateContainer` at `DeleteTemplate`. The backing image is removed.
3. The in-memory key copy is zeroized by `ForgetKey` after the shred.

After step 1 alone, the ciphertext on the node cannot be recovered even by
an attacker who has the node, because there is no surviving key copy. Steps 2-3
are defense-in-depth.

TEARDOWN BOUNDARY: the controller does NOT today send a `DeleteTemplate` RPC to
forkd when a `SandboxTemplate` is deleted. There is no SandboxTemplate
reconciler and the pool reconciler never calls `DeleteTemplate`. The key Secret
is GC'd via the owner reference (step 1 above), but the node-side encrypted
container is reclaimed only by node data dir lifecycle until the forkd
container-shred-on-template-GC wiring is added. That wiring is deliberately
deferred and tracked as a follow-up; the honest status is documented in
`enc_key_secret.go` as the `TEARDOWN BOUNDARY (PR2)` comment.

## PR1 vs PR2 split

- **PR1:** the mechanism. Per-scope LUKS containers, CoW-preserving restore
  through the decrypted mount, crypto-shred on delete, engine wiring behind
  `--enable-encryption`, KVM CI proof. The key was held in NODE MEMORY by
  `InMemoryKeyProvider`: generated per scope, not escrowed, and lost on restart.
  This was a deliberate placeholder.
- **PR2 (this work):** key custody hardening. The controller generates a
  per-template key with `crypto/rand`, stores it in a `<template>-enc-key`
  Secret owner-referenced to the `SandboxTemplate`, delivers it to forkd over
  the mTLS gRPC `CreateTemplate` and `Fork` requests, and forkd holds it in
  memory only via `RequestKeyProvider` and never writes it to the node data
  disk. Encryption enabled with no delivered key fails closed. The key is never
  logged. Issue #31 is addressed with PR1 + PR2.

## What is PROVEN in CI (PR2 additions)

The envtest suite (`internal/controller/enc_key_envtest_test.go`) and unit tests
(`internal/daemon/enc_key_test.go`, `internal/fork/encryption_test.go`) prove:

- **Secret lifecycle:** `EnsureEncKey` creates a `<template>-enc-key` Secret on
  first call and reads it back idempotently; the Secret has an owner reference to
  the `SandboxTemplate` so it is GC'd when the template is deleted.
- **Key over RPC:** the controller reads the key from the Secret and delivers it
  in `CreateTemplateRequest.EncryptionKey` and `ForkRequest.EncryptionKey`; the
  grpc_service stashes it into the `RequestKeyProvider` and forgets it after.
- **Key not on disk:** the `RequestKeyProvider` holds the key in node memory
  only; no code path writes the key to any file under the data dir.
- **Fail-closed:** `RequestKeyProvider.KeyFor` returns an error when no key is
  stashed; the engine refuses to run unencrypted rather than proceeding silently.
- **Key never logged:** no log statement, error format, span attribute, or
  condition message in `internal/fork/encryption.go`,
  `internal/daemon/grpc_service.go`, or `internal/controller/enc_key_secret.go`
  formats or names key bytes.

The LUKS mechanism (ciphertext at rest, decrypt/restore, crypto-shred
unrecoverable) is proven on real `cryptsetup` in the KVM CI job as described
above.

## Open follow-ups

- **forkd container-shred-on-template-GC:** the TEARDOWN BOUNDARY above. The
  controller does not yet send a `DeleteTemplate` RPC on template deletion; the
  node-side container is not crypto-shredded by the controller today.
- **KMS / HSM envelope encryption:** replace the raw k8s Secret with a key
  wrapped by a cloud KMS or HSM so the key material is never in plaintext in
  etcd, removing the etcd-encryption-at-rest assumption.
- **Key rotation and re-encryption:** rotate the key and re-encrypt the LUKS
  container without rebuilding the template.
- **Per-workspace scope (Workspace #21):** make the scope a workspace so erasing
  a workspace crypto-shreds all its templates and volumes.
- **Encrypting the CAS chunk store:** the content-addressed snapshot store is not
  encrypted today; only per-template containers are.
- **Node-memory dump while open:** while a container is open the key is
  necessarily in forkd's process memory to serve I/O. A node-memory dump by a
  root attacker yields the key. Full mitigation requires HSM key custody (the key
  is held in the HSM and only the encrypted session is in memory). Zeroize-on-
  close is the current partial mitigation.
- In-flight encryption of the vsock/control channels is a separate concern from
  at-rest (tracked elsewhere), not covered here.
