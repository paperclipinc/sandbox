package controller_test

// Coverage for the Workspace reconciler (W4, the declarative foundation).
//
// The suite manager runs a WorkspaceReconciler (registered in suite_test.go).
// These tests create a Workspace and its WorkspaceRevisions through the API and
// assert the reconciler converges the workspace status (head, the committed
// revision count, resumable) and the revision phases, enforces retention
// pruning over the DAG without pruning the head or a referenced ancestor,
// validates the FromWorkspaceRevision lineage, garbage-collects revisions when
// the workspace is deleted (owner ref), and keeps a committed revision's
// ContentManifest immutable. No data moves: a revision's ContentManifest is the
// test seam that stands in for dehydrate.

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// a valid content-addressed digest (64 lowercase hex chars) for the test seam.
func testManifest(seed byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed
	}
	return fmt.Sprintf("%x", b)
}

func makeWorkspace(t *testing.T, name string, retention v1alpha1.WorkspaceRetention) *v1alpha1.Workspace {
	t.Helper()
	ws := &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.WorkspaceSpec{
			Store:     v1alpha1.WorkspaceStore{ObjectStorageRef: "default"},
			Retention: retention,
		},
	}
	if err := k8sClient.Create(ctx, ws); err != nil {
		t.Fatalf("create workspace %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ws) })
	return ws
}

func makeRevision(t *testing.T, name, wsName, manifest string, snapshot *string, parent *v1alpha1.WorkspaceRevisionRef) *v1alpha1.WorkspaceRevision {
	t.Helper()
	rev := &v1alpha1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.WorkspaceRevisionSpec{
			WorkspaceRef:      v1alpha1.LocalObjectReference{Name: wsName},
			ContentManifest:   manifest,
			MemorySnapshotRef: snapshot,
			Source:            v1alpha1.RevisionSource{FromWorkspaceRevision: parent},
		},
	}
	if err := k8sClient.Create(ctx, rev); err != nil {
		t.Fatalf("create revision %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rev) })
	return rev
}

func waitWorkspace(t *testing.T, name string, cond func(*v1alpha1.Workspace) bool, what string) *v1alpha1.Workspace {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var ws v1alpha1.Workspace
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &ws); err == nil {
			if cond(&ws) {
				return &ws
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("workspace %s did not reach %s; last status: head=%q revisions=%d resumable=%v gen=%d observed=%d",
		name, what, ws.Status.Head, ws.Status.Revisions, ws.Status.Resumable, ws.Generation, ws.Status.ObservedGeneration)
	return nil
}

func readyCondition(ws *v1alpha1.Workspace) *metav1.Condition {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == "Ready" {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

func TestWorkspaceReconcilesToPendingWhenEmpty(t *testing.T) {
	makeWorkspace(t, "ws-empty", v1alpha1.WorkspaceRetention{})

	ws := waitWorkspace(t, "ws-empty", func(ws *v1alpha1.Workspace) bool {
		c := readyCondition(ws)
		return c != nil && c.Reason == controller.ReasonWorkspacePending && ws.Status.ObservedGeneration == ws.Generation
	}, "Pending with observedGeneration")

	if ws.Status.Head != "" || ws.Status.Revisions != 0 || ws.Status.Resumable {
		t.Fatalf("empty workspace should have no head/revisions/resumable, got head=%q revisions=%d resumable=%v",
			ws.Status.Head, ws.Status.Revisions, ws.Status.Resumable)
	}
}

func TestWorkspaceHeadAndCountFromCommittedRevisions(t *testing.T) {
	makeWorkspace(t, "ws-head", v1alpha1.WorkspaceRetention{})

	// Two committed revisions; the second is created later, so it is the head
	// (ordering is by creationTimestamp then name).
	makeRevision(t, "ws-head-r1", "ws-head", testManifest(0x11), nil, nil)
	time.Sleep(1100 * time.Millisecond) // distinct creationTimestamp (1s resolution)
	makeRevision(t, "ws-head-r2", "ws-head", testManifest(0x22), nil, nil)

	ws := waitWorkspace(t, "ws-head", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Revisions == 2 && ws.Status.Head == "ws-head-r2"
	}, "head ws-head-r2 with 2 revisions")

	c := readyCondition(ws)
	if c == nil || c.Reason != controller.ReasonWorkspaceReady || c.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True reason WorkspaceReady, got %+v", c)
	}
	if ws.Status.Resumable {
		t.Fatalf("no revision has a memory snapshot; resumable must be false")
	}

	// The revisions themselves transitioned to Committed.
	var r1 v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-head-r1"}, &r1); err != nil {
		t.Fatal(err)
	}
	if r1.Status.Phase != v1alpha1.WorkspaceRevisionCommitted {
		t.Fatalf("revision r1 should be Committed, got %q", r1.Status.Phase)
	}
}

func TestWorkspaceResumableWhenHeadHasMemorySnapshot(t *testing.T) {
	// The resumable status verifies the paired memory snapshot still exists
	// (fail-closed by default). Install an existence fake that confirms this
	// revision's snapshot; reset on cleanup so a later reconcile fail-closes.
	// Other workspaces are unaffected: their revisions carry no
	// memorySnapshotRef, so the head is never resumable regardless of this fake.
	snap := "mem-snap-abc"
	setMemSnapshot(nil, nil, func(_ context.Context, ref, _ string) (bool, error) {
		return ref == snap, nil
	})
	t.Cleanup(func() { setMemSnapshot(nil, nil, nil) })

	makeWorkspace(t, "ws-resume", v1alpha1.WorkspaceRetention{})
	makeRevision(t, "ws-resume-r1", "ws-resume", testManifest(0x33), &snap, nil)

	waitWorkspace(t, "ws-resume", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head == "ws-resume-r1" && ws.Status.Resumable
	}, "resumable head")
}

func TestWorkspaceRetentionPrunesOldRevisionsProtectingHeadLineage(t *testing.T) {
	// keep 2, minAge 0 so every over-count revision is immediately eligible.
	makeWorkspace(t, "ws-retain", v1alpha1.WorkspaceRetention{
		Revisions: 2,
		MinAge:    &metav1.Duration{Duration: 0},
	})

	// A lineage chain r1 -> r2 -> r3 -> r4 (each forks the previous). r4 is the
	// head. With keep=2, two of the four must be pruned, but r1/r2/r3 are all
	// ancestors of the head r4 and must be PROTECTED. Only a non-ancestor old
	// revision is eligible: add a detached old revision r0 (a root, not on r4's
	// lineage) that is the one prune target.
	makeRevision(t, "ws-retain-r0", "ws-retain", testManifest(0x01), nil, nil)
	time.Sleep(1100 * time.Millisecond)
	makeRevision(t, "ws-retain-r1", "ws-retain", testManifest(0x02), nil, nil)
	time.Sleep(1100 * time.Millisecond)
	makeRevision(t, "ws-retain-r2", "ws-retain", testManifest(0x03), nil,
		&v1alpha1.WorkspaceRevisionRef{Workspace: "ws-retain", Revision: "ws-retain-r1"})
	time.Sleep(1100 * time.Millisecond)
	makeRevision(t, "ws-retain-r3", "ws-retain", testManifest(0x04), nil,
		&v1alpha1.WorkspaceRevisionRef{Workspace: "ws-retain", Revision: "ws-retain-r2"})

	// Head is r3 (latest). Its lineage is r3 -> r2 -> r1, all protected. r0 is
	// the oldest non-ancestor; with keep=2 and 4 committed, the eligible old
	// non-protected revision (r0) is pruned.
	waitWorkspace(t, "ws-retain", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head == "ws-retain-r3"
	}, "head r3")

	// r0 must be pruned; r1, r2, r3 (the head lineage) must survive.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var r0 v1alpha1.WorkspaceRevision
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-retain-r0"}, &r0)
		if apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	var r0 v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-retain-r0"}, &r0); !apierrors.IsNotFound(err) {
		t.Fatalf("r0 (oldest non-ancestor) should be pruned, but it is still present (err=%v)", err)
	}
	for _, keep := range []string{"ws-retain-r1", "ws-retain-r2", "ws-retain-r3"} {
		var rev v1alpha1.WorkspaceRevision
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: keep}, &rev); err != nil {
			t.Fatalf("head-lineage revision %s must NOT be pruned, but Get failed: %v", keep, err)
		}
	}
}

