package ociroot

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// tarEntry describes a single member of a synthetic layer tar.
type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	linkname string
}

// imageFromEntries builds an in-memory single-layer image whose layer tar
// contains exactly the given entries. This lets tests assert specific file
// contents without any registry network.
func imageFromEntries(t *testing.T, entries []tarEntry) v1.Image {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	if err != nil {
		t.Fatalf("layer from opener: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append layers: %v", err)
	}
	return img
}

func TestExtractImage(t *testing.T) {
	img := imageFromEntries(t, []tarEntry{
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/hello.txt", typeflag: tar.TypeReg, mode: 0o644, body: "hello world"},
		{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/run", typeflag: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\n"},
		{name: "etc/link.txt", typeflag: tar.TypeSymlink, linkname: "hello.txt"},
	})

	dest := t.TempDir()
	if err := ExtractImage(img, dest); err != nil {
		t.Fatalf("ExtractImage: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "etc", "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("hello.txt content = %q, want %q", got, "hello world")
	}

	info, err := os.Stat(filepath.Join(dest, "bin", "run"))
	if err != nil {
		t.Fatalf("stat bin/run: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("bin/run mode = %o, want 0755", info.Mode().Perm())
	}

	linkTarget, err := os.Readlink(filepath.Join(dest, "etc", "link.txt"))
	if err != nil {
		t.Fatalf("readlink etc/link.txt: %v", err)
	}
	if linkTarget != "hello.txt" {
		t.Errorf("link target = %q, want %q", linkTarget, "hello.txt")
	}
	resolved, err := os.ReadFile(filepath.Join(dest, "etc", "link.txt"))
	if err != nil {
		t.Fatalf("read through symlink: %v", err)
	}
	if string(resolved) != "hello world" {
		t.Errorf("symlink resolves to %q, want %q", resolved, "hello world")
	}
}

func TestExtractImageRejectsDotDotTraversal(t *testing.T) {
	img := imageFromEntries(t, []tarEntry{
		{name: "../escape", typeflag: tar.TypeReg, mode: 0o644, body: "pwned"},
	})

	dest := t.TempDir()
	if err := ExtractImage(img, dest); err == nil {
		t.Fatal("ExtractImage accepted a ../ traversal entry, want error")
	}

	// The escape target must not exist next to destDir.
	escape := filepath.Join(filepath.Dir(dest), "escape")
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("traversal wrote outside destDir at %s (err=%v)", escape, err)
	}
}

func TestExtractImageRejectsAbsolutePath(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "abs-marker")
	img := imageFromEntries(t, []tarEntry{
		{name: marker, typeflag: tar.TypeReg, mode: 0o644, body: "pwned"},
	})

	dest := t.TempDir()
	if err := ExtractImage(img, dest); err == nil {
		t.Fatal("ExtractImage accepted an absolute path entry, want error")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("absolute path wrote outside destDir at %s (err=%v)", marker, err)
	}
}

