# Per-Activation Rootfs CoW Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every husk-pod activation its OWN copy-on-write clone of the template `rootfs.ext4` (reflink where the filesystem supports it, full copy otherwise) and point the restored VM's `rootfs` drive at that per-activation file, so concurrent activations of one template never share or corrupt a single rootfs.

**Architecture:** Reuse the existing reflink policy in `internal/volume.Backend` (the `cp --reflink=always` then `--reflink=auto` fallback) by extracting it into an exported `ReflinkCopy(src, dst)` method. The husk `Stub` clones `<template>/rootfs.ext4` to a per-activation file during `Prepare` (the dormant, pre-paid window, so `Activate` stays the engine-only hot path), then after `LoadSnapshotWithOverrides` in `Activate` it rebinds the baked `rootfs` drive to the clone with `PatchDrive("rootfs", clonePath)` (the exact pattern the fork engine already uses for volume drives). The controller mounts a per-node writable CoW directory that is co-located with the template directory on the SAME node filesystem (so reflink can find a shared extent source), and the clone is removed on pod teardown in `Stub.Close`.

**Tech Stack:** Go, Firecracker snapshot/restore, `internal/volume` (FICLONE via `cp --reflink`), `internal/husk` (the activate state machine), `internal/firecracker` (`PatchDrive`), `internal/controller` (husk pod spec).

---

## Key Design Decisions

