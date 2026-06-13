//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unsafe"

	"github.com/paperclipinc/mitos/internal/vsock"
	"golang.org/x/sys/unix"
)

// clockStepThresholdNanos is the drift past which the guest steps
// CLOCK_REALTIME. A restored guest's wall clock is frozen at snapshot time, so
// after a fork the drift is typically large; small drifts within this window
// are left alone to avoid fighting any in-guest NTP discipline.
const clockStepThresholdNanos = 500 * 1000 * 1000 // 500ms

// handleNotifyForked repairs fork-shared state after a restore:
//  1. reseed the kernel CRNG with the host-supplied entropy,
//  2. step CLOCK_REALTIME toward host wall time when drift is large,
//  3. record the fork generation at /run/sandbox/fork-generation,
//  4. signal userspace processes (SIGUSR2) to reseed their own PRNGs.
//
// Entropy bytes and the absolute clock value are never logged; only counts and
// the applied step magnitude are.
func handleNotifyForked(req *vsock.NotifyForkedRequest) vsock.Response {
	reseeded := reseedCRNG(req.Entropy)

	step := stepClock(req.HostWallClockNanos)

	writeForkGeneration(req.Generation)

	configureNetwork(req.Network)

	mounted := mountVolumes(req.Volumes)

	signaled := signalUserspace()

	fmt.Printf("sandbox-agent: notify_forked generation=%d entropy_bytes=%d reseeded=%v clock_step_ns=%d volumes_mounted=%d signaled=%d\n",
		req.Generation, len(req.Entropy), reseeded, step, mounted, signaled)

	return vsock.Response{
		OK: true,
		NotifyForked: &vsock.NotifyForkedResponse{
			AppliedClockStepNanos: step,
			ReseededRNG:           reseeded,
			SignaledProcesses:     signaled,
		},
	}
}

// rndAddEntropy mirrors the kernel's `struct rand_pool_info`:
//
//	struct rand_pool_info {
//	    int entropy_count;  // entropy credited, in bits
//	    int buf_size;       // length of buf in bytes
//	    __u32 buf[0];       // the entropy itself
//	};
//
// We build it as a packed little-endian byte slice and pass a pointer to the
// RNDADDENTROPY ioctl. unix.RNDADDENTROPY is an architecture-specific constant
// (0x40085203 on amd64/arm64); we use the package constant rather than a
// hardcoded request number so cross-arch builds stay correct.
func reseedCRNG(entropy []byte) bool {
	if len(entropy) == 0 {
		return false
	}

	// header: entropy_count (bits) + buf_size (bytes), both int32, then bytes.
	buf := make([]byte, 8+len(entropy))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(entropy)*8))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(entropy)))
	copy(buf[8:], entropy)

	f, err := os.OpenFile("/dev/urandom", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: open /dev/urandom: %v\n", err)
		return false
	}
	defer f.Close()

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		f.Fd(),
		uintptr(unix.RNDADDENTROPY),
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if errno == 0 {
		return true
	}
	fmt.Fprintf(os.Stderr, "sandbox-agent: RNDADDENTROPY failed (errno %d), falling back to write\n", int(errno))

	// Fallback: writing to /dev/urandom mixes the bytes into the pool without
	// crediting entropy. Worse than the ioctl, but still perturbs CRNG state.
	if _, err := f.Write(entropy); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: write /dev/urandom: %v\n", err)
		return false
	}
	return true
}

// stepClock reads CLOCK_REALTIME, compares it to the host wall clock delivered
// in the notification, and steps the clock when drift exceeds the threshold.
// Returns the signed adjustment applied in nanoseconds (0 when within
// tolerance or on error). The absolute clock value is never logged.
func stepClock(hostWallClockNanos int64) int64 {
	if hostWallClockNanos == 0 {
		return 0
	}

	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &ts); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: clock_gettime: %v\n", err)
		return 0
	}
	guestNanos := ts.Nano()
	drift := hostWallClockNanos - guestNanos
	if drift < 0 {
		if -drift <= clockStepThresholdNanos {
			return 0
		}
	} else if drift <= clockStepThresholdNanos {
		return 0
	}

	target := unix.NsecToTimespec(hostWallClockNanos)
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &target); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: clock_settime: %v\n", err)
		return 0
	}
	return drift
}

// writeForkGeneration records the fork generation at a fixed path so
// inotify-watching runtimes can detect a fork without a signal. Best effort.
func writeForkGeneration(generation uint64) {
	if err := os.MkdirAll("/run/sandbox", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: mkdir /run/sandbox: %v\n", err)
		return
	}
	data := []byte(strconv.FormatUint(generation, 10))
	if err := os.WriteFile("/run/sandbox/fork-generation", data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: write fork-generation: %v\n", err)
	}
}

// guestNetIface is the guest-side NIC name. The snapshot bakes one NIC
// (firecracker.NetIfaceID = "eth0" on the host side); inside the guest the
// kernel names the single virtio-net device eth0.
const guestNetIface = "eth0"

