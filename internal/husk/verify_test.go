package husk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/snapcompat"
)

// testEnv is the host environment the verify tests treat as "this node".
func testEnv() snapcompat.Environment {
	return snapcompat.Environment{
		FormatVersions: []int{cas.CurrentSnapshotFormatVersion},
		VMMVersion:     "v1.15.0",
		CPUModel:       "Intel Xeon Platinum",
		KernelVersion:  "6.1.0",
	}
}

// buildSnapshot writes a mem+vmstate snapshot under a fresh snapshot dir, records
// its CAS manifest with the given metadata, writes the manifest file, and returns
// the snapshot dir, the manifest file path, and the recorded digest.
func buildSnapshot(t *testing.T, meta cas.Metadata) (snapshotDir, manifestPath string, digest cas.Digest) {
	t.Helper()
	root := t.TempDir()
	snapshotDir = filepath.Join(root, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	memPath := filepath.Join(snapshotDir, "mem")
	statePath := filepath.Join(snapshotDir, "vmstate")
	if err := os.WriteFile(memPath, bytes.Repeat([]byte{0xAA}, 5<<20), 0o644); err != nil {
		t.Fatalf("write mem: %v", err)
	}
	if err := os.WriteFile(statePath, bytes.Repeat([]byte{0xBB}, 1<<20), 0o644); err != nil {
		t.Fatalf("write vmstate: %v", err)
	}
	// Include a rootfs in the manifest that is NOT in the snapshot dir, mirroring
	// production: the husk pod mounts only snapshot/{mem,vmstate}, not the rootfs,
	// yet the rootfs is part of the recorded manifest identity.
	rootfsPath := filepath.Join(root, "rootfs.ext4")
	if err := os.WriteFile(rootfsPath, bytes.Repeat([]byte{0xCC}, 2<<20), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	m, err := cas.BuildManifest(map[string]string{
		"mem":     memPath,
		"vmstate": statePath,
		"rootfs":  rootfsPath,
	}, meta)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	manifestPath = filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifestPath, m.Canonical(), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return snapshotDir, manifestPath, m.Digest()
}

func compatMeta() cas.Metadata {
	env := testEnv()
	return cas.Metadata{
		SnapshotFormatVersion: cas.CurrentSnapshotFormatVersion,
		VMMVersion:            env.VMMVersion,
		CPUModel:              env.CPUModel,
		KernelVersion:         env.KernelVersion,
	}
}

// newVerifyStub builds a stub wired to the PRODUCTION verifier (not the no-op
// test seam), so the fail-closed gate is exercised end-to-end through Activate.
func newVerifyStub(t *testing.T, vm *fakeVMM, manifestPath string) *Stub {
	t.Helper()
	return New(firecracker.VMConfig{ID: "husk-verify-test"}, Options{
		Start:        func(firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:        readyOK,
		Notify:       (&fakeNotifier{}).notify,
		ManifestPath: manifestPath,
		Env:          testEnv(),
	})
}

// TestActivateVerifyProceedsWhenDigestAndCompatMatch: a snapshot whose on-disk
// files re-hash to the recorded manifest, whose manifest digest matches the
// expected digest, and whose recorded environment is compatible, loads.
func TestActivateVerifyProceedsWhenDigestAndCompatMatch(t *testing.T) {
	snapDir, manifestPath, digest := buildSnapshot(t, compatMeta())
	vm := &fakeVMM{}
	s := newVerifyStub(t, vm, manifestPath)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir:    snapDir,
		ExpectedDigest: string(digest),
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate not OK: %s", res.Error)
	}
	if vm.loadCalls != 1 {
		t.Fatalf("expected snapshot to load once, got %d load calls", vm.loadCalls)
	}
}

// TestActivateVerifyRefusesTamperedSnapshot: a mem file tampered on disk after
// the manifest was recorded makes the re-hash diverge; Activate refuses and does
// NOT load.
func TestActivateVerifyRefusesTamperedSnapshot(t *testing.T) {
	snapDir, manifestPath, digest := buildSnapshot(t, compatMeta())
	// Tamper the loaded mem image AFTER the manifest was recorded.
	if err := os.WriteFile(filepath.Join(snapDir, "mem"), bytes.Repeat([]byte{0xAB}, 5<<20), 0o644); err != nil {
		t.Fatalf("tamper mem: %v", err)
	}
	vm := &fakeVMM{}
	s := newVerifyStub(t, vm, manifestPath)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir:    snapDir,
		ExpectedDigest: string(digest),
	})
	if err == nil {
		t.Fatal("expected activate to fail closed on a tampered snapshot")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK for a tampered snapshot")
	}
	if vm.loadCalls != 0 {
		t.Fatalf("fail closed: a tampered snapshot must NOT be loaded, got %d load calls", vm.loadCalls)
	}
	if s.State() == StateActive {
		t.Fatalf("state must not be active after a refused activate, got %s", s.State())
	}
}

