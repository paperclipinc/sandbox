//go:build linux

package main

import (
	"os"
	"testing"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// TestMountVolumesEmpty proves an empty mount table mounts nothing.
func TestMountVolumesEmpty(t *testing.T) {
	if got := mountVolumes(nil); got != 0 {
		t.Errorf("mountVolumes(nil) = %d, want 0", got)
	}
}

// TestMountVolumesSkipsEmptyEntries proves an entry with no device or no mount
// path is skipped (not mounted) rather than attempted, so a malformed table
// cannot mount a device at an empty path.
func TestMountVolumesSkipsEmptyEntries(t *testing.T) {
	entries := []vsock.VolumeMountEntry{
		{Device: "", MountPath: "/x"},
		{Device: "/dev/vdb", MountPath: ""},
	}
	if got := mountVolumes(entries); got != 0 {
		t.Errorf("mountVolumes with only empty entries = %d, want 0", got)
	}
}

// TestIsMountedDetectsRoot proves isMounted parses /proc/mounts: the root
// filesystem "/" is always a mount point, and a path that is not a mount point
// is reported false.
func TestIsMountedDetectsRoot(t *testing.T) {
	if !isMounted("/") {
		t.Error("isMounted(\"/\") = false, want true (root is always mounted)")
	}
	if isMounted("/definitely/not/a/mount/point/zzz") {
		t.Error("isMounted of a non-mount path = true, want false")
	}
}

// TestWriteResolvConf proves the guest writes a single nameserver line for the
// delivered resolver IP, and that the write is idempotent (re-delivery yields
// the same content, not appended lines).
func TestWriteResolvConf(t *testing.T) {
	path := t.TempDir() + "/resolv.conf"

	if err := writeResolvConf(path, "169.254.1.1"); err != nil {
		t.Fatalf("writeResolvConf: %v", err)
	}
	want := "nameserver 169.254.1.1\n"
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if string(got) != want {
		t.Errorf("resolv.conf = %q, want %q", got, want)
	}

	// Idempotent: writing again does not append, the content is identical.
	if err := writeResolvConf(path, "169.254.1.1"); err != nil {
		t.Fatalf("writeResolvConf (second): %v", err)
	}
	got2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf (second): %v", err)
	}
	if string(got2) != want {
		t.Errorf("resolv.conf after re-write = %q, want %q", got2, want)
	}
}

// TestWriteResolvConfEmptyIsNoop proves that with no resolver IP the guest does
// NOT create or clobber resolv.conf, preserving the feature-off behavior.
func TestWriteResolvConfEmptyIsNoop(t *testing.T) {
	path := t.TempDir() + "/resolv.conf"
	if err := os.WriteFile(path, []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
		t.Fatalf("seed resolv.conf: %v", err)
	}
	if err := writeResolvConf(path, ""); err != nil {
		t.Fatalf("writeResolvConf(empty): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if string(got) != "nameserver 8.8.8.8\n" {
		t.Errorf("resolv.conf was clobbered: %q", got)
	}
}