**(a) Co-location for reflink.** `cp --reflink=always` (FICLONE) requires source and destination on the SAME reflink-capable filesystem (XFS or Btrfs). The clone source is the template rootfs at `<dataDir>/templates/<id>/rootfs.ext4`, already mounted into the husk pod at its real absolute path (`huskpod.go` `template` volume). The per-activation CoW file therefore lives in a SIBLING directory on the same node data dir: `<dataDir>/husk-rootfs`, mounted read-write into the pod at its real absolute path (so `cp` inside the pod sees both files on one filesystem). An `emptyDir` would land on a DIFFERENT filesystem (the pod's ephemeral storage), where FICLONE fails and every activation silently degrades to a full copy; co-locating under `<dataDir>` keeps the reflink fast path available on XFS/Btrfs node disks.

**(b) Clone at Prepare, not Activate.** The clone runs in `Stub.Prepare` (dormant, pre-paid warm period before any claim arrives), NOT in `Stub.Activate` (the claim -> Ready hot path). Justification: the template rootfs is read-only and content-addressed, so a clone taken at Prepare is byte-identical to one taken at Activate; doing it during the dormant window keeps the activate latency the engine cost (load + handshake), exactly as the existing `PrepareSnapshotDir` verification was moved off the hot path. A reflink clone is near-instant, but a full-copy fallback (non-reflink FS) is hundreds of MiB and MUST NOT land on the hot path. One dormant pod owns exactly one activation (the husk model), so a single Prepare-time clone is sufficient.

**(c) Cleanup on teardown.** `Stub.Close` removes the per-activation rootfs file (best effort, path-only logging). The husk pod is long-lived and owns one VM; when the pod terminates, `Close` reaps the Firecracker process and now also unlinks the clone, so the CoW file does not outlive the pod. The pod's `RestartPolicy: Always` plus a fresh `Prepare` on restart re-clones, so a leftover file from a crash is overwritten (O_TRUNC) rather than leaked.

**Threat-model delta:** Surface 3 (the read-only snapshot hostPath) in `docs/threat-model.md` currently states the residual "all husk pods on a node share the SAME read-only snapshot dir ... Cross-pod isolation of the mount is the read-only property, not a per-pod copy." The rootfs is part of the template dir and was previously mounted READ-WRITE and shared. This plan changes the isolation surface: each activation now writes to its OWN CoW clone, never the shared template rootfs. Task 7 updates Surface 3 to record that the rootfs is cloned per activation (no cross-activation write sharing) while the snapshot mem/vmstate stays read-only and shared.

---

## File Structure

- `internal/volume/backend.go` (modify): extract the reflink dance from `Snapshot` into an exported `ReflinkCopy(src, dst string) error`; `Snapshot` calls it. One place owns the FICLONE policy.
- `internal/volume/backend_test.go` (modify): add tests for `ReflinkCopy` (argv shape + fallback), darwin-safe (recording runner). Keep the existing `Snapshot` tests green.
- `internal/husk/stub.go` (modify): add `PatchDrive` to the `vmm` interface and `clientVMM`; add `RootfsCoW` fields to `Options` and `Stub`; clone in `Prepare`; rebind in `Activate`; unlink in `Close`. Add a `reflinker` seam so the clone is unit-testable on darwin.
- `internal/husk/stub_test.go` (modify): extend `fakeVMM` with `PatchDrive` recording; add tests that Prepare clones and Activate rebinds the `rootfs` drive, that a rebind failure fails closed, and that Close removes the clone.
- `internal/firecracker/client.go` (no change): `PatchDrive` already exists.
- `internal/controller/huskpod.go` (modify): mount a writable `<dataDir>/husk-rootfs` hostPath at its real path; pass `--rootfs-cow-dir` and `--template-rootfs` to the stub; replace the "PRODUCTION FOLLOW-UP" comment.
- `internal/controller/huskpod_test.go` (modify): assert the new volume/mount and args are present.
- `cmd/husk-stub/main.go` (modify): add `--rootfs-cow-dir` and `--template-rootfs` flags; wire `husk.Options.RootfsCoWDir` and `RootfsTemplatePath`.
- `docs/threat-model.md` (modify): update Surface 3 residual for per-activation rootfs CoW.
- `internal/volume/reflink_cow_test.go` (create): a KVM-CI / bare-metal reflink integration test (real `cp --reflink`), build-tagged so darwin and the default `make test-unit` skip it.

---

### Task 1: Extract the reflink policy into `volume.ReflinkCopy`

**Files:**
- Modify: `internal/volume/backend.go`
- Test: `internal/volume/backend_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/volume/backend_test.go`:

```go
func TestReflinkCopyTriesAlwaysThenAuto(t *testing.T) {
	b, rr := newTestBackend(t)
	rr.failOn = func(argv []string) bool {
		return strings.Contains(strings.Join(argv, " "), "--reflink=always")
	}
	src := filepath.Join(t.TempDir(), "src.ext4")
	dst := filepath.Join(t.TempDir(), "dst.ext4")

	if err := b.ReflinkCopy(src, dst); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}
	if len(rr.calls) != 2 {
		t.Fatalf("expected 2 cp calls (always then auto), got %d: %v", len(rr.calls), rr.calls)
	}
	if !strings.Contains(strings.Join(rr.calls[0], " "), "--reflink=always") {
		t.Errorf("first cp should try --reflink=always: %v", rr.calls[0])
	}
	if !strings.Contains(strings.Join(rr.calls[1], " "), "--reflink=auto") {
		t.Errorf("fallback should use --reflink=auto: %v", rr.calls[1])
	}
	if rr.calls[1][len(rr.calls[1])-2] != src || rr.calls[1][len(rr.calls[1])-1] != dst {
		t.Errorf("cp src/dst = %q %q, want %q %q", rr.calls[1][len(rr.calls[1])-2], rr.calls[1][len(rr.calls[1])-1], src, dst)
	}
}

func TestReflinkCopyMakesDestDir(t *testing.T) {
	b, _ := newTestBackend(t)
	src := filepath.Join(t.TempDir(), "src.ext4")
	dst := filepath.Join(t.TempDir(), "nested", "dst.ext4")
	if err := b.ReflinkCopy(src, dst); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dst)); err != nil {
		t.Errorf("dest parent dir not created: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/volume/ -run TestReflinkCopy -v`
Expected: FAIL, `b.ReflinkCopy undefined (type *Backend has no field or method ReflinkCopy)`.

- [ ] **Step 3: Write minimal implementation**

In `internal/volume/backend.go`, add the exported method and refactor `Snapshot` to call it. Insert `ReflinkCopy` immediately before `Snapshot`:

```go
// ReflinkCopy copies src to dst with copy-on-write semantics. It first tries
// cp --reflink=always for a true FICLONE clone (instant, shared extents on
// btrfs/xfs); on a filesystem without reflink support that fails, so it falls
// back to cp --reflink=auto, which performs a full byte copy and logs a warning
// (CoW was unavailable). The destination's parent directory is created if
// absent. src and dst carry no secrets and are safe to log. This is the single
// owner of the reflink policy: both per-fork volume Snapshot and the husk
// per-activation rootfs clone go through it so they cannot drift.
func (b *Backend) ReflinkCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("reflink copy: mkdir: %w", err)
	}
	if err := b.runner([]string{"cp", "--reflink=always", src, dst}); err != nil {
		fmt.Fprintf(os.Stderr, "volume: WARNING reflink CoW unavailable (filesystem lacks reflink support); falling back to a full copy of %s to %s\n", src, dst)
		if err := b.runner([]string{"cp", "--reflink=auto", src, dst}); err != nil {
			return fmt.Errorf("reflink copy %s to %s: %w", src, dst, err)
		}
	}
	return nil
}
```

Then replace the body of `Snapshot` between the `mkdir` and the `return Prepared{...}` with a `ReflinkCopy` call. The new `Snapshot`:

```go
func (b *Backend) Snapshot(spec Spec, sandboxID, sourcePath string) (Prepared, error) {
	if err := validateName(spec.Name); err != nil {
		return Prepared{}, err
	}
	dst := b.volumePath(sandboxID, spec.Name)
	if err := b.ReflinkCopy(sourcePath, dst); err != nil {
		return Prepared{}, fmt.Errorf("volume %s: %w", spec.Name, err)
	}
	return Prepared{Name: spec.Name, HostPath: dst, MountPath: spec.MountPath, ReadOnly: spec.ReadOnly}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/volume/ -v`
Expected: PASS, including the existing `TestSnapshotIssuesReflinkCopyToDistinctPath` and `TestSnapshotFallsBackToReflinkAuto` (they still see one or two `cp` calls; the argv shape is unchanged because `ReflinkCopy` issues the identical commands).

- [ ] **Step 5: Lint**

Run: `golangci-lint run --timeout=5m ./internal/volume/ && GOOS=linux golangci-lint run --timeout=5m ./internal/volume/`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add internal/volume/backend.go internal/volume/backend_test.go
git commit -m "refactor: extract reflink CoW policy into volume.ReflinkCopy

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Add `PatchDrive` to the husk `vmm` interface

**Files:**
- Modify: `internal/husk/stub.go`
- Test: `internal/husk/stub_test.go`

- [ ] **Step 1: Write the failing test**

Extend `fakeVMM` in `internal/husk/stub_test.go`. Add fields to the struct (after `closed bool`):

```go
	patchCalls []struct {
		driveID string
		path    string
	}
	patchErr error
```

And add the method after `func (f *fakeVMM) VsockHostPath`:

```go
func (f *fakeVMM) PatchDrive(driveID, pathOnHost string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patchCalls = append(f.patchCalls, struct {
		driveID string
		path    string
	}{driveID, pathOnHost})
	return f.patchErr
}
```

Add a compile-time interface assertion test at the end of `internal/husk/stub_test.go`:

```go
// TestFakeVMMSatisfiesInterface fails to compile if the vmm interface gains a
// method fakeVMM does not implement, keeping the fake in lockstep with the seam.
func TestFakeVMMSatisfiesInterface(t *testing.T) {
	var _ vmm = (*fakeVMM)(nil)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/husk/ -run TestFakeVMMSatisfiesInterface -v`
Expected: FAIL to compile, `*fakeVMM does not implement vmm (missing method PatchDrive)` is not yet the error; instead the interface still lacks `PatchDrive`, so the assertion compiles but is meaningless. To make the test meaningful, also add the interface method in Step 3 and re-run.

- [ ] **Step 3: Write minimal implementation**

In `internal/husk/stub.go`, add `PatchDrive` to the `vmm` interface (after `LoadSnapshotWithOverrides`):

```go
	// PatchDrive rebinds an existing baked drive (by drive id) to a host backing
	// file via PATCH /drives, after the snapshot is loaded and resumed. The husk
	// activate path uses it to point the rootfs drive at this activation's CoW
	// clone, the same rebind the fork engine applies to volume drives.
	PatchDrive(driveID, pathOnHost string) error
```

`clientVMM` embeds `*firecracker.Client`, which already has `PatchDrive(driveID, pathOnHost string) error`, so `clientVMM` satisfies the interface with no extra code.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/husk/ -run TestFakeVMMSatisfiesInterface -v`
Expected: PASS (and the whole `fakeVMM` now implements the wider interface).

- [ ] **Step 5: Commit**

```bash
git add internal/husk/stub.go internal/husk/stub_test.go
git commit -m "feat: add PatchDrive to the husk vmm interface

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Clone the rootfs at Prepare

**Files:**
- Modify: `internal/husk/stub.go`
- Test: `internal/husk/stub_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/husk/stub_test.go`:

```go
func TestPrepareClonesRootfsWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	tmplRootfs := filepath.Join(dir, "templates", "tmpl", "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(tmplRootfs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmplRootfs, []byte("ROOTFS"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowDir := filepath.Join(dir, "husk-rootfs")

	var gotSrc, gotDst string
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:  func(cfg firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil },
		Ready:  readyOK,
		Notify: (&fakeNotifier{}).notify,
		Verify: verifyOK,
		RootfsTemplatePath: tmplRootfs,
		RootfsCoWDir:       cowDir,
		Reflink: func(src, dst string) error {
			gotSrc, gotDst = src, dst
			return os.WriteFile(dst, []byte("CLONE"), 0o644)
		},
	})

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if gotSrc != tmplRootfs {
		t.Errorf("reflink src = %q, want template rootfs %q", gotSrc, tmplRootfs)
	}
	wantDst := filepath.Join(cowDir, "husk-test", "rootfs.ext4")
	if gotDst != wantDst {
		t.Errorf("reflink dst = %q, want per-activation path %q", gotDst, wantDst)
	}
	if _, err := os.Stat(wantDst); err != nil {
		t.Errorf("clone not written: %v", err)
	}
}

func TestPrepareSkipsCloneWhenUnconfigured(t *testing.T) {
	called := false
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:   func(cfg firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil },
		Ready:   readyOK,
		Notify:  (&fakeNotifier{}).notify,
		Verify:  verifyOK,
		Reflink: func(src, dst string) error { called = true; return nil },
	})
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if called {
		t.Error("reflink must not run when RootfsTemplatePath/RootfsCoWDir are empty")
	}
}

func TestPrepareCloneFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	tmplRootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(tmplRootfs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	vm := &fakeVMM{}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:              func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:              readyOK,
		Notify:             (&fakeNotifier{}).notify,
		Verify:             verifyOK,
		RootfsTemplatePath: tmplRootfs,
		RootfsCoWDir:       filepath.Join(dir, "husk-rootfs"),
		Reflink:            func(src, dst string) error { return errors.New("no space") },
	})
	if err := s.Prepare(context.Background()); err == nil {
		t.Fatal("expected Prepare to fail closed on clone error")
	}
	if s.State() == StateDormant {
		t.Error("state must not be dormant after a failed clone")
	}
	if !vm.closed {
		t.Error("the dormant VMM must be torn down when Prepare fails closed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/husk/ -run TestPrepareClonesRootfs -v`
Expected: FAIL to compile, `unknown field RootfsTemplatePath in struct literal of type husk.Options` (and `RootfsCoWDir`, `Reflink`).

- [ ] **Step 3: Write minimal implementation**

In `internal/husk/stub.go`, add a `reflinker` seam type near the other seams (after the `notifier` type):

```go
// reflinker copies a source file to a destination with copy-on-write semantics
// (reflink where the filesystem supports it, full copy otherwise). The husk
// stub clones the template rootfs to a per-activation file through it. The
// production seam is volume.Backend.ReflinkCopy; tests inject a fake. src and
// dst carry no secrets.
type reflinker func(src, dst string) error
```

Add to `Options` (after `PrepareExpectedDigest`):

```go
	// RootfsTemplatePath and RootfsCoWDir, when both set, give this activation its
	// OWN copy-on-write clone of the template rootfs instead of writing the shared
	// template rootfs.ext4 in place. At Prepare the stub reflink-clones
	// RootfsTemplatePath to <RootfsCoWDir>/<vm id>/rootfs.ext4 (pre-paid, dormant),
	// and at Activate it rebinds the snapshot's baked "rootfs" drive to that clone
	// with PatchDrive after the snapshot loads. Both empty keeps the prior behavior
	// (the resumed VM writes the shared template rootfs). The paths are content
	// addresses, not secrets.
	RootfsTemplatePath string
	RootfsCoWDir       string
	// Reflink performs the per-activation rootfs clone. Nil uses the production
	// seam (volume.Backend.ReflinkCopy, which is FICLONE with a full-copy
	// fallback). Tests inject a fake.
	Reflink reflinker
```

Add to `Stub` (after `prepareExpectedDigest`):

```go
	// rootfsTemplatePath / rootfsCoWDir configure the per-activation rootfs CoW;
	// reflink performs the clone; rootfsClonePath records the clone Prepare made so
	// Activate rebinds the drive to it and Close removes it. Empty rootfsClonePath
	// means no per-activation rootfs was prepared (prior behavior).
	rootfsTemplatePath string
	rootfsCoWDir       string
	reflink            reflinker
	rootfsClonePath    string
```

In `New`, wire the options (after the `prepareExpectedDigest` assignment in the struct literal):

```go
		rootfsTemplatePath: opts.RootfsTemplatePath,
		rootfsCoWDir:       opts.RootfsCoWDir,
		reflink:            opts.Reflink,
```

and add a default after the `s.readyTimeout` default block:

```go
	if s.reflink == nil {
		s.reflink = volume.New("").ReflinkCopy
	}
```

Add the import `"github.com/paperclipinc/mitos/internal/volume"` to the import block. `volume.New("")` is only a holder for the `ReflinkCopy` method (which uses absolute src/dst and never touches the backend root), so the empty root is intentional and harmless.

In `Prepare`, after the `prepareVerified` block and before `s.state = StateDormant`, add the clone:

```go
	// Per-activation rootfs CoW (opt-in): clone the template rootfs to this
	// activation's OWN file NOW, during the dormant pre-paid window, so the
	// Activate hot path is only load + handshake (the clone, especially a
	// full-copy fallback on a non-reflink filesystem, must never land on the hot
	// path). The clone source is read-only and content-addressed, so a clone taken
	// here is byte-identical to one taken at Activate. Fail closed: a clone failure
	// tears the dormant VMM down and keeps the pod out of StateDormant so the pool
	// never offers it.
	if s.rootfsTemplatePath != "" && s.rootfsCoWDir != "" {
		clonePath := filepath.Join(s.rootfsCoWDir, s.cfg.ID, "rootfs.ext4")
		if err := s.reflink(s.rootfsTemplatePath, clonePath); err != nil {
			_ = s.vm.Close()
			s.vm = nil
			return fmt.Errorf("husk: clone per-activation rootfs: %w", err)
		}
		s.rootfsClonePath = clonePath
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/husk/ -run TestPrepare -v`
Expected: PASS for the three new tests and the existing Prepare tests.

- [ ] **Step 5: Lint**

Run: `golangci-lint run --timeout=5m ./internal/husk/ && GOOS=linux golangci-lint run --timeout=5m ./internal/husk/`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add internal/husk/stub.go internal/husk/stub_test.go
git commit -m "feat: clone per-activation rootfs at husk Prepare

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Rebind the rootfs drive at Activate

**Files:**
- Modify: `internal/husk/stub.go`
- Test: `internal/husk/stub_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/husk/stub_test.go`. These drive the full activate path with a clone configured; they assert the `rootfs` drive is PATCHed to the clone AFTER load:

```go
func TestActivateRebindsRootfsDriveToClone(t *testing.T) {
	dir := t.TempDir()
	tmplRootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(tmplRootfs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowDir := filepath.Join(dir, "husk-rootfs")
	vm := &fakeVMM{}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:              func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:              readyOK,
		Notify:             (&fakeNotifier{}).notify,
		Verify:             verifyOK,
		RootfsTemplatePath: tmplRootfs,
		RootfsCoWDir:       cowDir,
		Reflink:            func(src, dst string) error { return os.WriteFile(dst, []byte("c"), 0o644) },
	})
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: filepath.Join(dir, "snap")})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate not OK: %s", res.Error)
	}
	if len(vm.patchCalls) != 1 {
		t.Fatalf("expected exactly 1 PatchDrive call, got %d: %v", len(vm.patchCalls), vm.patchCalls)
	}
	if vm.patchCalls[0].driveID != "rootfs" {
		t.Errorf("rebind drive id = %q, want \"rootfs\"", vm.patchCalls[0].driveID)
	}
	wantPath := filepath.Join(cowDir, "husk-test", "rootfs.ext4")
	if vm.patchCalls[0].path != wantPath {
		t.Errorf("rebind path = %q, want clone %q", vm.patchCalls[0].path, wantPath)
	}
}

func TestActivateNoRebindWhenNoClone(t *testing.T) {
	vm := &fakeVMM{}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate not OK: %s", res.Error)
	}
	if len(vm.patchCalls) != 0 {
		t.Errorf("no rootfs clone configured, so no PatchDrive expected, got %v", vm.patchCalls)
	}
}

func TestActivateRebindFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	tmplRootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(tmplRootfs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	vm := &fakeVMM{patchErr: errors.New("drive busy")}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:              func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:              readyOK,
		Notify:             (&fakeNotifier{}).notify,
		Verify:             verifyOK,
		RootfsTemplatePath: tmplRootfs,
		RootfsCoWDir:       filepath.Join(dir, "husk-rootfs"),
		Reflink:            func(src, dst string) error { return os.WriteFile(dst, []byte("c"), 0o644) },
	})
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: filepath.Join(dir, "snap")})
	if err == nil {
		t.Fatal("expected activate to fail closed on rebind error")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK")
	}
	if s.State() == StateActive {
		t.Errorf("state must not be active after a failed rebind, got %s", s.State())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/husk/ -run TestActivateRebindsRootfsDriveToClone -v`
Expected: FAIL, `expected exactly 1 PatchDrive call, got 0` (Activate does not yet rebind).

- [ ] **Step 3: Write minimal implementation**

In `internal/husk/stub.go` `Activate`, insert the rebind immediately AFTER the `LoadSnapshotWithOverrides` block and BEFORE the `vsockPath := s.vm.VsockHostPath(...)` line:

```go
	// Rebind the baked "rootfs" drive to THIS activation's CoW clone now that the
	// snapshot is loaded and resumed, before the guest writes anything. This is the
	// husk analog of the fork engine's per-fork volume drive rebind: the snapshot
	// bakes the rootfs block device at path_on_host, and Firecracker supports
	// updating a drive's path_on_host on a restored+resumed VM (PATCH /drives).
	// Skipped when no per-activation clone was prepared (the prior shared-rootfs
	// behavior). Fail closed: a rebind failure means the VM is still pointed at the
	// shared template rootfs, which is exactly the corruption hazard this prevents,
	// so do NOT mark active. The drive id and path carry no secrets.
	if s.rootfsClonePath != "" {
		if err := s.vm.PatchDrive("rootfs", s.rootfsClonePath); err != nil {
			werr := fmt.Errorf("husk: rebind rootfs drive to per-activation clone: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/husk/ -run TestActivate -v`
Expected: PASS for the three new rebind tests and all existing Activate tests (the non-clone tests use `newTestStub`, whose `rootfsClonePath` stays empty, so they make zero PatchDrive calls).

- [ ] **Step 5: Lint**

Run: `golangci-lint run --timeout=5m ./internal/husk/ && GOOS=linux golangci-lint run --timeout=5m ./internal/husk/`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add internal/husk/stub.go internal/husk/stub_test.go
git commit -m "feat: rebind rootfs drive to per-activation clone at husk Activate

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Remove the per-activation clone on teardown

**Files:**
- Modify: `internal/husk/stub.go`
- Test: `internal/husk/stub_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/husk/stub_test.go`:

```go
func TestCloseRemovesRootfsClone(t *testing.T) {
	dir := t.TempDir()
	tmplRootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(tmplRootfs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowDir := filepath.Join(dir, "husk-rootfs")
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:              func(cfg firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil },
		Ready:              readyOK,
		Notify:             (&fakeNotifier{}).notify,
		Verify:             verifyOK,
		RootfsTemplatePath: tmplRootfs,
		RootfsCoWDir:       cowDir,
		Reflink:            func(src, dst string) error { return os.WriteFile(dst, []byte("c"), 0o644) },
	})
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	clonePath := filepath.Join(cowDir, "husk-test", "rootfs.ext4")
	if _, err := os.Stat(clonePath); err != nil {
		t.Fatalf("clone should exist after Prepare: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
		t.Errorf("clone should be removed after Close, stat err = %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/husk/ -run TestCloseRemovesRootfsClone -v`
Expected: FAIL, `clone should be removed after Close` (Close does not yet unlink it).

- [ ] **Step 3: Write minimal implementation**

In `internal/husk/stub.go` `Close`, after the `s.vm = nil` / `s.state = StateNew` assignments and before `return err`, add:

```go
	// Best effort: remove this activation's rootfs CoW clone so it does not
	// outlive the pod. A reflink clone shares extents with the template until
	// written, so removing it frees only the activation's own divergent blocks.
	// Path only is logged on failure; the clone carries no secrets.
	if s.rootfsClonePath != "" {
		if rmErr := os.Remove(s.rootfsClonePath); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Fprintf(os.Stderr, "husk: remove per-activation rootfs clone %s: %v\n", s.rootfsClonePath, rmErr)
		}
		s.rootfsClonePath = ""
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/husk/ -v`
Expected: PASS for the new test and the whole husk suite.

- [ ] **Step 5: Lint**

Run: `golangci-lint run --timeout=5m ./internal/husk/ && GOOS=linux golangci-lint run --timeout=5m ./internal/husk/`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add internal/husk/stub.go internal/husk/stub_test.go
git commit -m "feat: remove per-activation rootfs clone on husk teardown

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Wire `--rootfs-cow-dir` and `--template-rootfs` flags in the stub binary

**Files:**
- Modify: `cmd/husk-stub/main.go`

- [ ] **Step 1: Write the failing check (manual, no unit test for flag parsing)**

`cmd/husk-stub/main.go` has no unit tests; flag wiring is verified by build plus the controller test in Task 7 asserting the args. The verification command for this task is the build.

- [ ] **Step 2: Confirm it does not yet build with the new flags**

Run: `grep -c "rootfs-cow-dir" cmd/husk-stub/main.go`
Expected: `0` (flag absent).

- [ ] **Step 3: Write the implementation**

In `cmd/husk-stub/main.go` `run()`, add two flags inside the `var ( ... )` block alongside `manifest` and `allowUnverified`:

```go
		rootfsCoWDir    = flag.String("rootfs-cow-dir", "", "directory on the SAME node filesystem as the template rootfs where this activation's copy-on-write rootfs clone is written (reflink where supported, full copy otherwise). Empty keeps the prior behavior of writing the shared template rootfs in place. A content address, not a secret")
		templateRootfs  = flag.String("template-rootfs", "", "host path of the template rootfs.ext4 to clone per activation. Empty (with --rootfs-cow-dir) disables the per-activation clone")
```

In the `husk.New(cfg, husk.Options{ ... })` literal, add (after `PrepareExpectedDigest`):

```go
			// Per-activation rootfs CoW: clone the template rootfs to a per-pod file
			// on a writable co-located volume at Prepare and rebind the rootfs drive
			// to it at Activate, so concurrent activations of one template never
			// share or corrupt a single rootfs. Both empty keeps the prior in-place
			// shared-rootfs behavior.
			RootfsTemplatePath: *templateRootfs,
			RootfsCoWDir:       *rootfsCoWDir,
```

- [ ] **Step 4: Run build to verify it compiles**

Run: `go build ./cmd/husk-stub/ && GOOS=linux GOARCH=amd64 go build ./cmd/husk-stub/`
Expected: both succeed (no output).

- [ ] **Step 5: Lint**

Run: `golangci-lint run --timeout=5m ./cmd/husk-stub/ && GOOS=linux golangci-lint run --timeout=5m ./cmd/husk-stub/`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add cmd/husk-stub/main.go
git commit -m "feat: add --rootfs-cow-dir and --template-rootfs flags to husk-stub

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Mount the writable CoW dir and pass the flags from the controller

**Files:**
- Modify: `internal/controller/huskpod.go`
- Test: `internal/controller/huskpod_test.go`

- [ ] **Step 1: Write the failing test**

First read `internal/controller/huskpod_test.go` to confirm the helper and option shape (the existing tests call `r.BuildHuskPodForTest(pool, template, controller.HuskPodOptions{...})` with `DataDir` and `SnapshotID` set; see `TestBuildHuskPodControlAndSnapshot`, which uses `DataDir: "/var/lib/mitos"` and `SnapshotID: "ctl-tmpl"`). Add this test, mirroring that setup verbatim so the pool/template/reconciler are constructed the same way:

```go
func TestBuildHuskPodMountsWritableRootfsCoWDir(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "cow-pool", Namespace: "default", UID: "pool-uid-cow"},
		Spec:       v1alpha1.SandboxPoolSpec{TemplateRef: v1alpha1.LocalObjectReference{Name: "cow-tmpl"}, Replicas: 1},
	}
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "cow-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, template, controller.HuskPodOptions{
		StubImage:  "mitos-husk-stub:test",
		SnapshotID: "cow-tmpl",
		DataDir:    "/var/lib/mitos",
	})
	container := pod.Spec.Containers[0]

	// The CoW dir hostPath volume must be present and WRITABLE (ReadOnly false),
	// co-located under the node data dir as a sibling of templates.
	var cowMount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == "husk-rootfs-cow" {
			cowMount = &container.VolumeMounts[i]
		}
	}
	if cowMount == nil {
		t.Fatal("expected a husk-rootfs-cow volume mount")
	}
	if cowMount.ReadOnly {
		t.Error("the rootfs CoW dir must be mounted read-write")
	}

	var cowVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "husk-rootfs-cow" {
			cowVol = &pod.Spec.Volumes[i]
		}
	}
	if cowVol == nil || cowVol.HostPath == nil {
		t.Fatal("expected a husk-rootfs-cow hostPath volume")
	}
	wantHostPath := filepath.Join("/var/lib/mitos", "husk-rootfs")
	if cowVol.HostPath.Path != wantHostPath {
		t.Errorf("CoW hostPath = %q, want %q (sibling of templates under the data dir)", cowVol.HostPath.Path, wantHostPath)
	}

	// The stub must be told where to clone from and to.
	args := strings.Join(container.Args, " ")
	if !strings.Contains(args, "--rootfs-cow-dir "+cowMount.MountPath) {
		t.Errorf("args missing --rootfs-cow-dir %s: %v", cowMount.MountPath, container.Args)
	}
	wantTemplateRootfs := filepath.Join("/var/lib/mitos", "templates", "cow-tmpl", "rootfs.ext4")
	if !strings.Contains(args, "--template-rootfs "+wantTemplateRootfs) {
		t.Errorf("args missing --template-rootfs %s: %v", wantTemplateRootfs, container.Args)
	}
}
```

`v1alpha1`, `metav1`, `corev1`, `strings`, `filepath`, `controller`, and `k8sClient` are all already imported / available in this test file (the existing tests use every one). If `filepath` or `strings` is not yet imported, add it to the import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestHuskPodMountsWritableRootfsCoWDir -v`
Expected: FAIL, `expected a husk-rootfs-cow volume mount` (no such volume yet).

- [ ] **Step 3: Write the implementation**

In `internal/controller/huskpod.go`, add a mount-path constant in the `const` block near `huskSnapshotMountPath` (line ~83):

```go
	// huskRootfsCoWMountPath is the in-pod path the writable per-activation rootfs
	// CoW directory is mounted at. It is a hostPath under the node data dir
	// (<dataDir>/husk-rootfs), co-located with the template dir on the SAME node
	// filesystem so the stub's reflink clone of the template rootfs lands on a
	// reflink-capable filesystem (a full copy fallback otherwise). Each activation
	// writes its own clone under here, never the shared read-only template rootfs.
	huskRootfsCoWMountPath = "/var/lib/mitos/husk-rootfs"
```

Then, replace the "PRODUCTION FOLLOW-UP (per-activation rootfs CoW)" comment block (lines ~374-382) above the `template` volume with a comment recording that the rootfs is now cloned per activation:

```go
			// The template directory, mounted at the SAME absolute path the snapshot
			// was built at (<dataDir>/templates/<id>), so the rootfs.ext4 drive the
			// snapshot's vmstate references resolves on load. Firecracker re-opens the
			// drive at its baked path_on_host during /snapshot/load. The stub then
			// rebinds the rootfs drive to a PER-ACTIVATION copy-on-write clone (see
			// the husk-rootfs-cow mount below) immediately after load, so the resumed
			// VM writes its OWN rootfs, never the shared template rootfs: concurrent
			// activations of one template no longer share or corrupt a single rootfs.
			// The clone source (this template rootfs) stays effectively read-only.
```

After the `template` volume + mount append (the block ending at line ~393), add the writable CoW dir volume and mount:

```go
			// The writable per-activation rootfs CoW directory, a sibling of the
			// template dir under the node data dir so the stub's reflink clone of the
			// template rootfs stays on ONE reflink-capable filesystem. Mounted
			// READ-WRITE (unlike the snapshot and template mounts) because the stub
			// writes this activation's clone here; an emptyDir would land on a
			// different filesystem and defeat reflink. DirectoryOrCreate so the dir is
			// created on first use.
			volumes = append(volumes, corev1.Volume{
				Name: "husk-rootfs-cow",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: filepath.Join(dataDir, "husk-rootfs"),
						Type: &hostType,
					},
				},
			})
			mounts = append(mounts, corev1.VolumeMount{Name: "husk-rootfs-cow", MountPath: huskRootfsCoWMountPath})