// TestActivateVerifyRefusesWrongExpectedDigest: a manifest whose own digest does
// not match the controller-passed ExpectedDigest is refused (the mounted manifest
// is not the one the controller activated).
func TestActivateVerifyRefusesWrongExpectedDigest(t *testing.T) {
	snapDir, manifestPath, _ := buildSnapshot(t, compatMeta())
	vm := &fakeVMM{}
	s := newVerifyStub(t, vm, manifestPath)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wrong := "0000000000000000000000000000000000000000000000000000000000000000"
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir:    snapDir,
		ExpectedDigest: wrong,
	})
	if err == nil || res.OK || vm.loadCalls != 0 {
		t.Fatalf("expected fail-closed refusal on digest mismatch: err=%v ok=%v loads=%d", err, res.OK, vm.loadCalls)
	}
}

// TestActivateVerifyRefusesIncompatibleSnapshot: a snapshot whose recorded
// producing Firecracker version differs from this node's is refused by
// snapcompat, even though its integrity is intact.
func TestActivateVerifyRefusesIncompatibleSnapshot(t *testing.T) {
	meta := compatMeta()
	meta.VMMVersion = "v9.9.9" // produced by a different Firecracker than this node.
	snapDir, manifestPath, digest := buildSnapshot(t, meta)
	vm := &fakeVMM{}
	s := newVerifyStub(t, vm, manifestPath)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir:    snapDir,
		ExpectedDigest: string(digest),
	})
	if err == nil {
		t.Fatal("expected activate to fail closed on an incompatible snapshot")
	}
	if res.OK || vm.loadCalls != 0 {
		t.Fatalf("fail closed: an incompatible snapshot must NOT be loaded: ok=%v loads=%d", res.OK, vm.loadCalls)
	}
}

// TestActivateVerifyRefusesIncompatibleFormat: a snapshot recorded with an
// unsupported format version is refused.
func TestActivateVerifyRefusesIncompatibleFormat(t *testing.T) {
	meta := compatMeta()
	meta.SnapshotFormatVersion = cas.CurrentSnapshotFormatVersion + 999
	snapDir, manifestPath, digest := buildSnapshot(t, meta)
	vm := &fakeVMM{}
	s := newVerifyStub(t, vm, manifestPath)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir:    snapDir,
		ExpectedDigest: string(digest),
	})
	if err == nil || res.OK || vm.loadCalls != 0 {
		t.Fatalf("expected fail-closed refusal on incompatible format: err=%v ok=%v loads=%d", err, res.OK, vm.loadCalls)
	}
}

// TestActivateVerifyRefusesMissingExpectedDigest: with verify enforced (no escape
// hatch) an activate that carries no ExpectedDigest is refused before any load.
func TestActivateVerifyRefusesMissingExpectedDigest(t *testing.T) {
	snapDir, manifestPath, _ := buildSnapshot(t, compatMeta())
	vm := &fakeVMM{}
	s := newVerifyStub(t, vm, manifestPath)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: snapDir})
	if err == nil || res.OK || vm.loadCalls != 0 {
		t.Fatalf("expected fail-closed refusal on missing digest: err=%v ok=%v loads=%d", err, res.OK, vm.loadCalls)
	}
}

// TestActivateAllowUnverifiedEscapeHatch: with the development escape hatch the
// verifier warns and proceeds even with no digest, mirroring forkd's
// --allow-unverified-snapshots, so the CI latency/handshake phases still run.
func TestActivateAllowUnverifiedEscapeHatch(t *testing.T) {
	snapDir, manifestPath, _ := buildSnapshot(t, compatMeta())
	vm := &fakeVMM{}
	s := New(firecracker.VMConfig{ID: "husk-verify-test"}, Options{
		Start:           func(firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:           readyOK,
		Notify:          (&fakeNotifier{}).notify,
		ManifestPath:    manifestPath,
		Env:             testEnv(),
		AllowUnverified: true,
	})
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: snapDir})
	if err != nil {
		t.Fatalf("Activate with escape hatch: %v", err)
	}
	if !res.OK || vm.loadCalls != 1 {
		t.Fatalf("escape hatch must proceed: ok=%v loads=%d", res.OK, vm.loadCalls)
	}
}
