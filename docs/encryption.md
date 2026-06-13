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
   `EnsureEncKey` (`internal/controller/enc_key_secret.go`). This generates a
   32-byte data-encryption key (DEK) with `crypto/rand`, WRAPS it with the KMS
   key-encryption key (KEK) via `kms.Wrap`, zeroizes the plaintext DEK
   immediately, and creates a `<templateID>-enc-key` Secret in the template's
   namespace holding ONLY the wrapped DEK (data key `wrapped-dek`) and the
   non-secret KEK id (data key `kek-id`). It reads the wrapped DEK back
   idempotently on later calls. The plaintext DEK is NEVER persisted to etcd or
   disk; only the Secret name and the KEK id (non-secret) appear in controller
   logs. The wrapped DEK and the plaintext DEK are never logged, never in event
   messages, and never in CRD status or conditions. See the KMS/HSM envelope
   encryption section below.
2. The Secret is owner-referenced to the `SandboxTemplate` object with
   `SetControllerReference`. Kubernetes garbage collection deletes the Secret
   automatically when the template is deleted, performing the crypto-shred at
   the Kubernetes level.
3. The WRAPPED DEK plus the KEK id are delivered to forkd inside the
   mTLS-protected gRPC request: `CreateTemplateRequest.EncryptionKey` +
   `kek_id` for template builds and `ForkRequest.EncryptionKey` + `kek_id` for
   forks. The RPC channel is TLS 1.3 with mutual certificate authentication (see
   `internal/pki` and the threat model §3). Because only the WRAPPED DEK travels
   and is persisted, the plaintext DEK is never outside controller memory (where
   it is zeroized post-wrap) and the forkd open window.
4. forkd's grpc_service stashes the wrapped DEK + KEK id into a
   `RequestKeyProvider` (`internal/fork/encryption.go`) via `SetWrappedKey`
   before invoking the engine and calls `ForgetKey` (drop the wrapped entry) in
   a deferred call after the RPC returns. `KeyFor` UNWRAPS the DEK via the local
   KMS (`--kek-file`) on demand into a fresh `storecrypt.Key`; the engine
   zeroizes that plaintext DEK immediately after the cryptsetup open/create. A
   `RequestKeyProvider` fails closed: if no wrapped DEK is stashed for a scope
   and encryption is enabled, or if the KMS cannot unwrap (wrong KEK), `KeyFor`
   returns an error and the operation is refused rather than running unencrypted.
5. The plaintext DEK exists in forkd memory ONLY for the duration of a container
   open/create and is zeroized immediately after. The wrapped DEK is held by the
   `RequestKeyProvider` only for the duration of the RPC. If forkd restarts, the
   next `CreateTemplate` or `Fork` RPC re-delivers the wrapped DEK from the
   controller, and forkd re-unwraps it via its KEK.

### Trust boundary

