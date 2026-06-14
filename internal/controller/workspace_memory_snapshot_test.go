package controller

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
)

func adapterClaim(name, sa string) *v1alpha1.SandboxClaim {
	c := &v1alpha1.SandboxClaim{}
	c.Name = name
	c.Spec.ServiceAccount = sa
	return c
}

// TestWorkspaceMemorySnapshotAdapterCheckpointBindsPrincipal asserts the adapter
// stamps the capturing claim's principal onto the snapshot result, so the new
// revision's MemorySnapshotPrincipal binds the image to that principal.
func TestWorkspaceMemorySnapshotAdapterCheckpointBindsPrincipal(t *testing.T) {
	a := &WorkspaceMemorySnapshotAdapter{
		CheckpointLiveVM: func(_ context.Context, _ *v1alpha1.SandboxClaim) (string, error) {
			return "snap-x", nil
		},
	}
	res, err := a.Checkpoint(context.Background(), adapterClaim("c1", "sa-a"))
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if res.Ref != "snap-x" {
		t.Fatalf("ref = %q, want snap-x", res.Ref)
	}
	if res.Principal != "sa-a" {
		t.Fatalf("principal = %q, want sa-a (the capturing claim's ServiceAccount)", res.Principal)
	}
}

// TestWorkspaceMemorySnapshotAdapterCheckpointFailsLoudWithoutLiveVM asserts that
// when the live-VM image is not wired (the bare-metal tail absent), Checkpoint
// fails loud rather than fabricating a resumable revision.
func TestWorkspaceMemorySnapshotAdapterCheckpointFailsLoudWithoutLiveVM(t *testing.T) {
	a := &WorkspaceMemorySnapshotAdapter{} // CheckpointLiveVM nil
	_, err := a.Checkpoint(context.Background(), adapterClaim("c1", "sa-a"))
	if err == nil {
		t.Fatal("Checkpoint without a live-VM image must fail loud, not fabricate a snapshot")
	}
	if !strings.Contains(err.Error(), "live-VM") {
		t.Fatalf("error must name the missing live-VM image: %v", err)
	}
}

// TestWorkspaceMemorySnapshotAdapterResumeRefusesCrossPrincipal asserts the
// adapter is a second line of defense: it refuses to restore a snapshot bound to
// a different principal even if a caller bypassed the upstream refusal.
func TestWorkspaceMemorySnapshotAdapterResumeRefusesCrossPrincipal(t *testing.T) {
	restored := false
	a := &WorkspaceMemorySnapshotAdapter{
		CheckpointLiveVM: func(_ context.Context, _ *v1alpha1.SandboxClaim) (string, error) { return "snap-x", nil },
		RestoreLiveVM: func(_ context.Context, _ *v1alpha1.SandboxClaim, _ string) error {
			restored = true
			return nil
		},
	}
	// sa-a captures the snapshot.
	if _, err := a.Checkpoint(context.Background(), adapterClaim("c1", "sa-a")); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// sa-a may resume it.
	if err := a.Resume(context.Background(), adapterClaim("c2", "sa-a"), "snap-x"); err != nil {
		t.Fatalf("same-principal Resume must succeed: %v", err)
	}
	if !restored {
		t.Fatal("same-principal Resume did not invoke the live-VM restore")
	}
	// sa-b must be REFUSED, and the restore must not run.
	restored = false
	err := a.Resume(context.Background(), adapterClaim("c3", "sa-b"), "snap-x")
	if err == nil {
		t.Fatal("cross-principal Resume must be refused")
	}
	if restored {
		t.Fatal("cross-principal Resume invoked the live-VM restore; a memory image must never be served across principals")
	}
}

// TestWorkspaceMemorySnapshotAdapterExistsPrincipalBound asserts Exists reports
// absent for a cross-principal ref and fails closed when the store check is not
// wired.
func TestWorkspaceMemorySnapshotAdapterExistsPrincipalBound(t *testing.T) {
	present := map[string]bool{"snap-x": true}
	a := &WorkspaceMemorySnapshotAdapter{
		CheckpointLiveVM: func(_ context.Context, _ *v1alpha1.SandboxClaim) (string, error) { return "snap-x", nil },
		SnapshotPresent: func(_ context.Context, ref string) (bool, error) {
			return present[ref], nil
		},
	}
	if _, err := a.Checkpoint(context.Background(), adapterClaim("c1", "sa-a")); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	ok, err := a.Exists(context.Background(), "snap-x", "sa-a")
	if err != nil || !ok {
		t.Fatalf("same-principal Exists = (%v,%v), want (true,nil)", ok, err)
	}
	ok, err = a.Exists(context.Background(), "snap-x", "sa-b")
	if err != nil || ok {
		t.Fatalf("cross-principal Exists = (%v,%v), want (false,nil): a cross-principal ref is never resumable", ok, err)
	}

	// No store check wired: fail closed (absent), even for the bound principal.
	b := &WorkspaceMemorySnapshotAdapter{}
	ok, err = b.Exists(context.Background(), "snap-x", "sa-a")
	if err != nil || ok {
		t.Fatalf("unwired Exists = (%v,%v), want (false,nil) fail-closed", ok, err)
	}
}
