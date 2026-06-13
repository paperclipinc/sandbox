# ADR 0002: the Workspace is a content-addressed artifact, not a CSI PersistentVolume

Status: accepted (2026-06-12)
Issue: #21 (W4 Workspace & state). Related: docs/api/v2-spec.md (§Workspace), ROADMAP.md (W4), the compliance & observability addendum (Workspace-not-CSI is a named residual ADR).

## Context

W4 introduces a `Workspace`: durable, versioned, forkable agent state that
lives independent of any single sandbox. The obvious Kubernetes analogy is the
PersistentVolume (the PVC:Pod relationship the v2 spec itself reaches for to
explain the lifecycle). That analogy invites an implementation: back a Workspace
with a CSI PersistentVolume, mount it into the sandbox, and let CSI snapshots
provide versions.

We have to settle whether a Workspace IS a CSI volume (and should be implemented
as one) or whether it only RHYMES with one. The decision touches the on-disk and
on-wire formats and the honest-Kubernetes-semantics principle, so it is recorded
as an ADR rather than left implicit, and it is named in the compliance addendum
as a residual that ships as an ADR.

## Decision: model the Workspace as a content-addressed, versioned artifact; do NOT implement it as a CSI PersistentVolume

A Workspace is a versioned content-addressed artifact. Its concrete shape:

- A `Workspace` object holds the declarative policy (the store reference, git
  paths, retention, grants).
- Each version is a `WorkspaceRevision`: an immutable point whose payload is a
  content-addressed manifest digest (the same content-addressed store, package
  `internal/cas`, that backs snapshot distribution in §3). Revisions are
  deduplicated by chunk digest across revisions and across workspaces.
- Revisions form a DAG. The lineage edge is `fromWorkspaceRevision` (a fork from
  a parent revision) or `fromClaim` (a revision produced by a sandbox's
  dehydrate). The head is the latest committed revision on that DAG.
- A revision optionally PAIRS with a memory snapshot (`memorySnapshotRef`),
  which makes the head resumable: a sandbox bound to the workspace can resume
  from captured VM state rather than a cold start. The pairing is
  principal-bound per the secrets policy.

Hydrate (materialize a revision into a running sandbox) and dehydrate (capture a
sandbox's state into a new revision) are a DATA PATH over the content-addressed
transfer layer, the same pull/push pipeline snapshot distribution uses (§3: one
pipeline, two artifact types). They are not a volume mount. That data path is a
later W4 slice; this slice is the declarative model only, and no data moves yet.

## Why not CSI

The CSI surface cannot express the properties a Workspace is built on, and
forcing the fit would corrupt them:

1. **Forkability.** A Workspace fork is a cheap, content-addressed branch of the
   revision DAG: a new revision that shares every unchanged chunk with its
   parent. CSI snapshots are volume-scoped, storage-class-dependent, and have no
   notion of a shared-chunk fork that is O(changed bytes); a forky branch would
   become a full snapshot/clone per fork on most drivers.
2. **Content-addressed dedup.** The artifact is chunked and deduplicated by
   sha256 digest across every revision and every workspace, which is what makes
   the revision feed and the per-node toolchain cache cheap. A block volume has
   no cross-volume, cross-workspace dedup; CSI gives us opaque blocks, not
   content addresses.
3. **Revision lineage as first-class.** The DAG (`fromClaim`/
   `fromWorkspaceRevision`), the single-writer-per-revision doctrine, and the
   git rendezvous for fork-and-merge (git is the merge layer; we never do
   filesystem merge) are workspace concepts with no CSI counterpart. CSI has no
   lineage, no merge story, and no immutability guarantee on a version.
4. **Memory-snapshot pairing.** A resumable head pairs disk-state with a VM
   memory snapshot. CSI is a block/filesystem volume interface; it has no place
   to carry, or atomically pair, a memory image with a disk version.

Modeling the Workspace as a CSI PV would also misstate the Kubernetes semantics
the project commits to be honest about: a CSI PV implies pod-scoped mount
lifecycle, storage-class quotas, and CSI snapshot semantics that simply do not
govern a content-addressed revision DAG. Sandboxes are not pods (ADR-adjacent to
the husk boundary), and a Workspace is not their PVC.

## Consequences

- The artifact layer reuses `internal/cas` (the content-addressed store), not a
  CSI driver. The S3-compatible object-storage backend named by
  `spec.store.objectStorageRef` resolves to that store; a real S3 backend is a
  later W4 slice.
- A Workspace does NOT consume a StorageClass, a PVC, or CSI snapshot quota.
  Operators sizing storage reason about the object store and the retention
  policy (`retention.revisions` / `minAge`), not PV capacity.
- Versioning is the revision DAG with immutable revisions, not CSI
  VolumeSnapshots. Retention pruning walks the DAG and protects the head's
  ancestry; it is not a snapshot-class retention.
- The honest-semantics line in docs and the README must NOT describe a Workspace
  as a PersistentVolume or imply CSI snapshot/quota mechanisms govern it. The
  PVC:Pod phrasing in the spec is an analogy for the LIFECYCLE (durable state
  outliving the compute), explicitly not an implementation claim.

## Status of the slice

PROVEN now (envtest, internal/controller): the declarative model. The Workspace
and WorkspaceRevision CRDs, the reconciler that computes head/revisions/
resumable with typed conditions and observedGeneration, the revision Pending ->
Committed transition with immutability of a committed manifest, the
`fromWorkspaceRevision` lineage validation, retention pruning that protects the
head's ancestry, and owner-ref GC of revisions to their workspace.

OPEN (later W4 slices, none move data here): hydrate/dehydrate over the
content-addressed transfer layer; the sandbox<->workspace binding; git
rendezvous for fork-and-merge; outputs extraction on terminate; the
CloudEvents revision change feed (`dev.mitos.workspace.revision.created`);
the memory-snapshot pairing producing a resumable head from a real checkpoint;
the per-node toolchain cache via a Share policy; the S3 object-store backend.