- etcd holds ONLY the WRAPPED DEK (and the non-secret KEK id), never the
  plaintext DEK. With envelope encryption the etcd-encryption-at-rest assumption
  is DOWNGRADED to defense-in-depth: an attacker who exfiltrates an etcd backup
  but not the KEK cannot unwrap the DEK. Encrypting etcd at rest (e.g. via a KMS
  provider in the kube-apiserver's `EncryptionConfiguration`) is still
  recommended as a second layer, but it is no longer the sole barrier.
- The controller no longer holds the plaintext DEK after `EnsureEncKey` returns:
  it generates the DEK, wraps it, and zeroizes the plaintext in the same call.
- The KEK is the new trust anchor. For the local provider the KEK is an AES-256
  key loaded from a Secret-mounted file (`--kek-file`) with restrictive
  permissions; it never appears in argv or logs (only its non-secret KEKID
  fingerprint does). For a future cloud KMS/HSM provider the KEK never leaves the
  HSM. Destroying or rotating the KEK crypto-shreds every DEK it wrapped at once.
- The node data disk is NOT trusted. Neither the plaintext DEK nor the wrapped
  DEK is written there; only ciphertext (`<scope>.img`) and the LUKS container
  structure (meaningless without the DEK) are stored on disk.
- The controller itself is trusted. A compromised controller can read the Secret
  and deliver the key to any forkd. The controller's RBAC and the cluster's
  admin boundary are the trust anchors here.

### Crypto-shred lifecycle

Deleting a `SandboxTemplate`:
1. Kubernetes GC deletes the `<templateID>-enc-key` Secret via the owner
   reference. The only stored copy of the WRAPPED DEK is now gone.
2. The LUKS keyslots on the node are wiped by `luksErase` when forkd runs
   `shredTemplateContainer` at `DeleteTemplate`. The backing image is removed.
3. The in-memory plaintext DEK was already zeroized after each cryptsetup call;
   `ForgetKey` drops the wrapped entry after the shred.

After step 1 alone, the ciphertext on the node cannot be recovered even by an
attacker who has the node, because there is no surviving DEK copy. With envelope
encryption there is a stronger property: even an etcd backup that still holds the
wrapped DEK is useless without the KEK, and destroying or rotating the KEK
crypto-shreds every DEK it wrapped at once. Steps 2-3 are defense-in-depth.

## KMS/HSM envelope encryption

The per-template DEK is wrapped by a key-encryption key (KEK) held behind a
pluggable `kms.Wrapper` (`internal/kms`). This is envelope encryption: the
controller generates the DEK, wraps it with the KEK, zeroizes the plaintext, and
persists only the wrapped DEK plus the KEK id; forkd unwraps via the KEK at use
time and zeroizes the plaintext immediately.

- **Interface:** `kms.Wrapper` is `Wrap(ctx, plaintextDEK) (WrappedKey, error)`,
  `Unwrap(ctx, WrappedKey) ([]byte, error)`, and `KEKID() string`. `WrappedKey`
  carries the non-secret `KEKID` and the opaque `Ciphertext` (the wrapped DEK).
  The context lets a cloud provider bound and cancel its remote call.
- **Local provider (shipped, CI-testable):** `kms.LocalKEK` is AES-256-GCM with a
  32-byte KEK and a fresh 12-byte nonce per wrap, framed as
  `nonce || GCM(ciphertext+tag)`. The KEK is loaded from a Secret-mounted file by
  PATH (`--kek-file` on both the controller and forkd), never as a value in argv,
  and is never logged. `KEKID()` is `local:` followed by the first 8 bytes of
  `SHA-256(KEK)` in hex: a stable, non-reversible fingerprint that matches a
  wrapped DEK to its KEK and makes a KEK rotation detectable (an `Unwrap` with a
  mismatched KEK id fails closed).
- **Fail closed:** forkd refuses to start under `--enable-encryption` without
  `--kek-file`, so a wrapped DEK can never arrive without an unwrapper. The
  controller fails `EnsureEncKey` for an Encrypted template when no KMS is wired
  (no `--kek-file`).
- **Cloud KMS/HSM (interface-only follow-up):** AWS KMS, GCP KMS, and HashiCorp
  Vault Transit each implement `kms.Wrapper` as a new file in `internal/kms`,
  where `Wrap`/`Unwrap` are remote calls and the KEK never leaves the HSM. No
  cloud SDK is added yet; the interface is shaped for them.

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

- **Envelope round-trip and tamper:** `internal/kms` unit tests prove `LocalKEK`
  wrap/unwrap round-trips a DEK, that a tampered wrapped DEK fails GCM
  authentication, that a wrong-length KEK is rejected, that the KEKID is stable
  and leaks no KEK bytes, and that a KEK mismatch fails closed on unwrap.
- **Secret stores ONLY the wrapped DEK:** the envtest proves `EnsureEncKey`
  creates a `<template>-enc-key` Secret holding `wrapped-dek` and `kek-id` and NO
  raw `key` data key, that the wrapped DEK unwraps to a 32-byte DEK via the test
  KMS, and that the Secret is owner-referenced to the `SandboxTemplate`.
- **Wrapped DEK over RPC:** the controller delivers the wrapped DEK in
  `CreateTemplateRequest.EncryptionKey`/`ForkRequest.EncryptionKey` and the KEK id
  in `kek_id`; the grpc_service stashes them via `SetWrappedKey` and forgets them
  after; forkd unwraps via the KMS and zeroizes the plaintext.
- **Plaintext DEK not on disk, zeroized after use:** the `RequestKeyProvider`
  holds only the wrapped DEK; `KeyFor` returns a freshly-unwrapped copy the
  engine zeroizes after each cryptsetup call; no code path writes the plaintext
  or wrapped DEK to any file under the data dir.
- **Fail-closed:** `RequestKeyProvider.KeyFor` returns an error when no wrapped
  DEK is stashed or when the KMS cannot unwrap (wrong KEK); the engine refuses to
  run unencrypted. forkd refuses to start under `--enable-encryption` without
  `--kek-file`.
- **DEK and KEK never logged:** no log statement, error format, span attribute,
  or condition message in `internal/kms/local.go`, `internal/fork/encryption.go`,
  `internal/daemon/grpc_service.go`, or `internal/controller/enc_key_secret.go`
  formats or names the plaintext DEK, the wrapped DEK, or the KEK; only the
  non-secret KEK id, scope ids, and counts appear.

The LUKS mechanism (ciphertext at rest, decrypt/restore, crypto-shred
unrecoverable) is proven on real `cryptsetup` in the KVM CI job as described
above.

## Open follow-ups

- **forkd container-shred-on-template-GC:** the TEARDOWN BOUNDARY above. The
  controller does not yet send a `DeleteTemplate` RPC on template deletion; the
  node-side container is not crypto-shredded by the controller today.
- **Cloud KMS / HSM providers:** the envelope mechanism ships with the LOCAL
  AES-256-GCM provider (`kms.LocalKEK`, CI-testable without cloud creds). AWS KMS,
  GCP KMS, and Vault Transit are interface-only follow-ups (`kms.Wrapper` is
  shaped for them; no cloud SDK is added yet) where the KEK never leaves the HSM.
- **KEK rotation and DEK re-wrap:** rotate the KEK and re-wrap every stored
  wrapped DEK. The KEKID mismatch in `Unwrap` is the rotation-detection hook this
  work installs.
- **DEK rotation and re-encryption:** rotate the DEK and re-encrypt the LUKS
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