// configureNetwork applies the per-fork eth0 address and default route after a
// restore. Every fork restores the same snapshot-baked guest IP, so without
// re-addressing here all forks would share one guest IP and the host could not
// route return traffic per fork. The address is flushed first so a re-fork or
// re-delivery is idempotent. Best effort: each step logs (addresses only, no
// secrets) and continues so a partial failure still brings the link up. No-op
// when the host did not deliver a network config.
func configureNetwork(cfg *vsock.NotifyForkedNetwork) {
	if cfg == nil {
		return
	}
	addr := fmt.Sprintf("%s/%d", cfg.GuestIP, cfg.PrefixLen)
	steps := [][]string{
		{"ip", "link", "set", guestNetIface, "up"},
		{"ip", "addr", "flush", "dev", guestNetIface},
		{"ip", "addr", "add", addr, "dev", guestNetIface},
		{"ip", "route", "replace", "default", "via", cfg.GatewayIP, "dev", guestNetIface},
	}
	for _, argv := range steps {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed ip(8) argv, no untrusted shell
		if err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-agent: net config %v failed: %v: %s\n", argv, err, out)
		}
	}
	// Point the guest at the controlled resolver so every name lookup goes
	// through the proxy that enforces the name allowlist. Only when the host
	// delivered a resolver IP (DNS egress on); otherwise resolv.conf is left
	// untouched. This runs before the guest resolves anything (the agent
	// applies it on the post-restore notification, ahead of exec traffic).
	if err := writeResolvConf(resolvConfPath, cfg.ResolverIP); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: write resolv.conf: %v\n", err)
	}
	fmt.Printf("sandbox-agent: configured %s addr=%s gateway=%s resolver=%s\n", guestNetIface, addr, cfg.GatewayIP, cfg.ResolverIP)
}

// resolvConfPath is the guest resolver configuration file. It is a package var
// so the write is unit-testable against a temp path.
const resolvConfPath = "/etc/resolv.conf"

// writeResolvConf points the guest's resolver at resolverIP by writing a single
// `nameserver <resolverIP>` line, so every name lookup goes through the
// controlled DNS proxy (the only address the egress chain allows on port 53).
// The write replaces the file in full, so it is idempotent: re-delivery of the
// same resolver yields identical content rather than appended lines. An empty
// resolverIP is a no-op so the feature-off path never clobbers an existing
// resolv.conf. The address is config, not a secret.
func writeResolvConf(path, resolverIP string) error {
	if resolverIP == "" {
		return nil
	}
	content := fmt.Sprintf("nameserver %s\n", resolverIP)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// volumeFSType is the filesystem the host formats every volume backing with.
// Fresh and Snapshot volumes are ext4 images; Share/Clone copy an ext4 seed.
const volumeFSType = "ext4"

// mountVolumes mounts each volume in the post-restore mount table. For every
// entry it mkdir -p's the mount path and mounts the device read-write, or
// read-only (MS_RDONLY) when ReadOnly is set so a shared or read-only volume
// cannot be written from the guest. The host has already rebound each baked
// placeholder drive to this fork's backing before sending the table, so the
// device is in place. It is idempotent: an already-mounted path is skipped
// (a re-delivered notification does not double-mount). Best effort per entry:
// a failure is logged (device and path only, no secrets) and the others still
// mount. Returns the count of devices now mounted at their path.
func mountVolumes(entries []vsock.VolumeMountEntry) int {
	mounted := 0
	for _, e := range entries {
		if e.Device == "" || e.MountPath == "" {
			fmt.Fprintf(os.Stderr, "sandbox-agent: skipping volume with empty device/path: device=%q path=%q\n", e.Device, e.MountPath)
			continue
		}
		if isMounted(e.MountPath) {
			mounted++
			continue
		}
		if err := os.MkdirAll(e.MountPath, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-agent: mkdir mount path %s: %v\n", e.MountPath, err)
			continue
		}
		var flags uintptr
		if e.ReadOnly {
			flags |= unix.MS_RDONLY
		}
		if err := unix.Mount(e.Device, e.MountPath, volumeFSType, flags, ""); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-agent: mount %s at %s (ro=%v): %v\n", e.Device, e.MountPath, e.ReadOnly, err)
			continue
		}
		mounted++
	}
	if len(entries) > 0 {
		fmt.Printf("sandbox-agent: mounted %d/%d volumes\n", mounted, len(entries))
	}
	return mounted
}

// isMounted reports whether mountPath is already a mount point by scanning
// /proc/mounts (field 2 is the mount target). It makes mountVolumes idempotent
// across a re-delivered fork notification. On any read error it returns false so
// the caller attempts the mount (a redundant mount fails loudly rather than
// silently skipping).
func isMounted(mountPath string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[1] == mountPath {
			return true
		}
	}
	return false
}

// signalUserspace sends SIGUSR2 to every userspace process except PID 1 (this
// init) and the agent itself, prompting language runtimes and TLS libraries to
// reseed their PRNGs. Best effort: failures per pid are ignored and the count
// of successful signals is returned.
func signalUserspace() int {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: read /proc: %v\n", err)
		return 0
	}

	signaled := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a numeric pid entry
		}
		if pid == 1 || pid == self {
			continue
		}
		if err := unix.Kill(pid, unix.SIGUSR2); err == nil {
			signaled++
		}
	}
	return signaled
}
