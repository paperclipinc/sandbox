package volume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingRunner captures the argv of each runner invocation and optionally
// fails on a configured predicate, so tests can assert command shape without
// running real mkfs or cp.
type recordingRunner struct {
	calls   [][]string
	failOn  func(argv []string) bool
	failErr error
}

func (r *recordingRunner) run(argv []string) error {
	r.calls = append(r.calls, argv)
	if r.failOn != nil && r.failOn(argv) {
		if r.failErr != nil {
			return r.failErr
		}
		return errors.New("recorded failure")
	}
	return nil
}

func newTestBackend(t *testing.T) (*Backend, *recordingRunner) {
	t.Helper()
	rr := &recordingRunner{}
	b := New(t.TempDir())
	b.runner = rr.run
	return b, rr
}

func TestFreshIssuesMkfsWithSizeAndPath(t *testing.T) {
	b, rr := newTestBackend(t)
	spec := Spec{Name: "data", SizeMB: 512, MountPath: "/data", Policy: ForkPolicyFresh}

	got, err := b.Fresh(spec, "sb-1")
	if err != nil {
		t.Fatalf("Fresh: %v", err)
	}

	wantPath := filepath.Join(b.root, "sandboxes", "sb-1", "volumes", "data.ext4")
	if got.HostPath != wantPath {
		t.Errorf("HostPath = %q, want %q", got.HostPath, wantPath)
	}
	if got.MountPath != "/data" {
		t.Errorf("MountPath = %q, want /data", got.MountPath)
	}
	if got.ReadOnly {
		t.Errorf("Fresh volume should not be read-only")
	}
	if len(rr.calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d: %v", len(rr.calls), rr.calls)
	}
	argv := rr.calls[0]
	if argv[0] != "mkfs.ext4" {
		t.Errorf("argv[0] = %q, want mkfs.ext4", argv[0])
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-F") || !strings.Contains(joined, "-q") {
		t.Errorf("mkfs argv missing -F/-q: %v", argv)
	}
	if argv[len(argv)-1] != "512M" {
		t.Errorf("mkfs size arg = %q, want 512M", argv[len(argv)-1])
	}
	if argv[len(argv)-2] != wantPath {
		t.Errorf("mkfs path arg = %q, want %q", argv[len(argv)-2], wantPath)
	}
	// The backing file must have been created before mkfs runs.
	if _, statErr := os.Stat(wantPath); statErr != nil {
		t.Errorf("backing file not created: %v", statErr)
	}
}

func TestSnapshotIssuesReflinkCopyToDistinctPath(t *testing.T) {
	b, rr := newTestBackend(t)
	src := filepath.Join(t.TempDir(), "source.ext4")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Name: "data", SizeMB: 512, MountPath: "/data", Policy: ForkPolicySnapshot}

	got, err := b.Snapshot(spec, "fork-1", src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got.HostPath == src {
		t.Errorf("Snapshot path %q should differ from source", got.HostPath)
	}
	wantPath := filepath.Join(b.root, "sandboxes", "fork-1", "volumes", "data.ext4")
	if got.HostPath != wantPath {
		t.Errorf("HostPath = %q, want %q", got.HostPath, wantPath)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d: %v", len(rr.calls), rr.calls)
	}
	argv := rr.calls[0]
	if argv[0] != "cp" {
		t.Errorf("argv[0] = %q, want cp", argv[0])
	}
	if !strings.Contains(strings.Join(argv, " "), "--reflink=always") {
		t.Errorf("first cp should try --reflink=always: %v", argv)
	}
	if argv[len(argv)-2] != src || argv[len(argv)-1] != wantPath {
		t.Errorf("cp src/dst = %q %q, want %q %q", argv[len(argv)-2], argv[len(argv)-1], src, wantPath)
	}
}

func TestSnapshotFallsBackToReflinkAuto(t *testing.T) {
	b, rr := newTestBackend(t)
	rr.failOn = func(argv []string) bool {
		return strings.Contains(strings.Join(argv, " "), "--reflink=always")
	}
	src := filepath.Join(t.TempDir(), "source.ext4")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Name: "data", Policy: ForkPolicySnapshot}

	if _, err := b.Snapshot(spec, "fork-2", src); err != nil {
		t.Fatalf("Snapshot fallback: %v", err)
	}
	if len(rr.calls) != 2 {
		t.Fatalf("expected 2 runner calls (always then auto), got %d: %v", len(rr.calls), rr.calls)
	}
	if !strings.Contains(strings.Join(rr.calls[1], " "), "--reflink=auto") {
		t.Errorf("fallback should use --reflink=auto: %v", rr.calls[1])
	}
}

