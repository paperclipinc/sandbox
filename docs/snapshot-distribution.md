# Snapshot distribution

Loading snapshots "from local storage" reinvents image pull with multi-GB
artifacts. This document describes the content-addressed store that forkd uses
to identify, deduplicate, transfer, evict, and verify snapshots, and states
precisely what is verified by tests today and what remains open and unmeasured.

The core single-node machinery is implemented and tested. The multi-node
distribution layer (P2P orchestration, propagation benchmarks, lazy serving) is
NOT built and carries no numbers; see "Open and unmeasured" below.

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

The node-to-node orchestration around this surface (which node pulls from which,
peer discovery) is out of scope of the `cas` package and tracked in #14.

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

The operational helper `cmd/cas-verify` (modes `put`, `materialize`, `check`,
`tamper-check`) is the scriptable harness that drives the real-snapshot checks
in CI; it depends only on `internal/cas` and the standard library.

## Open and unmeasured

The following are NOT built or NOT measured. No propagation-time numbers are
stated anywhere until a multi-node testbed exists.

- Multi-node P2P orchestration (Spegel-style node-to-node sync) and peer
  discovery: which node pulls from which is not implemented (tracked in #14).
- Pool-update-to-all-nodes-ready propagation at 10/50/100 nodes: requires a
  multi-node testbed; no numbers are stated.
- Lazy-load partial-fetch serving (`prefetch: full | lazy`): serving forks from
  a partially fetched snapshot is not built.
- External snapshot import and its publish-authorization policy beyond the
  mTLS-gated `CreateTemplate`.

See ROADMAP.md section 3 for status and #14 for the multi-node epic.
