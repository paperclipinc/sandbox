package fork

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
)

// writeFakeTemplate lays down fake mem/vmstate/rootfs files for a template in a
// temp dataDir, simulating what CreateTemplate produces, without launching
// Firecracker. It returns the dataDir.
func writeFakeTemplate(t *testing.T, id string) string {
	t.Helper()
	dataDir := t.TempDir()
	snapDir := filepath.Join(dataDir, "templates", id, "snapshot")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	mustWrite(t, filepath.Join(snapDir, "mem"), bytes.Repeat([]byte{0xAB}, 9<<20))
	mustWrite(t, filepath.Join(snapDir, "vmstate"), bytes.Repeat([]byte{0xCD}, 1<<20))
	mustWrite(t, filepath.Join(dataDir, "templates", id, "rootfs.ext4"), bytes.Repeat([]byte{0xEF}, 5<<20))
	return dataDir
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newTestStore(t *testing.T, dataDir string) *cas.Store {
	t.Helper()
	s, err := cas.New(filepath.Join(dataDir, "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	return s
}

func TestRecordAndVerifyTemplateRoundTrip(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	store := newTestStore(t, dataDir)

	d, err := recordTemplateDigest(store, dataDir, id, cas.Metadata{})
	if err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	if d == "" {
		t.Fatal("empty digest recorded")
	}
	if !isVerified(dataDir, id) {
		t.Fatal("verified marker not written by recordTemplateDigest")
	}

	// Recorded digest must persist and match what verifyTemplate re-derives.
	got, err := verifyTemplate(dataDir, id, cas.Metadata{})
	if err != nil {
		t.Fatalf("verifyTemplate: %v", err)
	}
	if got != d {
		t.Fatalf("verify digest %s != recorded %s", got, d)
	}
}

func TestVerifyTemplateFailsOnTamperedMem(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	store := newTestStore(t, dataDir)

	if _, err := recordTemplateDigest(store, dataDir, id, cas.Metadata{}); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}

	// Tamper the mem file after recording, and clear the marker so the gate
	// must re-derive.
	memPath := filepath.Join(dataDir, "templates", id, "snapshot", "mem")
	mustWrite(t, memPath, bytes.Repeat([]byte{0x00}, 9<<20))
	if err := os.Remove(filepath.Join(dataDir, "templates", id, verifiedMarker)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	if _, err := verifyTemplate(dataDir, id, cas.Metadata{}); err == nil {
		t.Fatal("expected verifyTemplate to fail on tampered mem")
	}
	if isVerified(dataDir, id) {
		t.Fatal("verified marker must not be written on mismatch")
	}
}

// newGateEngine builds a minimal Engine for exercising the Fork-time
// verify gate without launching Firecracker or requiring KVM. It bypasses
// NewEngine (which validates /dev/kvm) and wires only the fields ensureVerified
// touches.
func newGateEngine(t *testing.T, dataDir string, allowUnverified bool) *Engine {
	t.Helper()
	store := newTestStore(t, dataDir)
	return &Engine{
		dataDir:            dataDir,
		casStore:           store,
		allowUnverified:    allowUnverified,
		unverifiedWarned:   make(map[string]struct{}),
		incompatibleWarned: make(map[string]struct{}),
		templateDigests:    make(map[string]cas.Digest),
	}
}

func TestForkGateRefusesWhenMarkerAbsentAndFlagOff(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	e := newGateEngine(t, dataDir, false)

	// Record a digest, then tamper and drop the marker so the lazy verify
	// inside the gate must fail.
	if _, err := recordTemplateDigest(e.casStore, dataDir, id, cas.Metadata{}); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	mustWrite(t, filepath.Join(dataDir, "templates", id, "snapshot", "mem"), bytes.Repeat([]byte{0x00}, 9<<20))
	if err := os.Remove(filepath.Join(dataDir, "templates", id, verifiedMarker)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	if err := e.ensureVerified(id); err == nil {
		t.Fatal("expected fork gate to refuse tampered unverified snapshot with flag off")
	}
}

func TestForkGateProceedsWhenFlagOn(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	e := newGateEngine(t, dataDir, true)

	if _, err := recordTemplateDigest(e.casStore, dataDir, id, cas.Metadata{}); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	mustWrite(t, filepath.Join(dataDir, "templates", id, "snapshot", "mem"), bytes.Repeat([]byte{0x00}, 9<<20))
	if err := os.Remove(filepath.Join(dataDir, "templates", id, verifiedMarker)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	if err := e.ensureVerified(id); err != nil {
		t.Fatalf("expected fork gate to proceed with --allow-unverified-snapshots, got %v", err)
	}
}

func TestForkGateCheapPathWhenMarkerPresent(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	e := newGateEngine(t, dataDir, false)

	if _, err := recordTemplateDigest(e.casStore, dataDir, id, cas.Metadata{}); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	// Marker present from recording: gate passes without re-hashing even if we
	// tamper the file (steady-state cheap path; documented residual).
	mustWrite(t, filepath.Join(dataDir, "templates", id, "snapshot", "mem"), bytes.Repeat([]byte{0x00}, 9<<20))
	if err := e.ensureVerified(id); err != nil {
		t.Fatalf("expected cheap marker path to pass, got %v", err)
	}
}

func TestIsVerifiedReflectsMarker(t *testing.T) {
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	if isVerified(dataDir, id) {
		t.Fatal("template should not be verified before recording")
	}
	store := newTestStore(t, dataDir)
	if _, err := recordTemplateDigest(store, dataDir, id, cas.Metadata{}); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	if !isVerified(dataDir, id) {
		t.Fatal("template should be verified after recording")
	}
}

// TestTemplateFilePathRejectsTraversal is the direct guard for the path
// traversal hazard on the receiving side of a pull: a crafted manifest file
// name (especially in the volumes/ branch, which accepts volumes/*.ext4 by
// prefix/suffix) must never resolve to a path OUTSIDE the template dir. The
// filepath.Join + prefix-check at the end of templateFilePath is what blocks it;
// this asserts that guard rejects traversal names and accepts only legitimate
// ones that resolve inside the dir.
func TestTemplateFilePathRejectsTraversal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "templates", "py")
	cleanDir := filepath.Clean(dir)

	// Every one of these must be REJECTED with an error and resolve to no path
	// outside the template dir. The volumes/ branch is the load-bearing case: it
	// accepts volumes/*.ext4 by prefix/suffix, so only the Join + prefix-check
	// stops volumes/../../etc/x.ext4 from escaping.
	rejected := []string{
		"volumes/../../etc/shadow.ext4",
		"volumes/../escape.ext4",
		"/etc/x.ext4",
		"../mem",
		"unknown",
		"volumes/no-suffix",
	}
	for _, name := range rejected {
		t.Run("reject/"+name, func(t *testing.T) {
			got, err := templateFilePath(dir, name)
			if err == nil {
				t.Fatalf("templateFilePath(%q) = %q, want error", name, got)
			}
			// Belt and suspenders: even on the error path, no returned path may
			// land outside the template dir.
			if got != "" && got != cleanDir && !strings.HasPrefix(got, cleanDir+string(filepath.Separator)) {
				t.Fatalf("templateFilePath(%q) returned escaping path %q", name, got)
			}
		})
	}

	// Legitimate names must be ACCEPTED and resolve INSIDE the template dir.
	accepted := map[string]string{
		"mem":               filepath.Join(cleanDir, "snapshot", "mem"),
		"vmstate":           filepath.Join(cleanDir, "snapshot", "vmstate"),
		"rootfs":            filepath.Join(cleanDir, "rootfs.ext4"),
		"volumes/data.ext4": filepath.Join(cleanDir, "volumes", "data.ext4"),
	}
	for name, want := range accepted {
		t.Run("accept/"+name, func(t *testing.T) {
			got, err := templateFilePath(dir, name)
			if err != nil {
				t.Fatalf("templateFilePath(%q): unexpected error %v", name, err)
			}
			if got != want {
				t.Fatalf("templateFilePath(%q) = %q, want %q", name, got, want)
			}
			if got != cleanDir && !strings.HasPrefix(got, cleanDir+string(filepath.Separator)) {
				t.Fatalf("templateFilePath(%q) = %q, escapes template dir %q", name, got, cleanDir)
			}
		})
	}
}
