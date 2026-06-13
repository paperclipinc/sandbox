//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// prepareChrootMount makes chrootBase usable as the jailer's pivot_root target
// INSIDE a pod. The Firecracker jailer pivot_roots into
// <chroot-base>/firecracker/<vm-id>/root, and pivot_root(2) requires (a) the new
// root to be a mount point and (b) the new root's parent mount to NOT have
// shared propagation. A pod's container rootfs is commonly mounted with shared
// or otherwise-propagating flags, so a plain directory under it fails pivot_root
// with EINVAL/EBUSY. We fix both preconditions once, in the pod's own mount
// namespace, before any jailer launch:
//
//  1. bind-mount chrootBase onto itself, so it BECOMES a mount point (the parent
//     of every per-VM jail dir is now a mount the jailer can pivot under); and
//  2. recursively mark it MS_PRIVATE, so its (and its children's) propagation
//     does not defeat pivot_root.
//
// It needs CAP_SYS_ADMIN (mount(2)); the husk pod adds exactly that one
// capability back (huskpod.go), nothing else. It is idempotent: a re-bind of an
// already-bound private base is harmless, and re-marking private is a no-op.
// chrootBase carries no secrets; errors name the path only.
func prepareChrootMount(chrootBase string) error {
	// Bind chrootBase onto itself so it is a mount point. MS_BIND with no source
	// distinct from target is the canonical self-bind.
	if err := unix.Mount(chrootBase, chrootBase, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind-mount jailer chroot base %s onto itself (needed so the jailer can pivot_root inside the pod): %w", chrootBase, err)
	}
	// Mark it private (recursively) so pivot_root is not refused by shared
	// propagation on the parent mount.
	if err := unix.Mount("", chrootBase, "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make jailer chroot base %s a private mount (needed so the jailer can pivot_root inside the pod): %w", chrootBase, err)
	}
	return nil
}
