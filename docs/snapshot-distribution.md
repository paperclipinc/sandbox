# Snapshot distribution

Loading snapshots "from local storage" reinvents image pull with multi-GB
artifacts. This document describes the content-addressed store that forkd uses
to identify, deduplicate, transfer, evict, and verify snapshots, and states
precisely what is verified by tests today and what remains open and unmeasured.

The core single-node machinery is implemented and tested. The build-once-
distribute layer (forkd serves its CAS to peers over token-gated mTLS; a
PullTemplate RPC pulls, materializes, and verifies a template from a holder; the
pool reconciler builds a non-encrypted template once and distributes by pull
instead of rebuilding on every node) is implemented and proven on two processes
in CI. The measured cross-node propagation rate, a shared registry/object-store
mirror as a pull source, per-node SAN pinning, and distribution of encrypted
templates are NOT built or NOT measured and carry no numbers; see "Open and
unmeasured" below.

## Content-addressed store

A snapshot is a set of named files: `mem`, `vmstate`, and, when present,
`rootfs`. The store (`internal/cas`) is rooted at a directory:

```
<root>/chunks/<digest[:2]>/<digest>   chunk data
<root>/manifests/<digest>             canonical manifest bytes
<root>/pins/<digest>                  pin marker for a manifest
```

Each file is split into fixed-size chunks (`ChunkSize`, 4 MiB), each keyed by
the lowercase hex sha256 of its bytes. Identical chunks across snapshots are
stored once: that is the deduplication mechanism. All writes are atomic (temp
file in the same directory, then rename), so a crash never leaves a partial
chunk under its final digest name.

`PutSnapshot` streams each file exactly once: it hashes each chunk, writes it
if not already present (the dedup skip), and collects the ordered chunk refs in
the same pass, then assembles and writes the manifest. Memory stays bounded to
one `ChunkSize` buffer regardless of image size.

## Manifest and digest scheme

The manifest lists, per file, its name, byte size, and the ordered chunk refs
(digest plus size) that reconstruct it, along with `VMMVersion` and a
`CreatedUnix` field. `Manifest.Canonical()` produces a deterministic byte
encoding: files are sorted by name and every object uses a fixed field order,
so the encoding does not depend on Go map ordering or input ordering. The
snapshot's identity is `Digest()`: the sha256 of that canonical encoding. All
digests are content addresses and are safe to log.