func TestReflinkCopyTriesAlwaysThenAuto(t *testing.T) {
	b, rr := newTestBackend(t)
	rr.failOn = func(argv []string) bool {
		return strings.Contains(strings.Join(argv, " "), "--reflink=always")
	}
	src := filepath.Join(t.TempDir(), "src.ext4")
	dst := filepath.Join(t.TempDir(), "dst.ext4")

	if err := b.ReflinkCopy(src, dst); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}
	if len(rr.calls) != 2 {
		t.Fatalf("expected 2 cp calls (always then auto), got %d: %v", len(rr.calls), rr.calls)
	}
	if !strings.Contains(strings.Join(rr.calls[0], " "), "--reflink=always") {
		t.Errorf("first cp should try --reflink=always: %v", rr.calls[0])
	}
	if !strings.Contains(strings.Join(rr.calls[1], " "), "--reflink=auto") {
		t.Errorf("fallback should use --reflink=auto: %v", rr.calls[1])
	}
	if rr.calls[1][len(rr.calls[1])-2] != src || rr.calls[1][len(rr.calls[1])-1] != dst {
		t.Errorf("cp src/dst = %q %q, want %q %q", rr.calls[1][len(rr.calls[1])-2], rr.calls[1][len(rr.calls[1])-1], src, dst)
	}
}

func TestReflinkCopyMakesDestDir(t *testing.T) {
	b, _ := newTestBackend(t)
	src := filepath.Join(t.TempDir(), "src.ext4")
	dst := filepath.Join(t.TempDir(), "nested", "dst.ext4")
	if err := b.ReflinkCopy(src, dst); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dst)); err != nil {
		t.Errorf("dest parent dir not created: %v", err)
	}
}

func TestShareReturnsSourceReadOnlyNoCopy(t *testing.T) {
	b, rr := newTestBackend(t)
	src := "/srv/images/base.ext4"
	spec := Spec{Name: "shared", MountPath: "/shared", Policy: ForkPolicyShare}

	got, err := b.Share(spec, "fork-3", src)
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if got.HostPath != src {
		t.Errorf("Share HostPath = %q, want source %q", got.HostPath, src)
	}
	if !got.ReadOnly {
		t.Errorf("Share volume must be read-only")
	}
	if len(rr.calls) != 0 {
		t.Errorf("Share must not invoke the runner, got %v", rr.calls)
	}
}

func TestCloneIssuesFullCopy(t *testing.T) {
	b, rr := newTestBackend(t)
	src := filepath.Join(t.TempDir(), "source.ext4")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Name: "data", Policy: ForkPolicyClone}

	got, err := b.Clone(spec, "fork-4", src)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	wantPath := filepath.Join(b.root, "sandboxes", "fork-4", "volumes", "data.ext4")
	if got.HostPath != wantPath {
		t.Errorf("HostPath = %q, want %q", got.HostPath, wantPath)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(rr.calls))
	}
	argv := rr.calls[0]
	if argv[0] != "cp" {
		t.Errorf("argv[0] = %q, want cp", argv[0])
	}
	if strings.Contains(strings.Join(argv, " "), "--reflink") {
		t.Errorf("Clone should be a full copy, not reflink: %v", argv)
	}
	if argv[len(argv)-2] != src || argv[len(argv)-1] != wantPath {
		t.Errorf("cp src/dst = %q %q, want %q %q", argv[len(argv)-2], argv[len(argv)-1], src, wantPath)
	}
}

func TestCleanupRemovesVolumesDir(t *testing.T) {
	b, _ := newTestBackend(t)
	dir := filepath.Join(b.root, "sandboxes", "sb-9", "volumes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.Cleanup("sb-9"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("volumes dir should be removed, stat err = %v", err)
	}
	// Cleanup of a missing dir must be a no-op (idempotent).
	if err := b.Cleanup("never-existed"); err != nil {
		t.Errorf("Cleanup of missing dir should be nil, got %v", err)
	}
}

// TestBackendRejectsTraversingNames is the defense-in-depth half of the volume
// name traversal guard: even if a bad name reaches the backend (the gRPC layer
// should already have rejected it), no path is built and nothing is written
// outside the backend root.
func TestBackendRejectsTraversingNames(t *testing.T) {
	bad := []string{"", "..", "../x", "a/b", "a.b", "/abs", strings.Repeat("a", 65)}
	for _, name := range bad {
		b, rr := newTestBackend(t)
		spec := Spec{Name: name, SizeMB: 64, MountPath: "/x", Policy: ForkPolicyFresh}

		if _, err := b.Fresh(spec, "sb-1"); err == nil {
			t.Errorf("Fresh(%q) = nil error, want rejection", name)
		}
		if _, err := b.Snapshot(spec, "sb-1", filepath.Join(t.TempDir(), "src.ext4")); err == nil {
			t.Errorf("Snapshot(%q) = nil error, want rejection", name)
		}
		if _, err := b.Clone(spec, "sb-1", filepath.Join(t.TempDir(), "src.ext4")); err == nil {
			t.Errorf("Clone(%q) = nil error, want rejection", name)
		}
		if _, err := b.FreshTemplate(spec, "tmpl-1"); err == nil {
			t.Errorf("FreshTemplate(%q) = nil error, want rejection", name)
		}
		// No runner command (mkfs/cp) must have been issued for a rejected name,
		// so nothing was written anywhere.
		if len(rr.calls) != 0 {
			t.Errorf("name %q produced runner calls %v, want none", name, rr.calls)
		}
	}
}

func TestParseSizeMB(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"5Gi", 5120, false},
		{"512Mi", 512, false},
		{"", DefaultSizeMB, false},
		{"1Gi", 1024, false},
		{"not-a-size", 0, true},
	}
	for _, c := range cases {
		got, err := ParseSizeMB(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseSizeMB(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSizeMB(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSizeMB(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