```

Finally, pass the two flags to the stub. Find where the dormant-stub `args` are assembled (the `args = append(args, "--snapshot-dir", huskSnapshotMountPath, "--expected-digest", opts.ExpectedDigest)` line ~275 is inside the `opts.SnapshotID != ""` / digest branch). Add, in the same `opts.SnapshotID != ""` block where the template mount is set (so it is only passed when a snapshot/template is present):

```go
			args = append(args,
				"--rootfs-cow-dir", huskRootfsCoWMountPath,
				"--template-rootfs", filepath.Join(templateDir, "rootfs.ext4"),
			)
```

Note `templateDir` is the in-pod path variable already declared at line ~383 (`filepath.Join(dataDir, "templates", opts.SnapshotID)`), mounted at the SAME absolute path, so `filepath.Join(templateDir, "rootfs.ext4")` is the in-pod path of the template rootfs the stub clones FROM. Place this append AFTER `templateDir` is declared and the template mount is appended.

- [ ] **Step 4: Run tests to verify they pass**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestHuskPod -v`
Expected: PASS for the new test and the existing husk pod tests.

- [ ] **Step 5: Lint**

Run: `golangci-lint run --timeout=5m ./internal/controller/ && GOOS=linux golangci-lint run --timeout=5m ./internal/controller/`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/huskpod.go internal/controller/huskpod_test.go
git commit -m "feat: mount writable rootfs CoW dir and pass clone flags to husk pod

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: Update the threat model for the isolation-surface change

