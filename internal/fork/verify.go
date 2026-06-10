package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/paperclipinc/sandbox/internal/cas"
)

// Verify-on-load design (issue #9):
//
// A template snapshot is content-addressed in the CAS store the moment it is
// built (recordTemplateDigest), and its manifest digest is written to
// <dataDir>/templates/<id>/manifest.digest. Integrity is then enforced by
// re-deriving that digest from the on-disk mem+vmstate+rootfs and comparing.
//
// Re-hashing a multi-GB memory image on every Fork is unacceptable on the hot
// path, so verification happens ONCE: at template registration (build time, or
// first use after a forkd restart that discovered the template on disk). A
// cheap "verified" marker file records that a template passed verification;
// Fork only stats that marker. This is verify-once-at-registration, NOT
// per-fork. The honest residual (tracked in the threat model) is that a
// snapshot tampered with AFTER it was verified is not re-checked until the
// marker is cleared.

const (
	manifestDigestFile = "manifest.digest"
	verifiedMarker     = "verified"
)

// templateDir returns the on-disk directory holding a template's files.
func templateDir(dataDir, id string) string {
	return filepath.Join(dataDir, "templates", id)
}

// templateSnapshotFiles maps the CAS logical names to the on-disk paths of a
// template's snapshot files. The rootfs is included only when present, since
// some templates (live-fork checkpoints) carry no separate rootfs copy.
// Whether a rootfs is present is part of the template's content identity: its
// presence adds a file entry to the manifest and therefore changes the digest.
func templateSnapshotFiles(dataDir, id string) map[string]string {
	dir := templateDir(dataDir, id)
	files := map[string]string{
		"mem":     filepath.Join(dir, "snapshot", "mem"),
		"vmstate": filepath.Join(dir, "snapshot", "vmstate"),
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if _, err := os.Stat(rootfs); err == nil {
		files["rootfs"] = rootfs
	}
	return files
}

// recordTemplateDigest content-addresses a freshly built template's snapshot
// files into the CAS store, pins the manifest, records the manifest digest to
// disk, and writes the verified marker. The template was just built by this
// process, so it is trusted at creation: the marker is written without a
// re-hash. It returns the manifest digest, which is safe to log.
//
// The manifest is built with neutral metadata (the supplied vmmVersion but no
// build timestamp) so the digest is a pure content address: re-deriving it
// from the same on-disk bytes yields the same digest. The CAS createdUnix is
// fixed at 0 here for that reason; build time is not part of the snapshot
// identity.
func recordTemplateDigest(store *cas.Store, dataDir, id, vmmVersion string) (cas.Digest, error) {
	files := templateSnapshotFiles(dataDir, id)
	m, err := store.PutSnapshot(files, vmmVersion, 0)
	if err != nil {
		return "", fmt.Errorf("content-address template %s: %w", id, err)
	}
	d := m.Digest()
	if err := store.Pin(d); err != nil {
		return "", fmt.Errorf("pin template %s manifest: %w", id, err)
	}
	if err := writeDigestFile(dataDir, id, d); err != nil {
		return "", err
	}
	if err := writeVerifiedMarker(dataDir, id); err != nil {
		return "", err
	}
	return d, nil
}

// verifyTemplate re-derives the manifest digest of a template's on-disk
// snapshot files and compares it to the recorded digest. On match it writes
// the verified marker and returns the digest; on mismatch it returns an error
// and does NOT write the marker. This is the verify-on-load gate used for
// templates this process did not build (e.g. discovered on disk after a
// restart).
func verifyTemplate(dataDir, id, vmmVersion string) (cas.Digest, error) {
	want, err := readDigestFile(dataDir, id)
	if err != nil {
		return "", fmt.Errorf("read recorded digest for template %s: %w", id, err)
	}
	files := templateSnapshotFiles(dataDir, id)
	// Same neutral metadata as recordTemplateDigest so the digest reflects
	// the on-disk bytes alone and is reproducible across processes.
	m, err := cas.BuildManifest(files, vmmVersion, 0)
	if err != nil {
		return "", fmt.Errorf("re-derive manifest for template %s: %w", id, err)
	}
	got := m.Digest()
	if got != want {
		return "", fmt.Errorf("template %s failed integrity verification: recorded digest %s does not match on-disk content %s", id, want, got)
	}
	if err := writeVerifiedMarker(dataDir, id); err != nil {
		return "", err
	}
	return want, nil
}

// isVerified reports whether the cheap verified marker exists for a template.
// This is the steady-state Fork check: a single stat, no hashing.
func isVerified(dataDir, id string) bool {
	_, err := os.Stat(filepath.Join(templateDir(dataDir, id), verifiedMarker))
	return err == nil
}

func writeDigestFile(dataDir, id string, d cas.Digest) error {
	path := filepath.Join(templateDir(dataDir, id), manifestDigestFile)
	if err := os.WriteFile(path, []byte(d), 0o644); err != nil {
		return fmt.Errorf("write digest file for template %s: %w", id, err)
	}
	return nil
}

func readDigestFile(dataDir, id string) (cas.Digest, error) {
	path := filepath.Join(templateDir(dataDir, id), manifestDigestFile)
	data, err := os.ReadFile(path) //nolint:gosec // path derived from validated template id
	if err != nil {
		return "", err
	}
	// Trim surrounding whitespace to harden the write/read round-trip against a
	// stray trailing newline a tool or editor might have added.
	return cas.Digest(strings.TrimSpace(string(data))), nil
}

func writeVerifiedMarker(dataDir, id string) error {
	path := filepath.Join(templateDir(dataDir, id), verifiedMarker)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return fmt.Errorf("write verified marker for template %s: %w", id, err)
	}
	return nil
}
