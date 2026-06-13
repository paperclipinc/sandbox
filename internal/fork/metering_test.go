package fork

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/volume"
)

// writeSizedFile creates a file of exactly size bytes (apparent size) at path.
func writeSizedFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// TestMeteringDiskSnapshotSeedCountedOnce builds two Snapshot-volume forks of
// one template that share a 100 MiB seed and have small per-fork divergence.
// The seed disk must be counted ONCE, not once per fork, and each fork's
// divergence (fork apparent size minus seed) must count as unique.
func TestMeteringDiskSnapshotSeedCountedOnce(t *testing.T) {
	const (
		mib       = int64(1024 * 1024)
		seedSize  = 100 * mib
		f1Backing = 110 * mib // 10 MiB diverged
		f2Backing = 130 * mib // 30 MiB diverged
	)

	root := t.TempDir()
	backend := volume.New(root)

	const templateID = "tmpl-A"
	spec := volume.Spec{Name: "data", MountPath: "/data", Policy: volume.ForkPolicySnapshot}

	// Seed (shared by every fork of the template), counted once.
	writeSizedFile(t, backend.TemplateVolumePath(templateID, spec.Name), seedSize)
	// Per-fork reflink backing files.
	writeSizedFile(t, backend.VolumePath("f1", spec.Name), f1Backing)
	writeSizedFile(t, backend.VolumePath("f2", spec.Name), f2Backing)

	e := &Engine{
		sandboxes:  make(map[string]*Sandbox),
		volBackend: backend,
	}
	e.sandboxes["f1"] = &Sandbox{ID: "f1", TemplateID: templateID, hasVolumes: true, volumes: []volume.Spec{spec}}
	e.sandboxes["f2"] = &Sandbox{ID: "f2", TemplateID: templateID, hasVolumes: true, volumes: []volume.Spec{spec}}

	report := e.Metering()

	// Divergence is unique: (110-100) + (130-100) = 40 MiB.
	if want := 40 * mib; report.DiskTotalUnique != want {
		t.Errorf("DiskTotalUnique = %d, want %d", report.DiskTotalUnique, want)
	}
	// Seed counted once + divergence: 100 + 40 = 140 MiB.
	if want := 140 * mib; report.DiskUsedCoWAware != want {
		t.Errorf("DiskUsedCoWAware = %d, want %d (seed once)", report.DiskUsedCoWAware, want)
	}
	// Naive double-counts the seed across both forks: (110 + 130) = 240 MiB.
	if want := 240 * mib; report.DiskUsedNaive != want {
		t.Errorf("DiskUsedNaive = %d, want %d", report.DiskUsedNaive, want)
	}
	if want := 100 * mib; report.DiskCoWSavings != want {
		t.Errorf("DiskCoWSavings = %d, want %d", report.DiskCoWSavings, want)
	}
	if len(report.Templates) != 1 || report.Templates[0].DiskSharedOnce != seedSize {
		t.Errorf("template disk shared-once = %+v, want %d", report.Templates, seedSize)
	}
}

// TestMeteringDiskFreshIsAllUnique: a Fresh volume's whole backing is unique
// (nothing shared with siblings).
func TestMeteringDiskFreshIsAllUnique(t *testing.T) {
	const mib = int64(1024 * 1024)
	root := t.TempDir()
	backend := volume.New(root)
	spec := volume.Spec{Name: "scratch", MountPath: "/scratch", Policy: volume.ForkPolicyFresh}
	writeSizedFile(t, backend.VolumePath("f1", spec.Name), 50*mib)

	e := &Engine{sandboxes: make(map[string]*Sandbox), volBackend: backend}
	e.sandboxes["f1"] = &Sandbox{ID: "f1", TemplateID: "tmpl-A", hasVolumes: true, volumes: []volume.Spec{spec}}

	report := e.Metering()
	if want := 50 * mib; report.DiskTotalUnique != want {
		t.Errorf("DiskTotalUnique = %d, want %d", report.DiskTotalUnique, want)
	}
	if report.DiskCoWSavings != 0 {
		t.Errorf("Fresh DiskCoWSavings = %d, want 0", report.DiskCoWSavings)
	}
}
