# Jailer-in-pod Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Firecracker jailer run INSIDE the husk pod so every tenant VM runs jailed (per-VM uid/gid, chroot via pivot_root, nested cgroup, dropped caps) instead of the unjailed direct-exec path the husk stub uses today.

**Architecture:** The husk stub today builds a `firecracker.VMConfig` with NO `Jailer` field (`cmd/husk-stub/main.go`), so Firecracker is exec'd directly as the pod's uid 0: there is no per-VM uid, no chroot, no jailer cgroup. We close that gap WITHOUT making the pod privileged beyond what the device plugin grants. The hard problem is `pivot_root(2)`: the jailer pivot_roots into `<chroot-base>/firecracker/<vm-id>/root`, and `pivot_root` requires (a) the new root to be a mount point and (b) the new root's parent mount to NOT have shared propagation. In a pod the container rootfs is typically mounted `MS_SHARED` (or otherwise propagating), so the jailer's `pivot_root` returns `EINVAL` or `EBUSY`. The fix is a one-time, pod-local mount setup the stub performs at Prepare, before launching the jailer: bind-mount the chroot-base onto itself (`mount --bind`) so it IS a mount point, then mark it private (`mount --make-private`) so its propagation does not defeat `pivot_root`. This is done inside the pod's own mount namespace (the husk container already has its own mount ns) and needs only `CAP_SYS_ADMIN`, which we add back as a single, documented capability (the jailer already needs `CAP_SYS_ADMIN` + `CAP_CHOWN` + `CAP_SETUID` + `CAP_SETGID` + `CAP_MKNOD` to build a jail; see `cmd/forkd/jailer.go`). The jailer then nests its cgroup UNDER the pod's existing cgroup (it creates a child of the pod memcg, it does not fight the pod's `memory.max`). `/dev/kvm` and `/dev/net/tun` are already injected by the device plugin; the jailer mknods them inside the chroot from the injected device nodes.

**Tech Stack:** Go (linux-only syscalls in a `//go:build linux` file mirroring `cmd/forkd/fs_linux.go` / `fs_other.go`), the upstream Firecracker `jailer` binary (added to `Dockerfile.husk-stub`), `internal/firecracker` jailer helpers (already implemented and unit-tested), `internal/husk` stub, `internal/controller/huskpod.go` pod spec, `golang.org/x/sys/unix` for `Mount`.

---

## File Structure

Files created or modified, each with one clear responsibility:

- **`cmd/husk-stub/mount_linux.go`** (create): linux-only helper `prepareChrootMount(chrootBase string) error` that bind-mounts the chroot-base onto itself and marks it private so the jailer's `pivot_root` succeeds inside the pod. Mirrors the `fs_linux.go` / `fs_other.go` split forkd already uses.
- **`cmd/husk-stub/mount_other.go`** (create): non-linux stub of `prepareChrootMount` (no-op, returns nil) so the binary builds on darwin for development. Mirrors `cmd/forkd/fs_other.go`.
- **`cmd/husk-stub/mount_linux_test.go`** (create): linux-only test that the mount helper makes the path a private mount point. Marked KVM-CI / bare-metal (needs `CAP_SYS_ADMIN`); darwin cannot run it.
- **`cmd/husk-stub/jailer.go`** (create): `buildHuskJailerConfig(jailerBin, chrootBase, uidRange string, euid int) (firecracker.JailerConfig, error)` validating the husk jailer flags fail-closed, reusing the same uid-range parse and root requirement shape as `cmd/forkd/jailer.go`. NOTE: it does NOT require chroot-base to share a filesystem with the data dir, because the husk pod's chroot-base is on the pod-writable emptyDir, distinct from the read-only snapshot hostPath; the EXDEV copy fallback in `prepareChroot` handles the cross-fs link.
- **`cmd/husk-stub/jailer_test.go`** (create): unit tests for `buildHuskJailerConfig` (disabled, requires root, bad range, valid), runnable on darwin (no syscalls).
- **`cmd/husk-stub/main.go`** (modify): add `--jailer`, `--chroot-base`, `--uid-range` flags; call `prepareChrootMount` then `buildHuskJailerConfig`; set `cfg.Jailer` and `cfg.ChrootFiles` on the `firecracker.VMConfig`; set the kernel as a chroot file.
- **`internal/husk/stub.go`** (modify): thread `cfg.ChrootFiles` so the snapshot mem/vmstate files (known only at Activate from `req.SnapshotDir`) are added to the chroot file set before the VMM loads them. Today `ChrootFiles` is set once at Prepare; the snapshot path is per-activate, so the stub must prepare those files into the chroot at Activate.
- **`internal/controller/huskpod.go`** (modify): add the jailer args to the husk pod spec, add a writable `emptyDir` chroot-base volume, add `CAP_SYS_ADMIN` to the container capabilities (drop ALL, add SYS_ADMIN only), and update the per-field securityContext rationale comments.
- **`internal/controller/huskpod_test.go`** (modify): assert the new jailer args, the chroot-base emptyDir volume + mount, and the single added capability.
- **`Dockerfile.husk-stub`** (modify): install the upstream `jailer` binary alongside `firecracker` (the release tarball ships both).
- **`docs/threat-model.md`** (modify): update Surface 1 and section 1's jailer row to state the husk pod now runs jailed; record the one added capability (`CAP_SYS_ADMIN`) and that the unjailed-husk residual is closed.
- **`docs/husk-pods.md`** (modify): add a "Jailer in the pod" subsection under section 5 describing the mount setup, the nested cgroup, and the device path.

---

## Task 1: Linux chroot-base mount helper (the pivot_root fix)

**Files:**
- Create: `cmd/husk-stub/mount_linux.go`
- Create: `cmd/husk-stub/mount_other.go`
- Create: `cmd/husk-stub/mount_linux_test.go`

- [ ] **Step 1: Write the failing test** (KVM-CI / bare-metal verification: needs `CAP_SYS_ADMIN`; cannot run on darwin or unprivileged kind)

Create `cmd/husk-stub/mount_linux_test.go`:

```go
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
```

Also add the mountinfo helper to the SAME test file (it parses the propagation field; a private mount has NO `shared:` tag in its optional fields):

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOOS=linux go test ./cmd/husk-stub/ -run TestPrepareChrootMountMakesPrivateMountPoint -v`
Expected: FAIL to COMPILE with `undefined: prepareChrootMount` (the helper does not exist yet). This is the correct failing state on darwin (the test body itself is gated to root-only at runtime, but compilation must fail until the helper exists).

- [ ] **Step 3: Write minimal implementation**

Create `cmd/husk-stub/mount_linux.go`:

```go
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
```

Create `cmd/husk-stub/mount_other.go`:

```go
//go:build !linux

package main

// prepareChrootMount on non-linux platforms is a no-op. The husk stub's jailer
// path only runs on linux (it needs mount(2) and the Firecracker jailer); on
// darwin this exists solely so the binary builds for development, mirroring
// cmd/forkd/fs_other.go.
func prepareChrootMount(chrootBase string) error {
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run on darwin (development): `go test ./cmd/husk-stub/ -run TestPrepareChrootMountMakesPrivateMountPoint -v`
Expected: the linux-only test file is excluded by build tags on darwin, so it does not run; instead confirm the linux build compiles: `GOOS=linux GOARCH=amd64 go build ./cmd/husk-stub/`
Expected: builds cleanly (exit 0).

KVM-CI / bare-metal verification (the real assertion, runs as root with CAP_SYS_ADMIN on the `kvm-test.yaml` runner or the #16 self-hosted node): `GOOS=linux go test ./cmd/husk-stub/ -run TestPrepareChrootMountMakesPrivateMountPoint -v`
Expected: PASS with the mountinfo assertion satisfied (base is a private mount point); on an unprivileged host the test SKIPS with "needs CAP_SYS_ADMIN".

- [ ] **Step 5: Commit**

```bash
git add cmd/husk-stub/mount_linux.go cmd/husk-stub/mount_other.go cmd/husk-stub/mount_linux_test.go
git commit -m "feat: husk stub bind-mounts and privatizes the jailer chroot base for in-pod pivot_root

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: buildHuskJailerConfig (fail-closed flag validation)

**Files:**
- Create: `cmd/husk-stub/jailer.go`
- Create: `cmd/husk-stub/jailer_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/husk-stub/jailer_test.go`:

```go
package main

import "testing"

func TestBuildHuskJailerConfigDisabled(t *testing.T) {
	// An empty jailer binary disables the jailer (the development direct-exec
	// path). The returned config must be the zero (disabled) value.
	cfg, err := buildHuskJailerConfig("", "/run/husk/jail", "64000-64999", 0)
	if err != nil {
		t.Fatalf("buildHuskJailerConfig disabled: %v", err)
	}
	if cfg.Enabled() {
		t.Fatal("empty jailer binary must yield a disabled JailerConfig")
	}
}

func TestBuildHuskJailerConfigRequiresRoot(t *testing.T) {
	// The jailer needs root (CAP_SYS_ADMIN/CHOWN/SETUID/SETGID/MKNOD) to build a
	// jail; refuse fail-closed when not root.
	_, err := buildHuskJailerConfig("/usr/local/bin/jailer", "/run/husk/jail", "64000-64999", 1000)
	if err == nil {
		t.Fatal("buildHuskJailerConfig accepted a non-root euid")
	}
}

func TestBuildHuskJailerConfigBadRangeFailsClosed(t *testing.T) {
	if _, err := buildHuskJailerConfig("/usr/local/bin/jailer", "/run/husk/jail", "0-10", 0); err == nil {
		t.Fatal("buildHuskJailerConfig accepted a uid range including 0 (root)")
	}
	if _, err := buildHuskJailerConfig("/usr/local/bin/jailer", "/run/husk/jail", "garbage", 0); err == nil {
		t.Fatal("buildHuskJailerConfig accepted a malformed uid range")
	}
}

func TestBuildHuskJailerConfigValid(t *testing.T) {
	cfg, err := buildHuskJailerConfig("/usr/local/bin/jailer", "/run/husk/jail", "64000-64999", 0)
	if err != nil {
		t.Fatalf("buildHuskJailerConfig valid: %v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("valid jailer config must be enabled")
	}
	if cfg.ChrootBaseDir != "/run/husk/jail" {
		t.Fatalf("ChrootBaseDir = %q, want /run/husk/jail", cfg.ChrootBaseDir)
	}
	if cfg.UIDRange != [2]uint32{64000, 64999} {
		t.Fatalf("UIDRange = %v, want [64000 64999]", cfg.UIDRange)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/husk-stub/ -run TestBuildHuskJailerConfig -v`
Expected: FAIL to COMPILE with `undefined: buildHuskJailerConfig`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/husk-stub/jailer.go`:

```go
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/paperclipinc/mitos/internal/firecracker"
)

// parseHuskUIDRange parses "low-high" (inclusive). uid 0 is refused: jailed VMs
// must never run as root. It mirrors cmd/forkd's parseUIDRange so the two jailer
// front ends share the same fail-closed shape.
func parseHuskUIDRange(s string) (uint32, uint32, error) {
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

// buildHuskJailerConfig validates the husk pod's jailer flags and produces the
// firecracker.JailerConfig the stub launches each VM through. It fails closed on
// every misconfiguration (malformed/root-including uid range, non-root euid).
//
// Unlike cmd/forkd's buildJailerConfig it does NOT require the chroot base to
// share a filesystem with the data dir: in the husk pod the chroot base lives on
// a pod-writable emptyDir, while the snapshot/kernel come from a READ-ONLY node
// hostPath, so the two are intentionally on different filesystems. prepareChroot
// already handles that with its EXDEV copy fallback (it copies the ~680 MiB mem
// file into the chroot once at Activate). The same-filesystem CoW optimization
// is a forkd-builder concern, not a husk-runner one.
//
// An empty jailerBin disables the jailer (the development direct-exec path; the
// caller logs a loud warning and the threat model flags the residual). euid is
// the caller's effective uid (os.Geteuid()), injected so the check is testable.
func buildHuskJailerConfig(jailerBin, chrootBase, uidRange string, euid int) (firecracker.JailerConfig, error) {
	if jailerBin == "" {
		return firecracker.JailerConfig{}, nil
	}
	low, high, err := parseHuskUIDRange(uidRange)
	if err != nil {
		return firecracker.JailerConfig{}, err
	}
	if euid != 0 {
		return firecracker.JailerConfig{}, fmt.Errorf("--jailer requires the husk stub to run as root (euid 0, currently %d): the jailer needs CAP_SYS_ADMIN, CAP_CHOWN, CAP_SETUID, CAP_SETGID, and CAP_MKNOD to build each VM's jail; run unjailed only for development by omitting --jailer", euid)
	}
	return firecracker.JailerConfig{
		JailerBin:     jailerBin,
		ChrootBaseDir: chrootBase,
		UIDRange:      [2]uint32{low, high},
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/husk-stub/ -run TestBuildHuskJailerConfig -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/husk-stub/jailer.go cmd/husk-stub/jailer_test.go
git commit -m "feat: husk stub jailer config builder, fail-closed on misconfiguration

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Thread snapshot files into the chroot at Activate

The stub sets `ChrootFiles` once at Prepare, but the snapshot `mem`/`vmstate` paths are known only at Activate (from `req.SnapshotDir`). Under the jailer those files must be hard-linked (or copied on EXDEV) into the per-VM chroot before `LoadSnapshotWithOverrides`, exactly as the fork engine does (`internal/fork/engine.go:864`). Add a per-activate chroot-prepare seam to the stub.

**Files:**
- Modify: `internal/husk/stub.go`
- Test: `internal/husk/stub_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/husk/stub_test.go`:

```go
func TestActivatePreparesSnapshotChrootFilesWhenJailed(t *testing.T) {
	// When the stub is configured with a jailer, Activate must prepare the
	// snapshot mem+vmstate into the per-VM chroot (via the injected prepare seam)
	// BEFORE loading them, so the jailed Firecracker can open them inside its
	// chroot. We assert the seam is called with the snapshot files and the VM id.
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snap")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"mem", "vmstate"} {
		if err := os.WriteFile(filepath.Join(snapDir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var preparedFiles []string
	cfg := firecracker.VMConfig{ID: "husk"}
	cfg.Jailer.JailerBin = "/usr/local/bin/jailer" // marks the config jailed

	stub := New(cfg, Options{
		Start:  func(firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil },
		Ready:  func(string, time.Duration) error { return nil },
		Notify: func(string, uint64, []byte, ActivateRequest) error { return nil },
		Verify: func(ActivateRequest) error { return nil },
		PrepareChroot: func(vmID string, files []string) error {
			preparedFiles = append([]string(nil), files...)
			if vmID != "husk" {
				t.Errorf("PrepareChroot vmID = %q, want husk", vmID)
			}
			return nil
		},
	})
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := stub.Activate(context.Background(), ActivateRequest{SnapshotDir: snapDir}); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	wantMem := filepath.Join(snapDir, "mem")
	wantState := filepath.Join(snapDir, "vmstate")
	if len(preparedFiles) != 2 || preparedFiles[0] != wantMem || preparedFiles[1] != wantState {
		t.Fatalf("PrepareChroot files = %v, want [%s %s]", preparedFiles, wantMem, wantState)
	}
}

func TestActivateSkipsChrootPrepareWhenDirectExec(t *testing.T) {
	// With NO jailer configured the stub must NOT call the chroot-prepare seam:
	// direct-exec needs no chroot. This keeps the development path unchanged.
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snap")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}

	called := false
	stub := New(firecracker.VMConfig{ID: "husk"}, Options{
		Start:         func(firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil },
		Ready:         func(string, time.Duration) error { return nil },
		Notify:        func(string, uint64, []byte, ActivateRequest) error { return nil },
		Verify:        func(ActivateRequest) error { return nil },
		PrepareChroot: func(string, []string) error { called = true; return nil },
	})
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := stub.Activate(context.Background(), ActivateRequest{SnapshotDir: snapDir}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if called {
		t.Fatal("PrepareChroot was called for a direct-exec (unjailed) stub")
	}
}
```

NOTE: this relies on the existing `fakeVMM` in `internal/husk/stub_test.go`. If the existing fake is named differently, use that name; the test logic is unchanged. Confirm with `grep -n "type fakeVMM\|func.*fakeVMM\|LoadSnapshotWithOverrides" internal/husk/stub_test.go` and adapt the type name only.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/husk/ -run 'TestActivate(PreparesSnapshotChrootFilesWhenJailed|SkipsChrootPrepareWhenDirectExec)' -v`
Expected: FAIL to COMPILE with `unknown field 'PrepareChroot' in struct literal of type Options`.

- [ ] **Step 3: Write minimal implementation**

In `internal/husk/stub.go`, add the seam type, the Options field, the Stub field, the New wiring, and the Activate call.

Add this type near the other seam types (after the `notifier` type, around line 83):

```go
// chrootPreparer hard-links (or copies on EXDEV) the per-activate snapshot files
// into the jailed VM's chroot before the VMM loads them. It is the husk analog
// of the fork engine's ChrootFiles handling: the snapshot mem/vmstate paths are
// known only at Activate (from the request's SnapshotDir), so they cannot be set
// in the Prepare-time VMConfig.ChrootFiles. The production seam calls
// firecracker.PrepareChrootForVM; tests inject a fake. It is a no-op for an
// unjailed (direct-exec) stub. The file paths carry no secrets.
type chrootPreparer func(vmID string, files []string) error
```

Add to the `Options` struct (after `OnActivated`, before `PrepareSnapshotDir`):

```go
	// PrepareChroot hard-links the per-activate snapshot files into the jailed
	// VM's chroot before load. Nil uses the production seam
	// (firecracker.PrepareChrootForVM) when the VMConfig is jailed, and is unused
	// when the stub runs direct-exec. Tests inject a fake to assert the files.
	PrepareChroot chrootPreparer
```

Add to the `Stub` struct (after `onActivated`):

```go
	prepareChroot chrootPreparer
```

In `New`, after the `onActivated: opts.OnActivated,` assignment in the struct literal, add:

```go
		prepareChroot: opts.PrepareChroot,
```

And after the `if s.notify == nil { ... }` block, add the production default:

```go
	if s.prepareChroot == nil {
		s.prepareChroot = func(vmID string, files []string) error {
			return firecracker.PrepareChrootForVM(s.cfg, vmID, files)
		}
	}
```

In `Activate`, immediately AFTER the `memFile`/`vmStateFile` are computed and AFTER the verify gate passes, but BEFORE `s.vm.LoadSnapshotWithOverrides`, add:

```go
	// Jailed VMs: the snapshot files live on the read-only node mount, OUTSIDE
	// the per-VM chroot. Hard-link (or copy on EXDEV) them into the chroot at
	// their mirrored host paths before the jailed Firecracker loads them, exactly
	// as the fork engine does (internal/fork/engine.go ChrootFiles). A direct-exec
	// stub has no chroot, so the seam is a no-op there. FAIL CLOSED: if the files
	// cannot be placed in the chroot, the load would fail with an opaque
	// Firecracker error, so we surface the prepare error here instead.
	if s.cfg.Jailer.Enabled() {
		if err := s.prepareChroot(s.cfg.ID, []string{memFile, vmStateFile}); err != nil {
			werr := fmt.Errorf("husk: prepare snapshot files in jailer chroot: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/husk/ -run 'TestActivate(PreparesSnapshotChrootFilesWhenJailed|SkipsChrootPrepareWhenDirectExec)' -v`
Expected: PASS (both tests). Then run the whole package: `go test ./internal/husk/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/husk/stub.go internal/husk/stub_test.go
git commit -m "feat: husk Activate prepares snapshot files into the jailer chroot

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Export PrepareChrootForVM from internal/firecracker

The stub's production chroot-prepare seam calls `firecracker.PrepareChrootForVM`, which does not exist yet (today `prepareChroot` is unexported and called only inside `startJailedVM`). Add an exported wrapper that creates the chroot run dir, prepares the files, and chowns them to the jailed uid. Since the husk stub does not own the per-VM uid (the jailer allocates it on launch), the wrapper prepares files WITHOUT a chown: the jailer launch path (`startJailedVM`) already chowns the prepared files via `chownIntoJail`, and these per-activate files are prepared just before that launch. The wrapper therefore mirrors only the link step.

**Files:**
- Modify: `internal/firecracker/jailer.go`
- Test: `internal/firecracker/jailer_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/firecracker/jailer_test.go`:

```go
func TestPrepareChrootForVMLinksFiles(t *testing.T) {
	// PrepareChrootForVM is the exported seam the husk stub calls at Activate to
	// place per-activate snapshot files in the chroot. It must hard-link each file
	// into the mirrored chroot location, refusing paths outside the allowed roots
	// (same guard as prepareChroot).
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	src := filepath.Join(dataDir, "templates", "t1", "snapshot", "mem")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("snap"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultVMConfig()
	cfg.ID = "husk"
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), dataDir)

	if err := PrepareChrootForVM(cfg, "husk", []string{src}); err != nil {
		t.Fatalf("PrepareChrootForVM: %v", err)
	}
	linked := chrootPath(cfg.Jailer.ChrootBaseDir, "husk", src)
	info, err := os.Stat(linked)
	if err != nil {
		t.Fatalf("linked file missing: %v", err)
	}
	srcInfo, _ := os.Stat(src)
	if !os.SameFile(info, srcInfo) {
		t.Fatalf("expected %q to be a hard link of %q", linked, src)
	}
}

func TestPrepareChrootForVMRefusesEscapingPath(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultVMConfig()
	cfg.Jailer = testJailerConfig(filepath.Join(root, "jail"), dataDir)
	if err := PrepareChrootForVM(cfg, "husk", []string{outside}); err == nil {
		t.Fatal("PrepareChrootForVM accepted a path outside the data dir")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/firecracker/ -run 'TestPrepareChrootForVM' -v`
Expected: FAIL to COMPILE with `undefined: PrepareChrootForVM`.

- [ ] **Step 3: Write minimal implementation**

In `internal/firecracker/jailer.go`, add immediately above `prepareChroot` (around line 116):

```go
// PrepareChrootForVM hard-links (or copies on EXDEV) the given host files into
// the VM's chroot at their mirrored locations, creating the chroot directory
// tree as needed. It is the exported seam the husk stub calls at Activate to
// place the per-activate snapshot files (mem, vmstate) in the chroot before the
// jailed Firecracker loads them; the fork engine instead passes these via
// VMConfig.ChrootFiles at launch. It applies the same guard as prepareChroot
// (paths must be absolute and within the VM workspace or the data dir), so a
// caller cannot expose an arbitrary host file inside the jail. It does NOT chown
// the files to the jailed uid: the jailer launch path chowns the chroot file set
// (chownIntoJail) after it allocates the uid, and these files are prepared just
// before that launch.
func PrepareChrootForVM(cfg VMConfig, vmID string, files []string) error {
	if _, err := prepareChroot(cfg, vmID, files); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/firecracker/ -run 'TestPrepareChrootForVM' -v`
Expected: PASS (both tests). Then: `go test ./internal/firecracker/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/firecracker/jailer.go internal/firecracker/jailer_test.go
git commit -m "feat: export PrepareChrootForVM for per-activate chroot file prep

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Wire the jailer into cmd/husk-stub/main.go

**Files:**
- Modify: `cmd/husk-stub/main.go`

- [ ] **Step 1: Write the failing test** (compile-level guard via a focused unit test on the new flag plumbing helper)

The flag wiring in `run()` is not unit-testable directly (it spawns a VMM), but the JAILER ASSEMBLY is. Add a small testable helper and test it. Add to `cmd/husk-stub/jailer_test.go`:

```go
func TestHuskVMConfigJailerWiring(t *testing.T) {
	// huskVMConfig assembles the VMConfig the stub launches, attaching the jailer
	// config and the kernel as a chroot file when jailed. With a jailer it must
	// set cfg.Jailer (enabled) and include the kernel in ChrootFiles; without one
	// it must leave both unset (direct exec).
	jailed := buildHuskVMConfigForTest(t, "/usr/local/bin/jailer", "/run/husk/jail", "64000-64999")
	if !jailed.Jailer.Enabled() {
		t.Fatal("expected jailed VMConfig to carry an enabled jailer")
	}
	foundKernel := false
	for _, f := range jailed.ChrootFiles {
		if f == "/var/lib/mitos/kernel/vmlinux" {
			foundKernel = true
		}
	}
	if !foundKernel {
		t.Fatalf("expected kernel in ChrootFiles, got %v", jailed.ChrootFiles)
	}

	direct := buildHuskVMConfigForTest(t, "", "/run/husk/jail", "64000-64999")
	if direct.Jailer.Enabled() {
		t.Fatal("expected direct-exec VMConfig to have a disabled jailer")
	}
	if len(direct.ChrootFiles) != 0 {
		t.Fatalf("expected no ChrootFiles for direct exec, got %v", direct.ChrootFiles)
	}
}
```

And add the test-only constructor at the bottom of `cmd/husk-stub/jailer_test.go` that calls the same helper `run()` uses with euid forced to 0:

```go
// buildHuskVMConfigForTest exercises huskVMConfig with a forced root euid so the
// jailer validation passes off-root in CI; it fatals on any build error.
func buildHuskVMConfigForTest(t *testing.T, jailerBin, chrootBase, uidRange string) firecracker.VMConfig {
	t.Helper()
	cfg, err := huskVMConfig(huskVMParams{
		firecrackerBin: "/usr/local/bin/firecracker",
		kernel:         "/var/lib/mitos/kernel/vmlinux",
		workdir:        "/run/husk/vm",
		vcpus:          1,
		memMiB:         512,
		jailerBin:      jailerBin,
		chrootBase:     chrootBase,
		uidRange:       uidRange,
		euid:           0,
	})
	if err != nil {
		t.Fatalf("huskVMConfig: %v", err)
	}
	return cfg
}
```

(Add `"github.com/paperclipinc/mitos/internal/firecracker"` to the test file imports.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/husk-stub/ -run TestHuskVMConfigJailerWiring -v`
Expected: FAIL to COMPILE with `undefined: huskVMConfig` and `undefined: huskVMParams`.

- [ ] **Step 3: Write minimal implementation**

In `cmd/husk-stub/main.go`, add the params struct and the assembly helper (place them just above `func main()`):

```go
// huskVMParams carries the inputs huskVMConfig needs to assemble the stub's
// VMConfig. euid is injected so the jailer root requirement is testable off-root.
type huskVMParams struct {
	firecrackerBin string
	kernel         string
	workdir        string
	vcpus          int
	memMiB         int
	jailerBin      string
	chrootBase     string
	uidRange       string
	euid           int
}

// huskVMConfig builds the firecracker.VMConfig the stub launches. When a jailer
// binary is configured it attaches a fail-closed JailerConfig (per-VM uid/gid,
// chroot, nested cgroup) and lists the kernel as a chroot file so the jailed
// Firecracker can open it inside the jail; the snapshot mem/vmstate are added per
// activate (internal/husk Activate). With no jailer binary it returns the prior
// direct-exec config unchanged (development only; the threat model flags it).
func huskVMConfig(p huskVMParams) (firecracker.VMConfig, error) {
	cfg := firecracker.VMConfig{
		ID:             huskSandboxID,
		FirecrackerBin: p.firecrackerBin,
		WorkDir:        p.workdir,
		KernelPath:     p.kernel,
		SocketPath:     filepath.Join(p.workdir, "firecracker.sock"),
		VcpuCount:      p.vcpus,
		MemSizeMib:     p.memMiB,
	}
	jailerCfg, err := buildHuskJailerConfig(p.jailerBin, p.chrootBase, p.uidRange, p.euid)
	if err != nil {
		return firecracker.VMConfig{}, err
	}
	cfg.Jailer = jailerCfg
	if jailerCfg.Enabled() {
		// DataDir bounds which host files may be exposed in the chroot; the husk
		// kernel and snapshot live under the node data dir mount. The engine sets
		// this from its data dir; here the stub's allowed root is the kernel's and
		// snapshot's mount root.
		cfg.Jailer.DataDir = "/var/lib/mitos"
		if p.kernel != "" {
			cfg.ChrootFiles = []string{p.kernel}
		}
	}
	return cfg, nil
}
```

NOTE: `huskSandboxID` already exists in `main.go` (const, value `"husk"`); reuse it for the VM id so the chroot layout and the sandbox-API registration agree.

Now add the three flags in `run()` next to the existing flags (after `memMiB`):

```go
		jailerBin  = flag.String("jailer", "", "path to the Firecracker jailer binary; every VM is launched through it with a per-VM uid, chroot (pivot_root), and a cgroup nested under the pod's. Empty disables the jailer (development only; the VM then runs unjailed as the pod uid, flagged in the threat model)")
		chrootBase = flag.String("chroot-base", "/run/husk/jail", "jailer chroot base directory; must be on a pod-WRITABLE volume (an emptyDir), distinct from the read-only snapshot mount. The stub bind-mounts and privatizes it so the jailer can pivot_root inside the pod")
		uidRange   = flag.String("uid-range", "64000-64999", "inclusive low-high uid/gid range the jailer allocates a dedicated per-VM uid from; uid 0 is refused")
```

Then, in the dormant-bring-up branch (after the `if *workdir == "" {` check and after `os.MkdirAll(*workdir, ...)`, and BEFORE the `cfg := firecracker.VMConfig{...}` literal), REPLACE the inline `cfg := firecracker.VMConfig{...}` literal (lines 229-237) with:

```go
	// Jailer setup MUST happen before the dormant VMM is prepared: the jailer
	// pivot_roots into the per-VM chroot under --chroot-base, which inside a pod
	// requires the chroot base to be a PRIVATE MOUNT POINT (a pod's rootfs is
	// commonly shared, and pivot_root refuses a new root whose parent mount is
	// shared). Make the base a private self-bind mount once, here, in the pod's
	// own mount namespace. This is a no-op on the development direct-exec path
	// (empty --jailer) and on non-linux (mount_other.go).
	if *jailerBin != "" {
		if err := os.MkdirAll(*chrootBase, 0o755); err != nil {
			return fmt.Errorf("create jailer chroot base %s: %w", *chrootBase, err)
		}
		if err := prepareChrootMount(*chrootBase); err != nil {
			return fmt.Errorf("prepare jailer chroot base mount: %w", err)
		}
	} else {
		fmt.Fprintln(os.Stderr, "husk-stub: WARNING running UNJAILED (no --jailer): the VM runs as the pod uid with no chroot or per-VM uid; development only, do not use for untrusted tenants")
	}

	cfg, err := huskVMConfig(huskVMParams{
		firecrackerBin: *firecrackerBin,
		kernel:         *kernel,
		workdir:        *workdir,
		vcpus:          *vcpus,
		memMiB:         *memMiB,
		jailerBin:      *jailerBin,
		chrootBase:     *chrootBase,
		uidRange:       *uidRange,
		euid:           os.Geteuid(),
	})
	if err != nil {
		return fmt.Errorf("build husk VM config: %w", err)
	}
```

(The later `husk.New(cfg, husk.Options{...})` call already consumes `cfg` unchanged.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/husk-stub/ -v`
Expected: PASS (the wiring test plus the jailer-config tests). Then confirm both builds:
`go build ./cmd/husk-stub/` and `GOOS=linux GOARCH=amd64 go build ./cmd/husk-stub/`
Expected: both exit 0.

- [ ] **Step 5: Commit**

```bash
git add cmd/husk-stub/main.go cmd/husk-stub/jailer_test.go
git commit -m "feat: husk stub launches Firecracker through the jailer inside the pod

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: Husk pod spec: jailer args, chroot-base emptyDir, CAP_SYS_ADMIN

**Files:**
- Modify: `internal/controller/huskpod.go`
- Test: `internal/controller/huskpod_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/huskpod_test.go` (use the same helpers the existing husk pod tests use to build a pod; confirm the builder call shape with `grep -n "buildHuskPod" internal/controller/huskpod_test.go`):

```go
func TestHuskPodRunsJailed(t *testing.T) {
	r := &SandboxPoolReconciler{} // matches the existing test construction; set Scheme via the suite helper if the existing tests do
	pool := &v1alpha1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	template := &v1alpha1.SandboxTemplate{}
	pod := r.buildHuskPod(pool, template, HuskPodOptions{StubImage: "img", SnapshotID: "t1"})

	c := pod.Spec.Containers[0]

	// The jailer flags must be present so the stub launches jailed.
	joined := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--jailer /usr/local/bin/jailer",
		"--chroot-base /run/husk/jail",
		"--uid-range 64000-64999",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("husk pod args missing %q; got: %s", want, joined)
		}
	}

	// A pod-writable chroot-base emptyDir volume must be mounted at the chroot base.
	foundVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "jailer-chroot" {
			if v.EmptyDir == nil {
				t.Error("jailer-chroot volume must be an emptyDir (pod-writable)")
			}
			foundVol = true
		}
	}
	if !foundVol {
		t.Error("husk pod missing the jailer-chroot emptyDir volume")
	}
	foundMount := false
	for _, m := range c.VolumeMounts {
		if m.Name == "jailer-chroot" {
			if m.MountPath != "/run/husk/jail" {
				t.Errorf("jailer-chroot mount path = %q, want /run/husk/jail", m.MountPath)
			}
			if m.ReadOnly {
				t.Error("jailer-chroot mount must be writable")
			}
			foundMount = true
		}
	}
	if !foundMount {
		t.Error("husk pod missing the jailer-chroot volume mount")
	}

	// Exactly CAP_SYS_ADMIN is added back (for mount(2) + the jailer); ALL dropped.
	caps := c.SecurityContext.Capabilities
	if len(caps.Drop) != 1 || caps.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", caps.Drop)
	}
	if len(caps.Add) != 1 || caps.Add[0] != "SYS_ADMIN" {
		t.Errorf("capabilities.add = %v, want [SYS_ADMIN]", caps.Add)
	}
}
```

Add `"strings"` to the test imports if not present.

- [ ] **Step 2: Run test to verify it fails**

Run (envtest assets, per CLAUDE.md): `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestHuskPodRunsJailed -v`
Expected: FAIL: the args lack `--jailer`, there is no `jailer-chroot` volume, and `capabilities.add` is empty.

- [ ] **Step 3: Write minimal implementation**

In `internal/controller/huskpod.go`:

First add the constants near the other husk path constants (after `huskWorkdir`, around line 50):

```go
	// huskJailerBin is the in-image path of the Firecracker jailer binary
	// (Dockerfile.husk-stub installs it next to firecracker). The husk stub
	// launches every VM through it so tenant VMs run jailed.
	huskJailerBin = "/usr/local/bin/jailer"
	// huskChrootBase is the pod-WRITABLE jailer chroot base. It is an emptyDir
	// (not the read-only snapshot hostPath): the jailer pivot_roots into a per-VM
	// dir under it and hard-links/copies the snapshot files in, both of which need
	// a writable filesystem. The stub bind-mounts + privatizes it so pivot_root
	// works inside the pod.
	huskChrootBase = "/run/husk/jail"
	// huskUIDRange is the inclusive uid/gid range the in-pod jailer allocates a
	// dedicated per-VM uid from (uid 0 refused). It matches forkd's default.
	huskUIDRange = "64000-64999"
```

In `buildHuskPod`, append the jailer flags to `args` (right after the existing `args := []string{...}` literal block, before the snapshot-verify gate block):

```go
	// Launch every VM through the jailer: a per-VM uid/gid from --uid-range, a
	// chroot (pivot_root) under --chroot-base, and a cgroup the jailer nests under
	// the pod's own cgroup (it creates a child, it does not override the pod's
	// memory.max). This is what makes a microVM escape land as a throwaway uid in
	// an empty chroot instead of the pod's uid 0. The stub privatizes --chroot-base
	// so pivot_root works inside the pod (cmd/husk-stub prepareChrootMount).
	args = append(args,
		"--jailer", huskJailerBin,
		"--chroot-base", huskChrootBase,
		"--uid-range", huskUIDRange,
	)
```

Add the writable chroot-base volume + mount. Place this near the other `volumes`/`mounts` appends, UNCONDITIONALLY (it is needed whenever the stub runs, independent of `SnapshotID`), just after `var volumes []corev1.Volume` / `var mounts []corev1.VolumeMount`:

```go
	// The jailer chroot base must be a pod-WRITABLE filesystem (an emptyDir),
	// separate from the read-only snapshot hostPath: the jailer creates per-VM
	// chroot dirs here and the stub links/copies the snapshot into them. emptyDir
	// is pod-scoped and torn down with the pod, so no per-VM jail outlives it.
	volumes = append(volumes, corev1.Volume{
		Name: "jailer-chroot",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})
	mounts = append(mounts, corev1.VolumeMount{Name: "jailer-chroot", MountPath: huskChrootBase})
```

Update the container `SecurityContext.Capabilities` from drop-ALL-only to drop-ALL-add-SYS_ADMIN. Replace the existing:

```go
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
```

with:

```go
						Capabilities: &corev1.Capabilities{
							// Drop everything, then add back EXACTLY CAP_SYS_ADMIN.
							// The stub needs it for two things, both inside the pod's
							// own mount namespace: (1) mount(2) to bind + privatize
							// the jailer chroot base so the jailer can pivot_root in a
							// pod; (2) the jailer's own jail construction (cgroup +
							// namespace + chroot). This is the SINGLE capability the
							// jailed-in-pod model adds; the jailer then drops to an
							// unprivileged per-VM uid inside the jail, so a VMM escape
							// does NOT inherit CAP_SYS_ADMIN. Without it the VM would
							// run UNJAILED as the pod uid, which is unacceptable for
							// untrusted tenants. Documented in docs/threat-model.md.
							Drop: []corev1.Capability{"ALL"},
							Add:  []corev1.Capability{"SYS_ADMIN"},
						},
```

Also update the long securityContext rationale comment block (around lines 215-233) where it says capabilities "Drop ALL, add NONE": change the relevant lines to state that exactly `CAP_SYS_ADMIN` is added back for the in-pod jailer (mount + pivot_root + jail construction), and that this is the single documented capability the jailed-in-pod model requires. Replace this bullet:

```go
	//   - Capabilities Drop ALL, add NONE. The dormant stub only Prepares a
	//     Firecracker VMM (open /dev/kvm via the device plugin, create files
	//     under the pod-local workdir, bind a unix socket); none of that needs a
	//     Linux capability. Networking capabilities (e.g. NET_ADMIN for tap
	//     setup) arrive with the networking slice, not here; we add back none so
	//     this slice stays minimal.
```

with:

```go
	//   - Capabilities Drop ALL, add EXACTLY CAP_SYS_ADMIN. The stub launches
	//     each VM through the jailer, which needs CAP_SYS_ADMIN to build the jail
	//     (cgroup + mount namespace + chroot); the stub itself needs it for the
	//     one mount(2) that bind-mounts and privatizes the chroot base so the
	//     jailer can pivot_root inside the pod (a pod's rootfs is commonly a shared
	//     mount, which pivot_root refuses). This is the SINGLE capability the
	//     jailed-in-pod model adds; the jailer then drops to an unprivileged per-VM
	//     uid inside the jail, so a guest that escapes the microVM lands as a
	//     throwaway uid in an empty chroot, NOT with CAP_SYS_ADMIN. Networking
	//     capabilities (e.g. NET_ADMIN for tap setup) arrive with the networking
	//     slice, not here.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestHuskPodRunsJailed -v`
Expected: PASS. Then the full controller suite: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
Expected: PASS (existing husk pod tests still green; if a prior test asserted `len(caps.Add) == 0` or "drop ALL only", update that assertion in the SAME commit to expect the single SYS_ADMIN add, since the security posture intentionally changed).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/huskpod.go internal/controller/huskpod_test.go
git commit -m "feat: husk pod runs jailed (jailer args, chroot emptyDir, CAP_SYS_ADMIN)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: Ship the jailer binary in the husk-stub image

**Files:**
- Modify: `Dockerfile.husk-stub`

- [ ] **Step 1: Write the failing test**

Dockerfiles have no unit test; the verification is a build-time assertion. Add a build assertion stage so a missing jailer fails the image build (this is the "test"). After the firecracker install RUN block (line 31), the firecracker release tarball ALSO contains `jailer-${FC_VERSION}-x86_64`; install it and assert both exist. We will modify the existing RUN to install both, then add an assertion RUN.

- [ ] **Step 2: Run to verify it fails (current image lacks the jailer)**

Run: `docker build -f Dockerfile.husk-stub -t husk-stub-test . && docker run --rm --entrypoint /bin/sh husk-stub-test -c 'test -x /usr/local/bin/jailer && echo JAILER_OK'`
Expected: FAIL: the container has no `/usr/local/bin/jailer`, so the `test -x` returns non-zero and `JAILER_OK` is not printed (the `docker run` exits non-zero).

- [ ] **Step 3: Write minimal implementation**

In `Dockerfile.husk-stub`, replace the firecracker install RUN (lines 24-31) with one that installs BOTH binaries from the same release tarball:

```dockerfile
RUN apt-get update -qq && apt-get install -y --no-install-recommends curl && \
    curl -fsSL -o /tmp/fc.tgz \
        "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-x86_64.tgz" && \
    tar -xzf /tmp/fc.tgz -C /tmp && \
    mv "/tmp/release-${FC_VERSION}-x86_64/firecracker-${FC_VERSION}-x86_64" /usr/local/bin/firecracker && \
    mv "/tmp/release-${FC_VERSION}-x86_64/jailer-${FC_VERSION}-x86_64" /usr/local/bin/jailer && \
    chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer && \
    rm -rf /tmp/fc.tgz /tmp/release-* && \
    apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*

# Fail the build if either binary is missing: the husk stub launches the VMM
# through the jailer, so the image must carry both.
RUN test -x /usr/local/bin/firecracker && test -x /usr/local/bin/jailer
```

- [ ] **Step 4: Run to verify it passes**

Run: `docker build -f Dockerfile.husk-stub -t husk-stub-test . && docker run --rm --entrypoint /bin/sh husk-stub-test -c 'test -x /usr/local/bin/jailer && echo JAILER_OK'`
Expected: prints `JAILER_OK` and exits 0.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile.husk-stub
git commit -m "build: ship the Firecracker jailer in the husk-stub image

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: KVM-CI / bare-metal end-to-end jailed-activation phase

This proves the WHOLE path on real KVM with pivot_root: a husk stub started WITH the jailer, the chroot-base privatized, a real snapshot activated, and an exec that shows the VM ran as a NON-ZERO per-VM uid in a chroot. It runs ONLY on `kvm-test.yaml` (KVM + `CAP_SYS_ADMIN`) or the #16 self-hosted node. Darwin and kind cannot run it.

**Files:**
- Modify: `.github/workflows/kvm-test.yaml` (add a `husk-jailed` phase)

- [ ] **Step 1: Write the failing test** (the CI phase script; the test is the gate `JAILED_OK`)

Add a step to the existing husk job in `.github/workflows/kvm-test.yaml`. Confirm the existing husk-stub phase shape first with `grep -n "husk-stub\|--activate\|control-socket\|bench template" .github/workflows/kvm-test.yaml` and mirror its snapshot setup. The new phase (reusing the bench template snapshot the husk-stub phase already builds at `$SNAP_DIR`):

```yaml
      - name: Husk jailed-activation (real KVM, pivot_root, per-VM uid)
        run: |
          set -euo pipefail
          sudo mkdir -p /run/husk/jail /run/husk/vm
          # Start a dormant jailed stub: --jailer makes the stub privatize the
          # chroot base and launch Firecracker through the jailer. Needs root
          # (CAP_SYS_ADMIN) and /dev/kvm, both present on this runner.
          sudo ./husk-stub \
            --firecracker /usr/local/bin/firecracker \
            --jailer /usr/local/bin/jailer \
            --chroot-base /run/husk/jail \
            --uid-range 64000-64999 \
            --kernel "$KERNEL" \
            --workdir /run/husk/vm \
            --control-socket /run/husk/control.sock \
            --allow-unverified-snapshots &
          STUB_PID=$!
          # Wait for the dormant control socket.
          for i in $(seq 1 100); do [ -S /run/husk/control.sock ] && break; sleep 0.1; done
          # Activate the bench snapshot in place over the control socket.
          sudo ./husk-stub --activate \
            --control-socket /run/husk/control.sock \
            --snapshot-dir "$SNAP_DIR" \
            --allow-unverified-snapshots | tee /tmp/activate.json
          grep -q '"ok":true' /tmp/activate.json || { echo "JAILED-ACTIVATE-FAILED"; exit 1; }
          # Prove the jail: the jailer created a per-VM chroot under the base, and
          # the Firecracker process runs as a NON-ZERO uid from the range.
          test -d /run/husk/jail/firecracker || { echo "no jailer chroot dir created"; exit 1; }
          FC_PID=$(pgrep -f 'firecracker --api-sock' | head -1)
          FC_UID=$(awk '/^Uid:/{print $2}' /proc/$FC_PID/status)
          echo "firecracker runs as uid $FC_UID"
          [ "$FC_UID" -ge 64000 ] && [ "$FC_UID" -le 64999 ] || { echo "JAILED-UID-FAILED: uid $FC_UID not in 64000-64999"; exit 1; }
          # Prove pivot_root: the FC process root is the per-VM chroot, not /.
          FC_ROOT=$(sudo readlink /proc/$FC_PID/root)
          echo "firecracker root: $FC_ROOT"
          case "$FC_ROOT" in /run/husk/jail/firecracker/*/root) ;; *) echo "JAILED-PIVOT-FAILED: root $FC_ROOT"; exit 1;; esac
          echo "JAILED_OK"
          sudo kill $STUB_PID || true
```

- [ ] **Step 2: Run to verify it fails (before the code lands)**

On the KVM runner, before Tasks 5-7 land, the stub has no `--jailer` flag, so `husk-stub --jailer ...` errors with `flag provided but not defined: -jailer`.
Expected: the phase fails at the dormant-stub start with an unknown-flag error.

- [ ] **Step 3: Write minimal implementation**

The implementation IS the YAML phase above plus Tasks 1-7. No additional code; this task only adds the phase.

- [ ] **Step 4: Run to verify it passes** (KVM-CI / bare-metal: `kvm-test.yaml` or #16 self-hosted runner)

The phase runs in `kvm-test.yaml`. Expected `$GITHUB_STEP_SUMMARY` / log output:
```
firecracker runs as uid 64000
firecracker root: /run/husk/jail/firecracker/husk/root
JAILED_OK
```
Expected: the step exits 0 with `JAILED_OK`. (uid will be the lowest free uid in the range, typically 64000; root path uses the stub's fixed VM id `husk`.)

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/kvm-test.yaml
git commit -m "ci: prove husk runs jailed on KVM (pivot_root, per-VM uid)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: Threat-model and husk-pods doc delta (CLAUDE.md hard rule)

The security surface moved: the husk pod now runs jailed and adds `CAP_SYS_ADMIN`. CLAUDE.md requires `docs/threat-model.md` to change in the SAME plan. Update both docs.

**Files:**
- Modify: `docs/threat-model.md`
- Modify: `docs/husk-pods.md`

- [ ] **Step 1: Write the failing test** (a grep gate that the residual is recorded and the capability is documented)

Add a CI doc-consistency check. In `.github/workflows/ci.yaml`, in the existing lint/docs job (confirm with `grep -n "threat-model\|docs/" .github/workflows/ci.yaml`; if no doc-grep step exists, add one to the `go-lint` job), add:

```yaml
      - name: Threat model records jailed-in-pod surface
        run: |
          set -euo pipefail
          grep -q 'CAP_SYS_ADMIN' docs/threat-model.md
          grep -qi 'jailed' docs/threat-model.md
          grep -qi 'pivot_root' docs/husk-pods.md
          echo "DOC_DELTA_OK"
```

- [ ] **Step 2: Run to verify it fails**

Run: `grep -qi 'pivot_root' docs/husk-pods.md && echo found || echo MISSING`
Expected: `MISSING` (husk-pods.md does not yet mention pivot_root). The grep gate would fail.

- [ ] **Step 3: Write the implementation (the doc edits)**

In `docs/threat-model.md`, update the husk-pod row of the Components table (line 20) so "drop ALL caps" becomes:

```
drop ALL caps, add only CAP_SYS_ADMIN (the in-pod jailer + chroot-base mount), seccomp: RuntimeDefault, read-only snapshot mount, every VM jailed (per-VM uid, chroot, nested cgroup)
```

In Surface 1 (the bullet list around lines 83-87), change the capabilities bullet from:

```
- `capabilities.drop: [ALL]`, none added back,
```

to:

```
- `capabilities.drop: [ALL]`, add EXACTLY `CAP_SYS_ADMIN`: the in-pod jailer needs it to build each VM's jail (cgroup + mount namespace + chroot), and the stub needs the one `mount(2)` that bind-mounts and privatizes the jailer chroot base so the jailer can `pivot_root` inside the pod (a pod's rootfs is commonly a shared mount, which `pivot_root` refuses). The jailer then drops to an unprivileged per-VM uid inside the jail, so a VMM escape does NOT inherit `CAP_SYS_ADMIN`,
```

Add a new paragraph at the END of Surface 1 (after line 104) closing the unjailed-husk residual:

```
JAILED IN THE POD (this closes the prior unjailed-husk residual). Until now the
husk stub exec'd Firecracker DIRECTLY as the pod's uid 0: no per-VM uid, no
chroot, no jailer cgroup, so a microVM escape landed as the pod's uid 0 with the
pod's full view. The stub now launches every VM through the Firecracker jailer
INSIDE the pod (`cmd/husk-stub`, `internal/firecracker/jailer.go`): a dedicated
per-VM uid/gid from `--uid-range` (default 64000-64999, uid 0 refused), a per-VM
chroot the jailer `pivot_root`s into, and a cgroup the jailer NESTS under the
pod's own cgroup (a child memcg, it does not override the pod's `memory.max`).
The in-pod `pivot_root` precondition is handled by the stub: before any launch it
bind-mounts the chroot base (a pod-writable emptyDir, NOT the read-only snapshot
hostPath) onto itself and marks it `MS_PRIVATE`, so the new root is a mount point
with non-shared parent propagation, exactly what `pivot_root(2)` requires. The
cost is the single added `CAP_SYS_ADMIN` above; the benefit is that a guest that
escapes the microVM now lands as a THROWAWAY uid in an EMPTY chroot, not as the
pod's uid 0. CI-proven on real KVM (`kvm-test.yaml` husk jailed-activation phase):
the activated Firecracker runs as a uid in 64000-64999 and its `/proc/<pid>/root`
is the per-VM chroot, not `/`.
```

In Section 1's jailer row (line 295), append to the Detail cell, after the existing "direct-exec dev path" residual sentence:

```
 UPDATE: the husk pod (the default runner) now ALSO runs jailed: `cmd/husk-stub`
launches its VM through the jailer with the same per-VM uid + chroot + nested
cgroup, after privatizing the chroot base so `pivot_root` works inside the pod
(section 0, surface 1). The unjailed-husk path is now the development-only escape
hatch (omit `--jailer`; the stub logs a loud warning), no longer the default.
```

In the per-axis tally table (line 250), update the husk Privilege cell to mention the single added cap:

```
`privileged: false`, `runAsNonRoot: false` (the `/dev/kvm` device exception), no escalation, drop ALL caps and add ONLY `CAP_SYS_ADMIN` for the in-pod jailer (which drops to a per-VM uid inside the jail)
```

In `docs/husk-pods.md`, add a new subsection at the end of section 5 (after line 310):

```
### Jailer in the pod (per-VM uid, chroot, nested cgroup)

The husk pod runs every VM through the Firecracker jailer, INSIDE the pod, so a
tenant VM gets a dedicated per-VM uid/gid, a chroot the jailer `pivot_root`s into,
and a cgroup nested under the pod's own cgroup. This is what makes a microVM
escape land as a throwaway unprivileged uid in an empty chroot rather than the
pod's uid 0.

The one hard part is `pivot_root(2)` inside a pod. The jailer `pivot_root`s into
`<chroot-base>/firecracker/<vm-id>/root`, and `pivot_root` requires the new root
to be a MOUNT POINT whose parent mount does NOT have SHARED propagation. A pod's
container rootfs is commonly mounted shared (or otherwise propagating), so a plain
directory under it fails `pivot_root` with `EINVAL`/`EBUSY`. The stub fixes both
preconditions once, at Prepare, in the pod's own mount namespace
(`cmd/husk-stub/mount_linux.go` `prepareChrootMount`):

1. bind-mount the chroot base onto itself (`mount --bind`), so it BECOMES a mount
   point the jailer can pivot under; and
2. recursively mark it `MS_PRIVATE` (`mount --make-private`), so its propagation
   does not defeat `pivot_root`.

The chroot base is a pod-WRITABLE `emptyDir` (`/run/husk/jail`), deliberately
separate from the READ-ONLY snapshot hostPath: the jailer creates per-VM chroot
dirs and the stub hard-links (or copies on EXDEV) the snapshot mem/vmstate into
them, both of which need a writable filesystem. The cross-filesystem copy fallback
is expected here (the snapshot is on the read-only node mount), so the husk jailer
config does NOT require the chroot base and the snapshot to share a filesystem,
unlike the forkd builder.

This needs exactly one capability beyond what the device plugin grants:
`CAP_SYS_ADMIN`, for the `mount(2)` above and for the jailer's own jail
construction (cgroup + namespace + chroot). The jailer then drops to the
unprivileged per-VM uid inside the jail, so the VMM does NOT keep `CAP_SYS_ADMIN`.
`/dev/kvm` and `/dev/net/tun` are still injected by the device plugin; the jailer
mknods them inside the chroot from the injected device nodes.

CI-proven on real KVM (`kvm-test.yaml` husk jailed-activation phase): a jailed
dormant stub activates a snapshot in place, the activated Firecracker runs as a
uid in 64000-64999, and its `/proc/<pid>/root` is the per-VM chroot, not `/`. The
mount setup itself is verified in the KVM-CI unit phase
(`cmd/husk-stub/mount_linux_test.go`, gated to root/`CAP_SYS_ADMIN`).
```

- [ ] **Step 4: Run to verify it passes**

Run: `grep -q 'CAP_SYS_ADMIN' docs/threat-model.md && grep -qi 'jailed' docs/threat-model.md && grep -qi 'pivot_root' docs/husk-pods.md && echo DOC_DELTA_OK`
Expected: `DOC_DELTA_OK`.

- [ ] **Step 5: Commit**

```bash
git add docs/threat-model.md docs/husk-pods.md .github/workflows/ci.yaml
git commit -m "docs: record jailed-in-pod surface and the single added CAP_SYS_ADMIN

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: Final lint and full-suite gate (both darwin and GOOS=linux)

**Files:** none (verification only)

- [ ] **Step 1: Run gofmt and both lint invocations**

Run:
```bash
gofmt -l cmd/husk-stub internal/husk internal/firecracker internal/controller
golangci-lint run --timeout=5m
GOOS=linux golangci-lint run --timeout=5m
```
Expected: `gofmt -l` prints nothing (all formatted); both `golangci-lint` runs exit 0 with no findings. The `GOOS=linux` run is REQUIRED because `mount_linux.go` (and the linux-only test) are invisible to the darwin run.

- [ ] **Step 2: Run the unit suites that do not need KVM**

Run:
```bash
go test ./internal/firecracker/ ./internal/husk/ ./cmd/husk-stub/
eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/
```
Expected: all PASS.

- [ ] **Step 3: Cross-build the guest-agent and the linux binaries**

Run:
```bash
GOOS=linux GOARCH=amd64 go build ./cmd/husk-stub/ ./guest/agent/
go build ./...
```
Expected: all build cleanly (exit 0).

- [ ] **Step 4: Confirm the KVM-only verifications are marked, not run here**

Confirm (manual check, no command): Task 1's `mount_linux_test.go` SKIPS off-root; Task 8's `husk-jailed` phase runs only on `kvm-test.yaml`. Neither is expected to PASS on darwin/kind; both are documented as KVM-CI / bare-metal (#16) verifications with exact commands and expected output above.

- [ ] **Step 5: Commit (only if any formatting fix was needed)**

```bash
# Only if gofmt or lint required a change:
git add -p
git commit -m "chore: gofmt and lint cleanup for jailer-in-pod

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:**
- Mount setup (bind + make-private for pivot_root): Task 1 (`prepareChrootMount`), wired in Task 5, asserted on KVM in Task 8.
- Jailer invocation from the husk stub: Tasks 2 (`buildHuskJailerConfig`), 5 (`huskVMConfig` + flags + call), 3 (per-activate chroot file prep), 4 (`PrepareChrootForVM`).
- Cgroup placement (nest under the pod, do not fight it): documented in Task 6's args comment and Task 9's docs; the jailer's `--cgroup-version 2` already nests under the task's existing cgroup (it creates a child, per jailer behavior). No code fights `memory.max`.
- Device access (kvm/net tun via device plugin): unchanged and stated in Task 9; the jailer mknods from the injected nodes. No new device wiring needed.
- Pod spec: securityContext (single added CAP_SYS_ADMIN), chroot-base emptyDir volume, jailer args: Task 6.
- Jailer binary in the image: Task 7.
- Threat-model delta in the same plan: Task 9 (required by CLAUDE.md). States the added capability and which posture changed; no capability can be DROPPED here (the unjailed path needed none, the jailed path needs exactly one more), so the doc honestly records an ADDED cap with the offsetting benefit.
- KVM/pivot_root tests marked as KVM-CI / bare-metal: Tasks 1 and 8, both with exact code, run command, and expected output.
- Lint clean darwin AND GOOS=linux: Task 10.

**Placeholder scan:** No TBD/TODO; every code step shows complete code; every run step has an exact command and expected output. The one cross-reference to confirm an existing symbol (`fakeVMM` in Task 3, `buildHuskPod` call shape in Task 6, existing phase shape in Task 8) gives the exact grep to confirm and says to adapt only the name, not the logic.

**Type consistency:** `prepareChrootMount(chrootBase string) error` is identical across `mount_linux.go`, `mount_other.go`, the test, and the call in `main.go`. `buildHuskJailerConfig(jailerBin, chrootBase, uidRange string, euid int) (firecracker.JailerConfig, error)` matches its test and its caller in `huskVMConfig`. `huskVMParams`/`huskVMConfig` match between `main.go` and the test helper. `PrepareChrootForVM(cfg VMConfig, vmID string, files []string) error` matches its test and the stub's production seam. The `chrootPreparer func(vmID string, files []string) error` seam matches the `Options.PrepareChroot` field, the `Stub.prepareChroot` field, and the Activate call. `huskSandboxID` (existing const `"husk"`) is reused for the VM id, so the chroot root path in Task 8's expected output (`/run/husk/jail/firecracker/husk/root`) is consistent with `jailerChrootDir`.
