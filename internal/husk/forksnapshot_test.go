package husk

import (
	"context"
	"path/filepath"
	"testing"
)

// activeStubWithFake returns a Stub already in StateActive holding the given fake
// vmm, so ForkSnapshot can be exercised without a real Activate. It uses the same
// fake vmm type the stub_test.go uses.
func activeStubWithFake(f *fakeVMM) *Stub {
	return &Stub{state: StateActive, vm: f}
}

func TestForkSnapshotPausesSnapshotsResumes(t *testing.T) {
	f := &fakeVMM{}
	s := activeStubWithFake(f)

	dir := t.TempDir()
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: dir,
		PauseSource: false,
	})
	if err != nil {
		t.Fatalf("ForkSnapshot: %v", err)
	}
	if !res.OK {
		t.Fatalf("ForkSnapshot not OK: %+v", res)
	}
	if !f.paused {
		t.Fatalf("source VM was not paused before snapshot")
	}
	if !f.resumed {
		t.Fatalf("source VM was not resumed after snapshot (PauseSource=false)")
	}
	if f.snapMem != filepath.Join(dir, "mem") || f.snapState != filepath.Join(dir, "vmstate") {
		t.Fatalf("snapshot written to wrong paths: mem=%s state=%s", f.snapMem, f.snapState)
	}
	if s.State() != StateActive {
		t.Fatalf("stub must remain active after fork snapshot, got %s", s.State())
	}
}

func TestForkSnapshotHonorsPauseSource(t *testing.T) {
	f := &fakeVMM{}
	s := activeStubWithFake(f)
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: t.TempDir(),
		PauseSource: true,
	})
	if err != nil || !res.OK {
		t.Fatalf("ForkSnapshot: err=%v res=%+v", err, res)
	}
	if !f.paused {
		t.Fatalf("source VM was not paused")
	}
	if f.resumed {
		t.Fatalf("PauseSource=true must leave the source paused, but it was resumed")
	}
}

func TestForkSnapshotRequiresActiveState(t *testing.T) {
	f := &fakeVMM{}
	s := &Stub{state: StateDormant, vm: f}
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "f", SnapshotDir: t.TempDir()})
	if err == nil || res.OK {
		t.Fatalf("ForkSnapshot must refuse a non-active stub: err=%v res=%+v", err, res)
	}
}

func TestForkSnapshotFailClosedOnSnapshotError(t *testing.T) {
	f := &fakeVMM{snapErr: errSnap}
	s := activeStubWithFake(f)
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "f", SnapshotDir: t.TempDir()})
	if err == nil || res.OK {
		t.Fatalf("snapshot error must fail closed: err=%v res=%+v", err, res)
	}
}

func TestForkSnapshotConfinedToForksDir(t *testing.T) {
	forks := t.TempDir()
	f := &fakeVMM{}
	s := &Stub{state: StateActive, vm: f, forksDir: forks}

	// A dir WITHIN the configured forks dir is accepted.
	inside := filepath.Join(forks, "fork-1")
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: inside})
	if err != nil || !res.OK {
		t.Fatalf("fork snapshot inside forks dir must succeed: err=%v res=%+v", err, res)
	}

	// A dir OUTSIDE the forks dir (here a traversal escape) is refused fail-closed
	// and the VM is never paused.
	f2 := &fakeVMM{}
	s2 := &Stub{state: StateActive, vm: f2, forksDir: forks}
	escape := filepath.Join(forks, "..", "escape")
	res2, err2 := s2.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "x", SnapshotDir: escape})
	if err2 == nil || res2.OK {
		t.Fatalf("fork snapshot outside forks dir must fail closed: err=%v res=%+v", err2, res2)
	}
	if f2.paused {
		t.Fatalf("an out-of-bounds fork snapshot must not pause the VM")
	}
}

func TestRemoveForkSnapshotConfinedToForksDir(t *testing.T) {
	forks := t.TempDir()
	s := &Stub{state: StateActive, vm: &fakeVMM{}, forksDir: forks}
	if err := s.RemoveForkSnapshot(ForkSnapshotRequest{SnapshotDir: filepath.Join(forks, "..", "escape")}); err == nil {
		t.Fatalf("remove fork snapshot outside forks dir must be refused")
	}
}