`VMMVersion` is part of the manifest and therefore part of the digest. This
aligns the snapshot identity with the version-compatibility contract (#32): a
snapshot taken under one VMM version produces a different digest from the same
memory bytes recorded under another, so a version mismatch cannot be masked by
matching content. Build time is deliberately NOT part of the identity used for
template content addressing: `recordTemplateDigest` and `verifyTemplate` build
the manifest with `createdUnix = 0` so the digest reflects the on-disk bytes
plus the VMM version alone and is reproducible across processes.

Whether a `rootfs` is present is also part of the identity: its presence adds a
file entry and changes the digest.

## Incremental transfer

Pool rebuilds and node-to-node refreshes ship deltas, not whole images. The
transfer surface is the `Transport` interface (`internal/cas/transport.go`):
`HasChunks`, `GetChunk`, and `GetManifest`. An HTTP transport
(`internal/cas/http_transport.go`) implements it over plain HTTP.

`Pull` is the read path: it fetches the remote manifest, verifies the manifest
digest, computes the chunks the local store is missing (`MissingChunks`),
fetches ONLY those, verifies each chunk's sha256 on receipt before storing it
(`PutChunk` refuses a chunk whose bytes do not hash to the claimed digest), and
finally writes the manifest locally (`PutManifest`). After `Pull` returns, the
manifest is materializable locally. Chunks the local store already holds (shared
with earlier snapshots) are never transferred.

The node-to-node orchestration around this surface (which node pulls from which)
lives above the `cas` package: see "Build once, distribute by pull" below.

## Build once, distribute by pull

A template is built ONCE on a single node and then distributed to peer nodes by
pull, instead of being rebuilt from the image on every node. This is the
build-once-distribute model: one node holds the authoritative snapshot, peers
pull the CAS chunks they are missing, and every peer verifies the snapshot
before it ever serves a fork from it.

### CAS serving and auth

Each forkd optionally serves its content-addressed store under `/cas` on a
DEDICATED listener, the `--cas-listen` port (default `:9092`), SEPARATE from the
sandbox HTTP API (`--http`, default `:9091`). This separation is deliberate: the
sandbox exec/files/metrics/healthz API keeps its existing scheme (SDK clients
connect over `http://`), while the CAS surface is served on its own port over
TLS only. CAS distribution never forces the sandbox API onto TLS. The surface is
the read-only `Transport` handler (`cas.NewHTTPHandler`: `GetManifest`,
`HasChunks`, `GetChunk`) wrapped by `cas.RequirePullToken`, and it is enabled
only when ALL of a store, a peer token, a TLS config, and a CAS listen address
are set (`daemon.CASServing`). The controller derives the CAS port (the same pod
IP as the sandbox HTTP endpoint with the CAS port) to build each holder's CAS
source URL. The auth is two layers:

- `FORKD_PEER_TOKEN`: a shared bearer credential a pull must present, read from
  the ENVIRONMENT (not a flag) so it is never exposed in `/proc/<pid>/cmdline`.
  The compare is constant-time, so a wrong token leaks no timing signal, and an
  absent or wrong token is rejected with 403 before any store access. The
  controller is configured with the SAME `FORKD_PEER_TOKEN` and attaches it to
  every pull request (`HTTPTransport.WithBearerToken`). The token is a
  credential: it is never logged, never put in an error or condition message,
  and never passed on the command line.
- forkd-to-forkd mTLS: the surface is served ONLY over TLS, using the same
  control-plane cert pair the gRPC server uses, with `RequireAndVerifyClientCert`
  against the control-plane CA. A peer without a CA-signed client cert is
  rejected at the handshake; the token is the additional gate the holder
  enforces on top. TLS-only is what keeps the bearer token confidential on the
  wire (the chunks themselves are digest-addressed, so chunk integrity does not
  depend on the channel, but the token does). forkd refuses to enable the
  surface if `FORKD_PEER_TOKEN` is set without mTLS, so the token is never served
  in cleartext.

SAN pinning: the pull client pins `pki.ServerName` (the single forkd serving
SAN) as the verified server identity. The holder's serving cert must therefore
carry that SAN, which it does today because the PKI issues exactly that name.
Per-node SAN pinning (a distinct verified identity per holder) is a follow-up,
tracked with the per-pull-token work; until then all forkd nodes share the one
serving identity.

### Pull, materialize, verify, fail closed

`Engine.PullTemplate(templateID, manifestDigest, sourceURL, token)` is the
receiving side. It constructs an `HTTPTransport` to the holder's CAS with the
token attached to every request, `cas.Pull`s the manifest plus only the chunks
this node is missing (each verified on receipt), materializes the snapshot files
into the template layout
(`<dataDir>/templates/<id>/{snapshot/{mem,vmstate},rootfs.ext4,volumes/...}`),
then runs the SAME digest verify and snapshot-compatibility check the fork path
uses and writes the `verified` marker. The integrity guarantee is end to end:
every chunk's sha256 is verified as it is stored, the whole snapshot is verified
against its recorded manifest digest, and the recorded producing environment is
checked for compatibility (snapcompat) before the template is ever servable.

The pull is FAIL CLOSED: on ANY error (a bad/tampered source, a digest mismatch,
an incompatible snapshot, a materialize failure) the partial template directory
is removed and the digest is dropped from the in-memory map, so a failed pull
never leaves a half-materialized, unverified snapshot that a later fork could
pick up. A bad pull never becomes a servable template.

### Reconciler distribution

For a non-encrypted template, the pool reconciler builds it on one selected node
(`CreateTemplate`) and then, for every other node that needs it, calls
`PullTemplate` sourced from a node that already holds it. Nodes do not each
rebuild from the image. Encrypted templates are the exception (see below): they
are built per node and are not distributed.

### Encryption carve-out

Encrypted templates are NOT distributed by pull. With at-rest encryption on
(`--enable-encryption`, #31), each template snapshot is built inside a per-node
LUKS container whose key is held only in that node's memory and never written to
disk, so the CAS chunks on disk for an encrypted template are not the thing a
peer could materialize and decrypt with a shared token. Encrypted templates are
therefore built PER NODE, exactly as before, and the build-once-distribute path
is skipped for them. Distributing encrypted templates requires the CAS chunk
store itself to be encrypted (or a sealed, per-recipient key exchange); that is
the prerequisite follow-up, tracked under #31.

## Bounded cache eviction

Nodes have finite NVMe, so the chunk store is bounded by an mtime-LRU policy
(`internal/cas/evict.go`). Each chunk's access time is touched on materialize;
`EvictToFit` removes least-recently-used chunks until the store fits the target,
skipping any chunk reachable from a pinned manifest. Pins are persisted markers,
so pinning survives restarts. mtime is the crash-safe LRU signal: a missed touch
only makes a chunk look slightly less recently used, never corrupts state.

## Verify-on-load

A template snapshot is content-addressed the moment it is built; its manifest
digest is recorded to `<dataDir>/templates/<id>/manifest.digest`, pinned, and
surfaced through forkd to `SandboxPoolStatus.TemplateDigest`. Before a snapshot
is forked, forkd verifies the on-disk bytes against the recorded digest and
refuses on mismatch.

To keep the fork hot path cheap, verification is verify-once-at-registration,
NOT per fork: at build time (trusted; a `verified` marker is written without
re-hashing) or at first use after a restart (lazy re-hash). Fork only stats the
marker. The dev-mode escape `--allow-unverified-snapshots` downgrades a failed
verification to a loud one-time warning. This control and its residual (tampering
AFTER verification is not re-detected until the marker is cleared) are recorded
in `docs/threat-model.md` section 5.

`Materialize` independently re-verifies every chunk's digest as it streams, and
on ANY error (chunk digest mismatch, missing chunk, copy or sync failure) it
removes the partially written destination file, so a verify failure never leaves
corrupt bytes behind for a caller to consume.

## What is VERIFIED

All of the following are proven by tests in this repository:

- Deduplication: a snapshot that shares most of its bytes with an earlier one
  adds only the differing chunks (`internal/cas` unit tests).
- Byte-identical reconstruction: `Materialize` reproduces every file
  bit-for-bit. Proven by unit tests AND, on a REAL multi-hundred-MB Firecracker
  snapshot, by the KVM CI integrity phase (`cmd/cas-verify check`).
- Incremental delta: `Pull` transfers only missing chunks and verifies each on
  receipt (`internal/cas` transport tests).
- Tamper detection: a single corrupted chunk byte makes `Materialize` fail.
  Proven by unit tests AND on the real KVM snapshot (`cmd/cas-verify
  tamper-check`), with no partial output left behind.
- Verify-on-load: forkd refuses a tampered snapshot before forking; the dev
  escape downgrades to a warning (`internal/fork` verify tests).
- Build once, distribute by pull: a peer PULLS a template over the production
  CAS-serving path and forks from it. `cmd/pull-smoke` drives TWO fork engines
  with TWO data dirs: node A builds a template into its CAS and serves it over
  TLS gated by a peer token; node B's `PullTemplate` pulls it over a real HTTPS
  handshake (the holder's cert carries the pinned `pki.ServerName`, no
  `InsecureSkipVerify`), materializes it, verifies the digest + snapcompat,
  writes the verified marker, and then forks a sandbox and execs inside the
  PULLED snapshot. The KVM CI distribution phase runs this. The same driver also
  proves the auth + integrity gates without KVM: a wrong token is rejected with
  403 and a wrong manifest digest fails the pull and leaves no manifest on the
  puller (fail-closed). The pool reconciler's build-once-then-pull behavior for
  non-encrypted templates (and per-node build for encrypted) is covered by
  `internal/controller` tests.

The operational helper `cmd/cas-verify` (modes `put`, `materialize`, `check`,
`tamper-check`) is the scriptable harness that drives the real-snapshot checks
in CI; it depends only on `internal/cas` and the standard library.

## Open and unmeasured

The following are NOT built or NOT measured. No propagation-time numbers are
stated anywhere until a multi-node testbed exists.

- Measured cross-node propagation rate: the pull + fork path is proven on two
  processes on one machine in CI, but the actual node-to-node transfer rate
  (pool-update-to-all-nodes-ready at 10/50/100 nodes, over a real network
  between hosts) requires a multi-node testbed; no numbers are stated (tracked
  in #14).
- Per-node SAN pinning: all forkd nodes currently share the one serving identity
  (`pki.ServerName`); a distinct verified identity per holder is a follow-up,
  tracked with the per-pull-token work.
- Per-pull minted tokens: today the token is a single shared, controller-
  configured bearer credential; per-pull (or short-lived) minted tokens and a
  full forkd-peer mTLS identity are a follow-up.
- A shared registry/object-store mirror as a pull source (instead of
  peer-to-peer pull from a holder node): not built.
- Distributing encrypted templates: encrypted templates are built per node and
  are NOT distributed; this needs the CAS chunk store itself encrypted (tracked
  under #31).
- Lazy-load partial-fetch serving (`prefetch: full | lazy`): serving forks from
  a partially fetched snapshot is not built.
- External snapshot import and its publish-authorization policy beyond the
  mTLS-gated `CreateTemplate`.

See ROADMAP.md section 3 for status and #14 for the multi-node epic.
