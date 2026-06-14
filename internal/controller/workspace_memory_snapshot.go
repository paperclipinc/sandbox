package controller

import (
	"context"
	"fmt"
	"sync"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
)

// WorkspaceMemorySnapshotAdapter binds the workspace memory-snapshot seams
// (CheckpointMemory / ResumeMemory / MemorySnapshotExists) to the husk live-VM
// snapshot path, gated behind the controller --workspace-memory-snapshots flag.
//
// Security invariant (non-negotiable): a memory image carries secrets-in-RAM and
// is bound to the principal that captured it (the capturing claim's
// ServiceAccount). The adapter stamps that principal at checkpoint and enforces
// it at exists/resume; it NEVER serves a snapshot to a different principal. The
// principal-binding REFUSAL that protects a resume is enforced upstream in
// maybeResumeMemory (the head's MemorySnapshotPrincipal vs the activating
// claim's ServiceAccount) and is exercised before this adapter's Resume is ever
// reached; this adapter is the second line, refusing any cross-principal Resume
// or Exists it is asked for directly.
//
// The real bare-metal VM-memory image (live snapshot of a running Firecracker
// guest, the issue #18 4b live-VM path) is the cluster-gated tail: on a cluster
// without a KVM-capable kubelet there is no live VMM to image, so the adapter is
// honest and fails loud rather than fabricating a resumable revision (the
// no-unverified-claims rule). When the live snapshot is wired on bare metal, the
// CheckpointLiveVM / RestoreLiveVM / SnapshotPresent hooks below are the slots it
// plugs into; until then they default to the fail-honest path.
type WorkspaceMemorySnapshotAdapter struct {
	// CheckpointLiveVM captures the live VM memory image for the claim's sandbox
	// and returns an opaque snapshot ref (a content pointer / snapshot id, never
	// the memory bytes). Nil means the live-VM image is not wired on this build:
	// Checkpoint then fails loud (no fabricated snapshot). It is the bare-metal
	// tail.
	CheckpointLiveVM func(ctx context.Context, claim *v1alpha1.SandboxClaim) (ref string, err error)

	// RestoreLiveVM loads a previously captured memory image (named by ref) into
	// the claim's sandbox VM. Nil means the live-VM restore is not wired: Resume
	// fails loud. Bare-metal tail.
	RestoreLiveVM func(ctx context.Context, claim *v1alpha1.SandboxClaim, ref string) error

	// SnapshotPresent reports whether the snapshot store still holds ref. Nil
	// means the store check is not wired: Exists reports absent (fail closed), so
	// an unverifiable snapshot never advertises a head as resumable. Bare-metal
	// tail.
	SnapshotPresent func(ctx context.Context, ref string) (bool, error)

	// binding records, in-process, which principal each captured ref is bound to.
	// It is a defense-in-depth principal guard on the adapter's own Resume/Exists
	// surface: the durable binding lives on the WorkspaceRevision
	// (MemorySnapshotPrincipal), and the upstream refusal already gates the
	// resume; this map refuses a cross-principal lookup the adapter is asked for
	// directly even if a caller bypassed the upstream check. A ref maps to exactly
	// one principal (the capturing claim's ServiceAccount). Never holds a secret
	// value: keys are refs, values are principal names.
	mu      sync.Mutex
	binding map[string]string
}

// Checkpoint captures the sandbox VM memory snapshot and binds it to the
// capturing claim's principal. The returned principal is stamped onto the new
// WorkspaceRevision (MemorySnapshotPrincipal) so a later resume is principal
// gated. A nil CheckpointLiveVM (the live-VM image not wired on this build) fails
// loud rather than producing a revision falsely marked resumable.
func (a *WorkspaceMemorySnapshotAdapter) Checkpoint(ctx context.Context, claim *v1alpha1.SandboxClaim) (memSnapshotResult, error) {
	if a.CheckpointLiveVM == nil {
		return memSnapshotResult{}, fmt.Errorf(
			"workspace memory-snapshot checkpoint is enabled (--workspace-memory-snapshots) but the live-VM image is not available on this node for claim %s: the bare-metal live-VM snapshot path must be present (a KVM-capable kubelet); refusing to fabricate a resumable revision",
			claim.Name,
		)
	}
	ref, err := a.CheckpointLiveVM(ctx, claim)
	if err != nil {
		return memSnapshotResult{}, fmt.Errorf("capture live-VM memory snapshot for claim %s: %w", claim.Name, err)
	}
	if ref == "" {
		return memSnapshotResult{}, fmt.Errorf("live-VM memory snapshot for claim %s returned an empty ref; refusing to pair an unidentifiable snapshot", claim.Name)
	}
	principal := claim.Spec.ServiceAccount
	a.mu.Lock()
	if a.binding == nil {
		a.binding = map[string]string{}
	}
	a.binding[ref] = principal
	a.mu.Unlock()
	return memSnapshotResult{Ref: ref, Principal: principal}, nil
}

// Resume restores the paired memory image into the claim's sandbox VM. It is
// principal gated: the adapter refuses to restore a ref bound (in its own
// binding map) to a different principal than the activating claim. This is the
// second line behind the upstream maybeResumeMemory refusal; a memory image is
// never served across principals.
func (a *WorkspaceMemorySnapshotAdapter) Resume(ctx context.Context, claim *v1alpha1.SandboxClaim, ref string) error {
	if a.RestoreLiveVM == nil {
		return fmt.Errorf(
			"workspace memory-snapshot resume is enabled (--workspace-memory-snapshots) but the live-VM restore is not available on this node for claim %s: the bare-metal live-VM restore path must be present (a KVM-capable kubelet)",
			claim.Name,
		)
	}
	a.mu.Lock()
	bound, known := a.binding[ref]
	a.mu.Unlock()
	if known && bound != claim.Spec.ServiceAccount {
		return fmt.Errorf(
			"refusing to resume memory snapshot for claim %s: the snapshot is bound to a different principal; a memory image carries secrets-in-RAM and is never served across principals",
			claim.Name,
		)
	}
	return a.RestoreLiveVM(ctx, claim, ref)
}

// Exists reports whether the snapshot named by ref still exists AND is bound to
// the given principal. A cross-principal ref reports absent (so a head bound to
// another principal never advertises resumable), and a GC'd ref reports absent.
// A nil SnapshotPresent (store check not wired) reports absent, fail closed.
func (a *WorkspaceMemorySnapshotAdapter) Exists(ctx context.Context, ref, principal string) (bool, error) {
	a.mu.Lock()
	bound, known := a.binding[ref]
	a.mu.Unlock()
	if known && bound != principal {
		// Cross-principal: never resumable.
		return false, nil
	}
	if a.SnapshotPresent == nil {
		return false, nil
	}
	return a.SnapshotPresent(ctx, ref)
}