**Files:**
- Modify: `docs/threat-model.md`

- [ ] **Step 1: Locate the residual paragraph**

Read `docs/threat-model.md` Surface 3 (around lines 140-152). The current residual ends: "Cross-pod isolation of the mount is the read-only property, not a per-pod copy; fully pod-native snapshot delivery (a CAS pull into the pod, removing the shared hostPath) is a documented follow-up."

- [ ] **Step 2: Edit the residual to record the per-activation rootfs CoW**

Replace the final sentence of the Surface 3 residual paragraph (the "Cross-pod isolation of the mount ..." sentence) with:

```
Cross-pod isolation of the snapshot mem/vmstate is the read-only property, not a
per-pod copy. The ROOTFS, by contrast, is no longer shared read-write: each
activation gets its OWN copy-on-write clone of the template rootfs
(`internal/husk` `Stub.Prepare` reflink-clones `<dataDir>/templates/<id>/rootfs.ext4`
to a per-pod file under the writable `<dataDir>/husk-rootfs` hostPath, and
`Stub.Activate` rebinds the snapshot's baked `rootfs` drive to that clone with
`PatchDrive` after load), so concurrent activations of one template never write
through to a single shared rootfs or leak one tenant's filesystem state into
another. This closes the prior residual where the template dir (including the
rootfs) was mounted read-write and shared. The clone is removed on pod teardown
(`Stub.Close`). Fully pod-native snapshot delivery (a CAS pull into the pod,
removing the shared read-only mem/vmstate hostPath) remains a documented follow-up.
```

