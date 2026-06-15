package fork

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realCopyReflinker is a reflinker seam that performs an honest byte copy from
// src to dst (creating the parent dir). It stands in for the production FICLONE
// clone so the per-fork rootfs CoW helper can be unit-tested with no subprocess
// and no KVM: a divergent write to one clone must not be visible in the other,
// which a byte copy models exactly (a real reflink clone diverges per-block on
// first write with the same observable semantics).
func realCopyReflinker(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // test-only paths under t.TempDir()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst) //nolint:gosec // test-only paths under t.TempDir()
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// newRootfsEngine builds an Engine with the per-fork rootfs CoW reflinker wired
// to an honest byte copy, rooted at a temp dir, WITHOUT touching /dev/kvm or
// Firecracker, so prepareForkRootfs is unit testable.
func newRootfsEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{
		dataDir:       t.TempDir(),
		rootfsReflink: realCopyReflinker,
	}
}

// writeTemplateRootfs writes a fake template rootfs.ext4 with known bytes under
// the engine data dir and returns its host path, mirroring the layout Fork uses
// (<dataDir>/templates/<id>/rootfs.ext4).
func writeTemplateRootfs(t *testing.T, e *Engine, templateID string, content []byte) string {
	t.Helper()
	dir := filepath.Join(e.dataDir, "templates", templateID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir template dir: %v", err)
	}
	p := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("write template rootfs: %v", err)
	}
	return p
}

func TestPrepareForkRootfsNoRootfsPathIsNoOp(t *testing.T) {
	e := newRootfsEngine(t)
	rebind, clone, err := e.prepareForkRootfs("tmpl", "sb1", "")
	if err != nil {
		t.Fatalf("prepareForkRootfs: %v", err)
	}
	if rebind != nil || clone != "" {
		t.Errorf("expected no rebind for empty rootfs path, got rebind=%v clone=%q", rebind, clone)
	}
}

func TestPrepareForkRootfsRebindsRootfsDrive(t *testing.T) {
	e := newRootfsEngine(t)
	tmpl := writeTemplateRootfs(t, e, "tmpl", []byte("template-bytes"))

	rebind, clone, err := e.prepareForkRootfs("tmpl", "sb1", tmpl)
	if err != nil {
		t.Fatalf("prepareForkRootfs: %v", err)
	}
	if rebind == nil {
		t.Fatal("expected a rootfs drive rebind, got nil")
	}
	// The drive id MUST be "rootfs": the template build attaches the root device
	// under that id, and the rebind must target the same drive.
	if rebind.DriveID != "rootfs" {
		t.Errorf("rebind drive id = %q, want \"rootfs\"", rebind.DriveID)
	}
	if rebind.PathOnHost != clone {
		t.Errorf("rebind path %q does not match clone path %q", rebind.PathOnHost, clone)
	}
	// The clone must NOT be the shared template rootfs: a rebind that points back
	// at the template is exactly the shared-writable-inode bug.
	if clone == tmpl {
		t.Fatal("clone path equals the shared template rootfs: forks would share one writable inode")
	}
	if !strings.Contains(clone, "sb1") {
		t.Errorf("clone path %q is not sandbox-scoped to sb1", clone)
	}
}

func TestTwoForksGetDistinctRootfsBackingPaths(t *testing.T) {
	e := newRootfsEngine(t)
	tmpl := writeTemplateRootfs(t, e, "tmpl", []byte("pristine-template"))

	_, cloneA, err := e.prepareForkRootfs("tmpl", "fork-a", tmpl)
	if err != nil {
		t.Fatalf("prepare fork-a: %v", err)
	}
	_, cloneB, err := e.prepareForkRootfs("tmpl", "fork-b", tmpl)
	if err != nil {
		t.Fatalf("prepare fork-b: %v", err)
	}

	if cloneA == cloneB {
		t.Fatalf("two forks share one rootfs backing path %q (cross-fork write bleed)", cloneA)
	}
	if cloneA == tmpl || cloneB == tmpl {
		t.Fatalf("a fork's rootfs backing equals the shared template (cloneA=%q cloneB=%q tmpl=%q)", cloneA, cloneB, tmpl)
	}

	// Distinct inodes: a byte copy (and a real reflink clone after first write)
	// must not share an inode, so a write to one is invisible to the other.
	if sameFileInode(t, cloneA, cloneB) {
		t.Fatal("two forks' rootfs clones share one inode")
	}
	if sameFileInode(t, cloneA, tmpl) {
		t.Fatal("a fork's rootfs clone shares the template inode")
	}
}

func TestWriteInOneForkRootfsNotVisibleInSibling(t *testing.T) {
	e := newRootfsEngine(t)
	tmpl := writeTemplateRootfs(t, e, "tmpl", []byte("AAAAAAAAAAAA"))

	_, cloneA, err := e.prepareForkRootfs("tmpl", "fork-a", tmpl)
	if err != nil {
		t.Fatalf("prepare fork-a: %v", err)
	}
	_, cloneB, err := e.prepareForkRootfs("tmpl", "fork-b", tmpl)
	if err != nil {
		t.Fatalf("prepare fork-b: %v", err)
	}

	// Guest A writes to its own rootfs backing.
	if err := os.WriteFile(cloneA, []byte("SECRET-FROM-A"), 0o644); err != nil {
		t.Fatalf("write to fork-a rootfs: %v", err)
	}

	// Guest B must still read the pristine template content, NOT A's write.
	gotB, err := os.ReadFile(cloneB) //nolint:gosec // test path under t.TempDir()
	if err != nil {
		t.Fatalf("read fork-b rootfs: %v", err)
	}
	if string(gotB) == "SECRET-FROM-A" {
		t.Fatal("guest A's rootfs write bled into guest B: cross-fork rootfs write channel")
	}

	// The template source must also be untouched by A's write.
	gotTmpl, err := os.ReadFile(tmpl) //nolint:gosec // test path under t.TempDir()
	if err != nil {
		t.Fatalf("read template rootfs: %v", err)
	}
	if string(gotTmpl) == "SECRET-FROM-A" {
		t.Fatal("guest A's rootfs write bled into the shared template source")
	}
}

func sameFileInode(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(ai, bi)
}
