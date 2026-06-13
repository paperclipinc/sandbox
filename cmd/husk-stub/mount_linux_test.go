//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestPrepareChrootMountMakesPrivateMountPoint proves the chroot-base becomes a
// MOUNT POINT marked private, which is exactly what the jailer's pivot_root
// requires inside a pod. It needs CAP_SYS_ADMIN to call mount(2); on a host
// without it the test SKIPS (so the unit suite stays green) and the real
// assertion runs in the KVM-CI / bare-metal jailer-in-pod phase.
func TestPrepareChrootMountMakesPrivateMountPoint(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("prepareChrootMount needs CAP_SYS_ADMIN; verified in the KVM-CI jailer-in-pod phase")
	}
	base := filepath.Join(t.TempDir(), "jail")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := prepareChrootMount(base); err != nil {
		t.Fatalf("prepareChrootMount: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(base, unix.MNT_DETACH) })

	// After the helper, base must be a distinct mount point: its st_dev differs
	// from its parent's, or /proc/self/mountinfo lists it. We assert it is a
	// mount point by checking that a bind self-mount is present: statfs succeeds
	// and the path is its own mount (parent dev differs after a bind only when
	// the bind crosses a fs; instead assert mountinfo lists base as a mount).
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if !mountinfoHasPrivate(string(data), base) {
		t.Fatalf("chroot base %q is not a private mount point after prepareChrootMount:\n%s", base, data)
	}

	// Idempotent: a second call (the stub may retry Prepare) must not error.
	if err := prepareChrootMount(base); err != nil {
		t.Fatalf("prepareChrootMount second call: %v", err)
	}
}

// mountinfoHasPrivate reports whether mountinfo lists target as a mount point
// whose optional propagation fields do NOT include a "shared:" tag (i.e. it is
// private or slave-without-shared). pivot_root refuses a new root whose parent
// mount is shared, so the husk chroot base must be private. The mountinfo line
// format is: id parent major:minor root mountpoint options - optional... where
// the optional fields between the 7th field and the standalone "-" carry the
// propagation tags.
func mountinfoHasPrivate(mountinfo, target string) bool {
	for _, line := range splitLines(mountinfo) {
		fields := splitFields(line)
		if len(fields) < 7 {
			continue
		}
		if fields[4] != target {
			continue
		}
		// Optional fields run from index 6 until the standalone "-".
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				return true // reached the separator with no shared: tag
			}
			if len(fields[i]) >= 7 && fields[i][:7] == "shared:" {
				return false
			}
		}
		return true
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func splitFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}