- [ ] **Step 3: Verify no em/en dashes were introduced**

Run: `grep -nP "[\x{2013}\x{2014}]" docs/threat-model.md`
Expected: no output (exit status 1).

- [ ] **Step 4: Commit**

```bash
git add docs/threat-model.md
git commit -m "docs: update threat model for per-activation rootfs CoW isolation

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: Add the reflink integration test (KVM-CI / bare-metal only)

**Files:**
- Create: `internal/volume/reflink_cow_test.go`

This test exercises a REAL `cp --reflink` against the real filesystem, so it cannot run on the darwin dev box (`cp` has no `--reflink` on macOS) nor reliably under the default unit suite (a CI runner's tmpfs/overlayfs may lack reflink). It is build-tagged `reflink_integration` so `make test-unit` and the darwin lint skip it; it runs on the KVM CI runner (kvm-test.yaml) and bare-metal nodes whose data dir is XFS/Btrfs.

- [ ] **Step 1: Write the test**

Create `internal/volume/reflink_cow_test.go`:

```go
//go:build reflink_integration

package volume

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestReflinkCopyRealFilesystem exercises ReflinkCopy against a real cp on the
// host filesystem: the destination must be a byte-identical copy of the source.
// It runs ONLY under the reflink_integration build tag (KVM CI / bare-metal),
// because cp --reflink does not exist on darwin and the destination filesystem
// must support FICLONE (XFS/Btrfs) for the fast path; the production
// ReflinkCopy falls back to a full copy when reflink is unavailable, so the
// byte-equality assertion holds either way.
func TestReflinkCopyRealFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	dst := filepath.Join(dir, "sub", "dst.ext4")
	want := bytes.Repeat([]byte("PAPERCLIP"), 4096) // ~36 KiB of recognizable bytes
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	b := New(dir) // production runner (real cp)
	if err := b.ReflinkCopy(src, dst); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("clone is not byte-identical to source: got %d bytes, want %d", len(got), len(want))
	}
}
```

- [ ] **Step 2: Confirm it is skipped by the default suite (darwin)**

Run: `go test ./internal/volume/ -run TestReflinkCopyRealFilesystem -v`
Expected: `testing: warning: no tests to run` / `ok ... [no tests to run]` (the build tag excludes the file).

- [ ] **Step 3: Run it under the build tag (KVM CI / bare-metal node only)**

On a Linux node whose temp dir filesystem has `cp --reflink` available:

Run: `go test -tags reflink_integration ./internal/volume/ -run TestReflinkCopyRealFilesystem -v`
Expected: `--- PASS: TestReflinkCopyRealFilesystem` then `ok github.com/paperclipinc/mitos/internal/volume`. On a non-reflink filesystem the production `ReflinkCopy` logs the `WARNING reflink CoW unavailable` line and still PASSES via the full-copy fallback (byte-equality holds).

- [ ] **Step 4: Lint (linux only; the tag is linux-relevant)**

Run: `GOOS=linux golangci-lint run --timeout=5m --build-tags reflink_integration ./internal/volume/`
Expected: no findings.

- [ ] **Step 5: Commit**

```bash
git add internal/volume/reflink_cow_test.go
git commit -m "test: add reflink CoW integration test for bare-metal/KVM CI

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 6: Wire the tag into the KVM CI job (manual, optional follow-up)**

