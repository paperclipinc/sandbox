package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/paperclipinc/mitos/internal/firecracker"
)

// parseUIDRange parses the --uid-range flag, "low-high" inclusive.
// uid 0 is refused: jailed VMs must never run as root.
func parseUIDRange(s string) (uint32, uint32, error) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("--uid-range %q: expected the form low-high, for example 64000-64999", s)
	}
	low, err := strconv.ParseUint(lo, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("--uid-range %q: low bound: %w", s, err)
	}
	high, err := strconv.ParseUint(hi, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("--uid-range %q: high bound: %w", s, err)
	}
	if low == 0 {
		return 0, 0, fmt.Errorf("--uid-range %q: uid 0 is root; jailed VMs must run as an unprivileged uid", s)
	}
	if low > high {
		return 0, 0, fmt.Errorf("--uid-range %q: low bound above high bound", s)
	}
	return uint32(low), uint32(high), nil
}

// buildJailerConfig validates the jailer flags and produces the engine
// JailerConfig. It fails closed on every misconfiguration:
//
//   - a malformed or root-including --uid-range;
//   - --chroot-base on a different filesystem from --data-dir (snapshot,
//     kernel, and rootfs files are hard-linked into each chroot; across
//     filesystems every fork would degrade to a full copy);
//   - forkd not running as root. The jailer needs root to set up the
//     jail; concretely it exercises CAP_SYS_ADMIN (cgroup and namespace
//     setup), CAP_CHOWN (handing the chroot to the per-VM uid),
//     CAP_SETUID and CAP_SETGID (dropping to that uid/gid), and
//     CAP_MKNOD (/dev/kvm and /dev/net/tun nodes inside the chroot);
//     forkd additionally needs to open /dev/kvm. The DaemonSet grants
//     exactly that set (deploy/daemon/daemonset.yaml).
//
// An empty jailerBin disables the jailer (development only; the caller
// logs a loud warning and the threat model flags the residual risk).
// sameFS compares the filesystems of two paths; it is injected so the
// check is unit-testable (see sameDevice for the platform versions).
func buildJailerConfig(jailerBin, chrootBase, uidRange, dataDir string, euid int, sameFS func(a, b string) (bool, error)) (firecracker.JailerConfig, error) {
	if jailerBin == "" {
		return firecracker.JailerConfig{}, nil
	}

	low, high, err := parseUIDRange(uidRange)
	if err != nil {
		return firecracker.JailerConfig{}, err
	}

	if euid != 0 {
		return firecracker.JailerConfig{}, fmt.Errorf("--jailer requires forkd to run as root (euid 0, currently %d): the jailer needs CAP_SYS_ADMIN, CAP_CHOWN, CAP_SETUID, CAP_SETGID, and CAP_MKNOD to build each VM's jail; run unjailed only for development by omitting --jailer", euid)
	}

	for _, dir := range []string{chrootBase, dataDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return firecracker.JailerConfig{}, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	same, err := sameFS(chrootBase, dataDir)
	if err != nil {
		return firecracker.JailerConfig{}, fmt.Errorf("compare filesystems of --chroot-base %s and --data-dir %s: %w", chrootBase, dataDir, err)
	}
	if !same {
		return firecracker.JailerConfig{}, fmt.Errorf("--chroot-base %s and --data-dir %s are on different filesystems; snapshot and rootfs files are hard-linked into each VM chroot, which requires one filesystem. Move --chroot-base under the data dir (for example %s/jailer)", chrootBase, dataDir, dataDir)
	}

	return firecracker.JailerConfig{
		JailerBin:     jailerBin,
		ChrootBaseDir: chrootBase,
		UIDRange:      [2]uint32{low, high},
	}, nil
}