func TestWorkspaceRetentionRespectsMinAge(t *testing.T) {
	// keep 1 but minAge is large: no revision is old enough to prune, so nothing
	// is pruned even though the count is over.
	makeWorkspace(t, "ws-minage", v1alpha1.WorkspaceRetention{
		Revisions: 1,
		MinAge:    &metav1.Duration{Duration: time.Hour},
	})
	makeRevision(t, "ws-minage-r1", "ws-minage", testManifest(0x05), nil, nil)
	time.Sleep(1100 * time.Millisecond)
	makeRevision(t, "ws-minage-r2", "ws-minage", testManifest(0x06), nil, nil)

	waitWorkspace(t, "ws-minage", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Revisions == 2
	}, "2 revisions")

	// Give the reconciler time to (not) prune, then assert both survive.
	time.Sleep(2 * time.Second)
	for _, name := range []string{"ws-minage-r1", "ws-minage-r2"} {
		var rev v1alpha1.WorkspaceRevision
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &rev); err != nil {
			t.Fatalf("minAge should protect %s from pruning, but Get failed: %v", name, err)
		}
	}
}

func TestWorkspaceRevisionLineageValidation(t *testing.T) {
	makeWorkspace(t, "ws-lineage", v1alpha1.WorkspaceRetention{})

	// A revision whose FromWorkspaceRevision points at a nonexistent parent must
	// NOT commit, and the workspace goes Degraded.
	makeRevision(t, "ws-lineage-bad", "ws-lineage", testManifest(0x07), nil,
		&v1alpha1.WorkspaceRevisionRef{Workspace: "ws-lineage", Revision: "does-not-exist"})

	waitWorkspace(t, "ws-lineage", func(ws *v1alpha1.Workspace) bool {
		c := readyCondition(ws)
		return c != nil && c.Reason == controller.ReasonWorkspaceDegraded
	}, "Degraded on broken lineage")

	var bad v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-lineage-bad"}, &bad); err != nil {
		t.Fatal(err)
	}
	if bad.Status.Phase == v1alpha1.WorkspaceRevisionCommitted {
		t.Fatalf("a revision with a broken lineage edge must not commit")
	}
}