`.github/workflows/kvm-test.yaml` is the KVM runner job. To run this integration test there, add a step that runs `go test -tags reflink_integration ./internal/volume/ -run TestReflinkCopyRealFilesystem -v` on a workdir under an XFS/Btrfs mount. This is documented here as the verification location; wiring it is a one-line job step and can land in this PR or a CI follow-up, but the test itself is committed above so a bare-metal operator can run it directly.

---

## Final Verification (run before opening the PR)

- [ ] **Full unit + controller suite**

Run: `make test-unit && eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
Expected: all PASS.

- [ ] **Cross-build the guest agent and the stub for linux**

Run: `GOOS=linux GOARCH=amd64 go build ./guest/agent/ && GOOS=linux GOARCH=amd64 go build ./cmd/husk-stub/`
Expected: both succeed.

- [ ] **Lint both targets**

Run: `golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m`
Expected: no findings.

- [ ] **No em/en dashes anywhere in the diff**

Run: `git diff main --unified=0 | grep -nP "^\+.*[\x{2013}\x{2014}]"`
Expected: no output.

---

## Self-Review Notes

- **Spec coverage:** reflink reuse (Task 1, reusing `volume.Backend` policy), per-fork-equivalent rebind (Tasks 2+4, `PatchDrive("rootfs", clone)`), clone location and co-location (Task 7, `<dataDir>/husk-rootfs` sibling of templates, writable hostPath), Prepare vs Activate decision (Task 3, clone at Prepare), cleanup (Task 5, `Stub.Close`), threat-model delta (Task 8), KVM-CI/bare-metal reflink verification (Task 9). All decisions (a), (b), (c) from the prompt are implemented and justified.
- **Type consistency:** the clone path `filepath.Join(<cowDir>, <vm id>, "rootfs.ext4")` is identical in Tasks 3, 4, 5, and the controller test (Task 7). The drive id `"rootfs"` matches `internal/firecracker/template.go:242` `AddDrive("rootfs", ...)`. `Options.RootfsTemplatePath`, `Options.RootfsCoWDir`, `Options.Reflink`, and the `Stub` fields `rootfsTemplatePath`, `rootfsCoWDir`, `reflink`, `rootfsClonePath` are named consistently across all tasks. `vmm.PatchDrive(driveID, pathOnHost string) error` matches `*firecracker.Client.PatchDrive` exactly so `clientVMM` satisfies it with no shim.
- **No placeholders:** every code step shows the full Go and the exact run command with expected output.
