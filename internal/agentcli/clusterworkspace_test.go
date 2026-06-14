package agentcli

import (
	"context"
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClusterCreateAndLogWorkspace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	b := NewClusterBackend(c, "ns", nil)
	ws := b.Workspace()
	if err := ws.CreateWorkspace(context.Background(), "proj-x"); err != nil {
		t.Fatalf("create: %v", err)
	}
	var got v1alpha1.Workspace
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "proj-x"}, &got); err != nil {
		t.Fatalf("workspace not created: %v", err)
	}

	rev := &v1alpha1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "proj-x-1", Namespace: "ns"},
		Spec:       v1alpha1.WorkspaceRevisionSpec{WorkspaceRef: v1alpha1.LocalObjectReference{Name: "proj-x"}, Source: v1alpha1.RevisionSource{FromClaim: "c1"}},
		Status:     v1alpha1.WorkspaceRevisionStatus{Phase: v1alpha1.WorkspaceRevisionCommitted},
	}
	if err := c.Create(context.Background(), rev); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	revs, err := ws.Log(context.Background(), "proj-x")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(revs) != 1 || revs[0].Name != "proj-x-1" || revs[0].Lineage != "fromClaim:c1" {
		t.Fatalf("unexpected log: %+v", revs)
	}
}

func TestClusterForkRejectsUncommittedWithRejectionError(t *testing.T) {
	parent := &v1alpha1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "proj-x-1", Namespace: "ns"},
		Spec:       v1alpha1.WorkspaceRevisionSpec{WorkspaceRef: v1alpha1.LocalObjectReference{Name: "proj-x"}},
		Status:     v1alpha1.WorkspaceRevisionStatus{Phase: v1alpha1.WorkspaceRevisionPending},
	}
	dst := &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "branch", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(parent, dst).Build()
	b := NewClusterBackend(c, "ns", nil)

	_, err := b.Workspace().Fork(context.Background(), "proj-x", "proj-x-1", "branch")
	if err == nil {
		t.Fatalf("want rejection, got nil")
	}
}
