package controller_test

// Phase 2 (W4): the resumable head from a memory snapshot, proven end to end
// behind the existing checkpoint/resume/exists seams.
//
// This is the security-critical pairing: a committed WorkspaceRevision pairs the
// workspace filesystem state (its ContentManifest) with a VM memory snapshot
// (its MemorySnapshotRef), and that memory image is bound to the principal that
// captured it (MemorySnapshotPrincipal). A NEW claim with the SAME principal
// resumes mid-execution from the checkpoint; a claim with a DIFFERENT principal
// is REFUSED, fail-closed, because a memory image carries secrets-in-RAM and is
// never served across principals.
//
// The whole flow runs against the swappable seams (no real VM): checkpoint
// returns a scripted {ref, principal}; exists is principal-bound; resume records
// the ref it is asked to restore. The real bare-metal VM-memory image is the
// cluster-gated tail (see cmd/controller --workspace-memory-snapshots).

import (
	"context"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// resumeRecorder records the refs the resume seam was asked to restore and the
// principals of the claims that asked, so a test can assert resume happened for
// the matching principal and NEVER happened for a mismatched one.
type resumeRecorder struct {
	mu    sync.Mutex
	refs  []string
	sas   []string
	calls int
}

func (r *resumeRecorder) record(claim *v1alpha1.SandboxClaim, ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.refs = append(r.refs, ref)
	r.sas = append(r.sas, claim.Spec.ServiceAccount)
}

func (r *resumeRecorder) calledWith(ref, sa string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.refs {
		if r.refs[i] == ref && r.sas[i] == sa {
			return true
		}
	}
	return false
}

func (r *resumeRecorder) sawPrincipal(sa string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sas {
		if s == sa {
			return true
		}
	}
	return false
}

// TestResumableHeadFromMemorySnapshot proves the resumable head end to end: a
// checkpoint-on-terminate pairs a revision with a memory snapshot bound to the
// capturing claim's principal; the workspace head becomes resumable; a new claim
// with the SAME principal resumes from it; and a claim with a DIFFERENT
// principal is REFUSED (the cross-principal resume never happens), fail-closed.
func TestResumableHeadFromMemorySnapshot(t *testing.T) {
	const (
		wsName    = "ws-resumable"
		snapRef   = "snap-resumable-1"
		principal = "sa-a"
		intruder  = "sa-b"
	)

	// Disk-state seam: hydrate records that the workspace content was restored;
	// dehydrate returns the revision's content manifest. This is the filesystem
	// half of the disk+memory pairing.
	headDigest := cas.Digest(testManifest(0x7a))
	var hydMu sync.Mutex
	hydratedFor := map[string]bool{}
	setWSTransfer(
		func(_ context.Context, claim *v1alpha1.SandboxClaim, _ cas.Digest) error {
			hydMu.Lock()
			defer hydMu.Unlock()
			hydratedFor[claim.Spec.ServiceAccount] = true
			return nil
		},
		func(_ context.Context, _ *v1alpha1.SandboxClaim, _, _ []string) (cas.Digest, error) {
			return headDigest, nil
		},
	)
	t.Cleanup(func() { setWSTransfer(nil, nil) })

	// Memory-state seams: checkpoint binds the snapshot to the capturing claim's
	// principal; exists is principal-bound (only the bound principal verifies
	// true); resume records the restore request.
	rr := &resumeRecorder{}
	setMemSnapshot(
		func(_ context.Context, claim *v1alpha1.SandboxClaim) (controller.MemSnapshotResultForTest, error) {
			return controller.NewMemSnapshotResult(snapRef, claim.Spec.ServiceAccount), nil
		},
		func(_ context.Context, claim *v1alpha1.SandboxClaim, ref string) error {
			rr.record(claim, ref)
			return nil
		},
		func(_ context.Context, ref, p string) (bool, error) {
			// Principal-bound existence: the snapshot exists only for the principal
			// that captured it. A GC'd / cross-principal ref reports absent.
			return ref == snapRef && p == principal, nil
		},
	)
	t.Cleanup(func() { setMemSnapshot(nil, nil, nil) })

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-resumable-node", "wsr-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, wsName, v1alpha1.WorkspaceRetention{})

	// 1) A claim with principal sa-a does work and checkpoints on terminate.
	checkpointClaim := makeBoundClaim(t, "wsr-cp", wsName, v1alpha1.SandboxClaimSpec{
		NodeName:              "ws-resumable-node",
		ServiceAccount:        principal,
		CheckpointOnTerminate: true,
		Timeout:               &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, "wsr-cp-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "wsr-cp-claim", v1alpha1.SandboxTerminated)

	// 2) The new revision pairs the disk manifest with the memory snapshot, bound
	// to the capturing principal. This is the disk+memory pairing.
	ws := waitWorkspace(t, wsName, func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != "" && ws.Status.Revisions >= 1
	}, "head advanced after checkpoint")

	var head v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: ws.Status.Head}, &head); err != nil {
		t.Fatalf("get head revision: %v", err)
	}
	if head.Spec.Source.FromClaim != checkpointClaim.Name {
		t.Fatalf("head fromClaim = %q, want %q", head.Spec.Source.FromClaim, checkpointClaim.Name)
	}
	if head.Spec.ContentManifest != string(headDigest) {
		t.Fatalf("head contentManifest = %q, want disk digest %q", head.Spec.ContentManifest, headDigest)
	}
	if head.Spec.MemorySnapshotRef == nil || *head.Spec.MemorySnapshotRef != snapRef {
		t.Fatalf("head memorySnapshotRef = %v, want %q", head.Spec.MemorySnapshotRef, snapRef)
	}
	if head.Spec.MemorySnapshotPrincipal == nil || *head.Spec.MemorySnapshotPrincipal != principal {
		t.Fatalf("head memorySnapshotPrincipal = %v, want %q (the principal binding)", head.Spec.MemorySnapshotPrincipal, principal)
	}

	// 3) The workspace head is resumable (its snapshot exists, principal-bound).
	waitWorkspace(t, wsName, func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head == head.Name && ws.Status.Resumable
	}, "head resumable")

	// 4) A NEW claim with the SAME principal resumes from the checkpoint: the
	// resume seam is invoked with the paired ref, and the content is hydrated too
	// (the disk+memory pairing is restored together). It also checkpoints on its
	// own terminate so the workspace head stays snapshot-paired to sa-a when the
	// cross-principal claim below arrives (otherwise a plain terminate would
	// advance the head to a non-resumable revision and the refusal would never be
	// exercised).
	makeBoundClaim(t, "wsr-resume", wsName, v1alpha1.SandboxClaimSpec{
		NodeName:              "ws-resumable-node",
		ServiceAccount:        principal,
		CheckpointOnTerminate: true,
		Timeout:               &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, "wsr-resume-claim", v1alpha1.SandboxReady)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if rr.calledWith(snapRef, principal) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !rr.calledWith(snapRef, principal) {
		t.Fatal("same-principal claim did not resume the paired memory snapshot within 15s")
	}
	// The content half was hydrated alongside the memory restore.
	hydMu.Lock()
	hydratedSameP := hydratedFor[principal]
	hydMu.Unlock()
	if !hydratedSameP {
		t.Fatal("same-principal resume did not also hydrate the workspace content (disk+memory must be paired)")
	}
	// Let the resume claim terminate (re-checkpointing, bound to sa-a) so it
	// releases the single-writer lock and leaves a resumable, sa-a-bound head for
	// the cross-principal claim below.
	waitBoundPhase(t, "wsr-resume-claim", v1alpha1.SandboxTerminated)

	// Confirm the new head is still resumable and bound to sa-a, so the intruder
	// genuinely faces a cross-principal resumable head (not a non-resumable one).
	resumedHead := waitWorkspace(t, wsName, func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != "" && ws.Status.Head != head.Name && ws.Status.Resumable
	}, "resumable head after same-principal re-checkpoint")
	var head2 v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: resumedHead.Status.Head}, &head2); err != nil {
		t.Fatalf("get re-checkpointed head: %v", err)
	}
	if head2.Spec.MemorySnapshotPrincipal == nil || *head2.Spec.MemorySnapshotPrincipal != principal {
		t.Fatalf("re-checkpointed head principal = %v, want %q", head2.Spec.MemorySnapshotPrincipal, principal)
	}

	// 5) SECURITY: a claim with a DIFFERENT principal is REFUSED. The
	// cross-principal resume never happens (the resume seam is never called for
	// sa-b), and the intruder's claim is never hydrated (fail-closed): a memory
	// image carries secrets-in-RAM and is never served across principals.
	makeBoundClaim(t, "wsr-intruder", wsName, v1alpha1.SandboxClaimSpec{
		NodeName:       "ws-resumable-node",
		ServiceAccount: intruder,
		Timeout:        &metav1.Duration{Duration: 4 * time.Second},
	})
	waitBoundPhase(t, "wsr-intruder-claim", v1alpha1.SandboxReady)

	// Give the reconciler ample time to (not) resume. The refusal is an error that
	// requeues; the resume seam must stay untouched for the intruder principal and
	// the intruder's content must never be hydrated.
	refusalDeadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(refusalDeadline) {
		if rr.sawPrincipal(intruder) {
			t.Fatal("cross-principal resume LEAKED: the resume seam was invoked for the intruder principal; a memory image must never be served across principals")
		}
		hydMu.Lock()
		intruderHydrated := hydratedFor[intruder]
		hydMu.Unlock()
		if intruderHydrated {
			t.Fatal("cross-principal claim was hydrated despite the resume refusal; the refusal must be fail-closed (no content restore past a cross-principal denial)")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The intruder claim must NOT have stamped the hydrated annotation: the
	// activation never completed past the cross-principal refusal.
	var intruder2 v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "wsr-intruder-claim"}, &intruder2); err == nil {
		if _, done := intruder2.Annotations["mitos.run/workspace-hydrated-head"]; done {
			t.Fatal("cross-principal claim stamped the hydrated annotation; the refusal did not fail closed")
		}
	}
}
