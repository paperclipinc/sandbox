package cas

import (
	"bytes"
	"testing"
)

func TestVerifyFilesAgainstManifestMatches(t *testing.T) {
	dir := t.TempDir()
	mem := writeFile(t, dir, "mem", bytes.Repeat([]byte{0xAA}, 5<<20))
	state := writeFile(t, dir, "vmstate", bytes.Repeat([]byte{0xBB}, 1<<20))

	m, err := BuildManifest(map[string]string{"mem": mem, "vmstate": state}, Metadata{VMMVersion: "v1"})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if err := VerifyFilesAgainstManifest(m, map[string]string{"mem": mem, "vmstate": state}); err != nil {
		t.Fatalf("verify of matching files failed: %v", err)
	}
}

func TestVerifyFilesAgainstManifestSubset(t *testing.T) {
	dir := t.TempDir()
	mem := writeFile(t, dir, "mem", bytes.Repeat([]byte{0xAA}, 5<<20))
	state := writeFile(t, dir, "vmstate", bytes.Repeat([]byte{0xBB}, 1<<20))
	rootfs := writeFile(t, dir, "rootfs", bytes.Repeat([]byte{0xCC}, 2<<20))

	m, err := BuildManifest(map[string]string{"mem": mem, "vmstate": state, "rootfs": rootfs}, Metadata{VMMVersion: "v1"})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	// Verify only the mounted subset (mem+vmstate); rootfs is not mounted in a
	// husk pod but is still part of the manifest digest the caller binds.
	if err := VerifyFilesAgainstManifest(m, map[string]string{"mem": mem, "vmstate": state}); err != nil {
		t.Fatalf("verify of subset failed: %v", err)
	}
}

func TestVerifyFilesAgainstManifestDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	mem := writeFile(t, dir, "mem", bytes.Repeat([]byte{0xAA}, 5<<20))
	state := writeFile(t, dir, "vmstate", bytes.Repeat([]byte{0xBB}, 1<<20))

	m, err := BuildManifest(map[string]string{"mem": mem, "vmstate": state}, Metadata{VMMVersion: "v1"})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	// Tamper the mem file on disk after the manifest was recorded.
	writeFile(t, dir, "mem", bytes.Repeat([]byte{0xAB}, 5<<20))
	err = VerifyFilesAgainstManifest(m, map[string]string{"mem": mem, "vmstate": state})
	if err == nil {
		t.Fatal("expected integrity failure for tampered mem, got nil")
	}
}

func TestVerifyFilesAgainstManifestRejectsUnknownFile(t *testing.T) {
	dir := t.TempDir()
	mem := writeFile(t, dir, "mem", bytes.Repeat([]byte{0xAA}, 1<<20))
	m, err := BuildManifest(map[string]string{"mem": mem}, Metadata{VMMVersion: "v1"})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	other := writeFile(t, dir, "other", bytes.Repeat([]byte{0xAA}, 1<<20))
	if err := VerifyFilesAgainstManifest(m, map[string]string{"unknown": other}); err == nil {
		t.Fatal("expected error for a file not in the manifest, got nil")
	}
}

func TestDecodeManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mem := writeFile(t, dir, "mem", bytes.Repeat([]byte{0xAA}, 1<<20))
	m, err := BuildManifest(map[string]string{"mem": mem}, Metadata{
		VMMVersion:            "v1.15.0",
		CPUModel:              "Intel Xeon",
		KernelVersion:         "6.1.0",
		SnapshotFormatVersion: CurrentSnapshotFormatVersion,
		ConfigHash:            "abc",
	})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	canon := m.Canonical()
	got, err := DecodeManifest(canon)
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	if got.Digest() != m.Digest() {
		t.Fatalf("decoded digest %s != original %s", got.Digest(), m.Digest())
	}
	if got.VMMVersion != "v1.15.0" || got.CPUModel != "Intel Xeon" || got.SnapshotFormatVersion != CurrentSnapshotFormatVersion {
		t.Fatalf("decoded metadata mismatch: %+v", got)
	}
}
