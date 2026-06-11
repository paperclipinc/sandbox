//go:build linux

package main

import (
	"testing"

	"github.com/paperclipinc/sandbox/internal/vsock"
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