func TestWorkspaceRevisionOwnerReferencedForGC(t *testing.T) {
	// The owner reference is the mechanism that garbage-collects a revision when
	// its workspace is deleted: kube-controller-manager's GC follows the
	// controller owner ref. envtest does NOT run that GC controller, so this test
	// asserts the invariant that ENABLES the cascade (a controller owner ref to
	// the workspace, with blockOwnerDeletion), rather than the cascade itself.
	ws := makeWorkspace(t, "ws-gc", v1alpha1.WorkspaceRetention{})
	makeRevision(t, "ws-gc-r1", "ws-gc", testManifest(0x08), nil, nil)

	deadline := time.Now().Add(15 * time.Second)
	var owner *metav1.OwnerReference
	for time.Now().Before(deadline) {
		var rev v1alpha1.WorkspaceRevision
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-gc-r1"}, &rev); err == nil {
			for i := range rev.OwnerReferences {
				o := &rev.OwnerReferences[i]
				if o.Kind == "Workspace" && o.Name == "ws-gc" {
					owner = o
				}
			}
		}
		if owner != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if owner == nil {
		t.Fatalf("revision was not owner-referenced to its workspace; GC would not follow")
	}
	if owner.UID != ws.UID {
		t.Fatalf("owner ref UID %q does not match workspace UID %q", owner.UID, ws.UID)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Fatalf("owner ref must be a controller ref for GC, got controller=%v", owner.Controller)
	}
}

func TestWorkspaceRevisionContentManifestImmutable(t *testing.T) {
	makeWorkspace(t, "ws-immut", v1alpha1.WorkspaceRetention{})
	original := testManifest(0x09)
	makeRevision(t, "ws-immut-r1", "ws-immut", original, nil, nil)

	// Wait for the revision to commit.
	deadline := time.Now().Add(15 * time.Second)
	committed := false
	for time.Now().Before(deadline) {
		var rev v1alpha1.WorkspaceRevision
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-immut-r1"}, &rev); err == nil {
			if rev.Status.Phase == v1alpha1.WorkspaceRevisionCommitted {
				committed = true
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !committed {
		t.Fatalf("revision did not commit")
	}

	// Mutate the spec ContentManifest to a different valid digest. The reconciler
	// must keep the phase Committed (the guard rejects the manifest change at the
	// status level; the revision stays committed and does not flip to Pending).
	var rev v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-immut-r1"}, &rev); err != nil {
		t.Fatal(err)
	}
	rev.Spec.ContentManifest = testManifest(0x0a)
	if err := k8sClient.Update(ctx, &rev); err != nil {
		t.Fatal(err)
	}

	// The phase must remain Committed across reconciles (it never flips to
	// Pending on a changed manifest).
	time.Sleep(3 * time.Second)
	var after v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-immut-r1"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Phase != v1alpha1.WorkspaceRevisionCommitted {
		t.Fatalf("a committed revision must stay Committed (immutable), got %q", after.Status.Phase)
	}
}
