package controller_test

// Envtest coverage for forking ACROSS workspaces (W4 user-facing surface).
//
// A fork branches a committed revision of one workspace into another workspace,
// so the new revision carries a CROSS-workspace fromWorkspaceRevision edge. The
// reconciler must accept that edge, commit the forked revision (it shares the
// parent's content manifest), and advance the destination workspace head, so a
// sandbox bound to the destination hydrates the forked content. A regression
// guard: the lineage validator previously rejected any cross-workspace edge,
// which left every fork Pending forever (its head never advanced) and a bound
// sandbox saw an empty workspace.

import (
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestForkCommitsCrossWorkspaceRevisionAndAdvancesHead(t *testing.T) {
	makeWorkspace(t, "wsfk-src", v1alpha1.WorkspaceRetention{})
	makeWorkspace(t, "wsfk-branch", v1alpha1.WorkspaceRetention{})

	// A committed revision in the source workspace: the reconciler commits a
	// revision whose ContentManifest is a valid content-addressed digest.
	manifest := testManifest(0xab)
	srcRev := &v1alpha1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "wsfk-src-",
			Namespace:    "default",
			Labels:       map[string]string{controller.WorkspaceLabel: "wsfk-src"},
		},
		Spec: v1alpha1.WorkspaceRevisionSpec{
			WorkspaceRef:    v1alpha1.LocalObjectReference{Name: "wsfk-src"},
			ContentManifest: manifest,
		},
	}
	if err := k8sClient.Create(ctx, srcRev); err != nil {
		t.Fatalf("create source revision: %v", err)
	}
	src := waitWorkspace(t, "wsfk-src", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != ""
	}, "source head committed")

	// Fork the source head into the branch workspace (a cross-workspace edge).
	v := &controller.WorkspaceVerbs{Client: k8sClient}
	if _, err := v.Fork(ctx, "default", "wsfk-src", src.Status.Head, "wsfk-branch"); err != nil {
		t.Fatalf("fork: %v", err)
	}

	// The branch head must advance to the forked, COMMITTED revision sharing the
	// source content manifest. Before the fix the cross-workspace lineage was
	// rejected, so the fork stayed Pending and Status.Head never advanced.
	branch := waitWorkspace(t, "wsfk-branch", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != ""
	}, "branch head advanced to the fork")

	var head v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: branch.Status.Head}, &head); err != nil {
		t.Fatalf("get branch head: %v", err)
	}
	if head.Status.Phase != v1alpha1.WorkspaceRevisionCommitted {
		t.Fatalf("branch head phase = %q, want Committed", head.Status.Phase)
	}
	if head.Spec.ContentManifest != manifest {
		t.Fatalf("branch head manifest = %q, want the shared source manifest %q", head.Spec.ContentManifest, manifest)
	}
	if src := head.Spec.Source.FromWorkspaceRevision; src == nil || src.Workspace != "wsfk-src" {
		t.Fatalf("branch head lineage = %+v, want fromWorkspaceRevision in wsfk-src", head.Spec.Source.FromWorkspaceRevision)
	}
}
