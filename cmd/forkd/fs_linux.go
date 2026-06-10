//go:build linux

package main

import (
	"fmt"
	"syscall"
)

// sameDevice reports whether two paths live on the same filesystem by
// comparing the device ids of their inodes. Hard links only work within
// one filesystem, so the jailer setup depends on this being true for
// --chroot-base and --data-dir.
func sameDevice(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("stat %s: %w", a, err)
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("stat %s: %w", b, err)
	}
	return sa.Dev == sb.Dev, nil
}