func TestExtractImageRejectsEscapingSymlink(t *testing.T) {
	// mutate.Extract drops "../" symlink targets itself, but absolute targets
	// survive flattening, so an absolute symlink is the case our guard must
	// catch end to end.
	img := imageFromEntries(t, []tarEntry{
		{name: "evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})

	dest := t.TempDir()
	if err := ExtractImage(img, dest); err == nil {
		t.Fatal("ExtractImage accepted a symlink escaping destDir, want error")
	}
}

func TestExtractImageBlocksWriteThroughSymlinkedParent(t *testing.T) {
	// Classic parent-symlink-traversal: an earlier entry creates a directory
	// symlink that points outside destDir, then a later entry writes a file
	// "through" that symlink (a/x). A lexical join would happily write to
	// a/x and follow the symlink at extraction time, landing outside destDir.
	// SecureJoin resolves the symlinked parent on disk and must keep the write
	// inside destDir (or reject it), never touching the escape target.
	outside := t.TempDir()
	escapeDir := filepath.Join(outside, "escape")

	img := imageFromEntries(t, []tarEntry{
		{name: "a", typeflag: tar.TypeSymlink, linkname: escapeDir},
		{name: "a/x", typeflag: tar.TypeReg, mode: 0o644, body: "pwned"},
	})

	dest := t.TempDir()
	// The symlink entry itself must be rejected (absolute target), so the
	// write-through never gets a symlinked parent to follow.
	if err := ExtractImage(img, dest); err == nil {
		t.Fatal("ExtractImage accepted a symlinked-parent write-through, want error")
	}

	// Nothing may be written through the escaping symlink.
	if _, err := os.Stat(filepath.Join(escapeDir, "x")); !os.IsNotExist(err) {
		t.Fatalf("write-through symlink landed outside destDir at %s (err=%v)", filepath.Join(escapeDir, "x"), err)
	}
}

func TestExtractImageBlocksWriteThroughRelativeSymlinkedParent(t *testing.T) {
	// Same class, but the symlinked parent uses a relative target that escapes
	// destDir via "..". mutate.Extract strips "../" symlink targets, so this
	// test drives extractEntry directly to exercise the on-disk guard without
	// the flattening layer scrubbing the target first.
	dest := t.TempDir()
	sibling := filepath.Join(filepath.Dir(dest), "ociroot-escape-sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	defer func() { _ = os.RemoveAll(sibling) }()

	// "a" -> "../ociroot-escape-sibling" (escapes dest).
	rel := filepath.Join("..", filepath.Base(sibling))
	linkHdr := &tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: rel}
	if err := extractEntry(dest, nil, linkHdr); err == nil {
		t.Fatal("extractEntry accepted an escaping relative symlink, want error")
	}

	// Even if a later writer targets a/x, SecureJoin must keep it inside dest.
	body := "pwned"
	fileHdr := &tar.Header{Name: "a/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}
	_ = extractEntry(dest, bytes.NewReader([]byte(body)), fileHdr)
	if _, err := os.Stat(filepath.Join(sibling, "x")); !os.IsNotExist(err) {
		t.Fatalf("write-through relative symlink landed outside destDir at %s (err=%v)", filepath.Join(sibling, "x"), err)
	}
}

func TestExtractEntryResolvesWriteThroughInTreeSymlinkedParent(t *testing.T) {
	// An in-tree directory symlink (real -> realdir) exists, then a later entry
	// writes through it (real/x). secureJoin must resolve the symlinked parent
	// on disk so the write lands at realdir/x, inside destDir, and never
	// escapes. This drives extractEntry directly to exercise the SecureJoin
	// on-disk walk on the accept path, independent of how mutate.Extract may
	// reorder or scrub write-through entries during flattening.
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "realdir"), 0o755); err != nil {
		t.Fatalf("mkdir realdir: %v", err)
	}
	if err := os.Symlink("realdir", filepath.Join(dest, "real")); err != nil {
		t.Fatalf("symlink real: %v", err)
	}

	body := "inside"
	hdr := &tar.Header{Name: "real/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}
	if err := extractEntry(dest, bytes.NewReader([]byte(body)), hdr); err != nil {
		t.Fatalf("extractEntry: %v", err)
	}

	// The write must have resolved through the symlink into realdir.
	got, err := os.ReadFile(filepath.Join(dest, "realdir", "x"))
	if err != nil {
		t.Fatalf("read realdir/x: %v", err)
	}
	if string(got) != body {
		t.Errorf("realdir/x = %q, want %q", got, body)
	}
}

func TestSymlinkTargetStaysInside(t *testing.T) {
	// Use a real temp dir because symlinkTargetStaysInside resolves on disk.
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "a", "b"), 0o755); err != nil {
		t.Fatalf("mkdir a/b: %v", err)
	}

	if !symlinkTargetStaysInside(dest, filepath.Join(dest, "bin/sh"), "busybox") {
		t.Error("relative in-tree symlink target rejected")
	}
	if !symlinkTargetStaysInside(dest, filepath.Join(dest, "a/b/link"), "../c/file") {
		t.Error("relative target that stays inside rejected")
	}
	if symlinkTargetStaysInside(dest, filepath.Join(dest, "evil"), "/etc/passwd") {
		t.Error("absolute symlink target accepted")
	}
	if symlinkTargetStaysInside(dest, filepath.Join(dest, "evil"), "../../etc/passwd") {
		t.Error("relative target escaping dest accepted")
	}
}

func TestSecureJoin(t *testing.T) {
	dest := "/var/lib/root"

	ok, err := secureJoin(dest, "etc/hosts")
	if err != nil {
		t.Fatalf("secureJoin clean path: %v", err)
	}
	if ok != filepath.Join(dest, "etc/hosts") {
		t.Errorf("secureJoin = %q, want %q", ok, filepath.Join(dest, "etc/hosts"))
	}

	if _, err := secureJoin(dest, "../escape"); err == nil {
		t.Error("secureJoin allowed ../ escape")
	}
	if _, err := secureJoin(dest, "/etc/passwd"); err == nil {
		t.Error("secureJoin allowed absolute path escape")
	}
}

func TestInjectAgent(t *testing.T) {
	dest := t.TempDir()

	agentSrc := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(agentSrc, []byte("\x7fELF fake agent"), 0o644); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	busyboxSrc := filepath.Join(t.TempDir(), "busybox")
	if err := os.WriteFile(busyboxSrc, []byte("\x7fELF fake busybox"), 0o755); err != nil {
		t.Fatalf("write fake busybox: %v", err)
	}

	if err := InjectAgent(dest, agentSrc, busyboxSrc); err != nil {
		t.Fatalf("InjectAgent: %v", err)
	}

	initInfo, err := os.Stat(filepath.Join(dest, "init"))
	if err != nil {
		t.Fatalf("stat init: %v", err)
	}
	if initInfo.Mode().Perm() != 0o755 {
		t.Errorf("init mode = %o, want 0755", initInfo.Mode().Perm())
	}
	initBody, err := os.ReadFile(filepath.Join(dest, "init"))
	if err != nil {
		t.Fatalf("read init: %v", err)
	}
	if !bytes.Contains(initBody, []byte("fake agent")) {
		t.Errorf("init body = %q, missing agent content", initBody)
	}

	for _, d := range []string{"proc", "sys", "dev", "tmp", "run", "workspace"} {
		info, err := os.Stat(filepath.Join(dest, d))
		if err != nil {
			t.Errorf("mount dir %s missing: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("mount point %s is not a dir", d)
		}
	}

	// /bin/sh must resolve to the busybox we injected.
	shInfo, err := os.Stat(filepath.Join(dest, "bin", "sh"))
	if err != nil {
		t.Fatalf("stat bin/sh: %v", err)
	}
	if shInfo.IsDir() {
		t.Error("bin/sh is a dir, want file or symlink to busybox")
	}
}

func TestInjectAgentNoShellNoBusyboxErrors(t *testing.T) {
	dest := t.TempDir()
	agentSrc := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(agentSrc, []byte("agent"), 0o644); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}

	if err := InjectAgent(dest, agentSrc, ""); err == nil {
		t.Fatal("InjectAgent with no shell and no busybox should error")
	}
}

func TestInjectAgentExistingShellKept(t *testing.T) {
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "bin", "sh"), []byte("real sh"), 0o755); err != nil {
		t.Fatalf("write existing sh: %v", err)
	}
	agentSrc := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(agentSrc, []byte("agent"), 0o644); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}

	// No busybox needed because /bin/sh already exists.
	if err := InjectAgent(dest, agentSrc, ""); err != nil {
		t.Fatalf("InjectAgent with existing shell: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "bin", "sh"))
	if err != nil {
		t.Fatalf("read bin/sh: %v", err)
	}
	if string(body) != "real sh" {
		t.Errorf("existing /bin/sh overwritten, got %q", body)
	}
}
