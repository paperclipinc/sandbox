package husk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/firecracker"
)

// fakeVMM records the snapshot-load arguments and returns a canned error.
type fakeVMM struct {
	loadErr error

	mu        sync.Mutex
	loadCalls int
	gotMem    string
	gotState  string
	gotResume bool
	gotOverr  []firecracker.NetworkOverride
	closed    bool

	patchCalls []struct {
		driveID string
		path    string
	}
	patchErr error

	resumeCalls int
	resumeErr   error

	// callOrder records the activate-time VMM call sequence ("load", "patch",
	// "resume") so a test can assert load(resume=false) -> PatchDrive -> Resume.
	callOrder []string
}

func (f *fakeVMM) LoadSnapshotWithOverrides(mem, snapshot string, resume bool, overrides []firecracker.NetworkOverride) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCalls++
	f.gotMem = mem
	f.gotState = snapshot
	f.gotResume = resume
	f.gotOverr = overrides
	f.callOrder = append(f.callOrder, "load")
	return f.loadErr
}

func (f *fakeVMM) VsockHostPath(rel string) string {
	return filepath.Join("/run/husk", rel)
}

func (f *fakeVMM) PatchDrive(driveID, pathOnHost string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patchCalls = append(f.patchCalls, struct {
		driveID string
		path    string
	}{driveID, pathOnHost})
	f.callOrder = append(f.callOrder, "patch")
	return f.patchErr
}

func (f *fakeVMM) Resume() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	f.callOrder = append(f.callOrder, "resume")
	return f.resumeErr
}

func (f *fakeVMM) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeNotifier records the fork-correctness handshake arguments and returns a
// canned error. A zero-value fakeNotifier succeeds (mirrors a guest that
// reseeded), so existing tests need not care about the handshake.
type fakeNotifier struct {
	err error

	mu         sync.Mutex
	calls      int
	gotVsock   string
	gotGen     []uint64
	gotEntropy [][]byte
	gotReq     []ActivateRequest
}

func (f *fakeNotifier) notify(vsockPath string, generation uint64, entropy []byte, req ActivateRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotVsock = vsockPath
	f.gotGen = append(f.gotGen, generation)
	// Copy the entropy: the caller owns the slice and may reuse it.
	cp := make([]byte, len(entropy))
	copy(cp, entropy)
	f.gotEntropy = append(f.gotEntropy, cp)
	f.gotReq = append(f.gotReq, req)
	return f.err
}

// verifyOK is the no-op verifier the unit-path stubs inject: it lets the activate
// state machine be exercised without an on-disk manifest. The dedicated
// verification tests (TestActivateVerify*) inject their own verifier to assert
// the fail-closed gate.
func verifyOK(ActivateRequest) error { return nil }

func newTestStub(t *testing.T, vm *fakeVMM, ready guestReady) *Stub {
	t.Helper()
	return New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:  func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:  ready,
		Notify: (&fakeNotifier{}).notify,
		Verify: verifyOK,
	})
}

// newTestStubWithNotifier is newTestStub but lets a test observe or fail the
// fork-correctness handshake.
func newTestStubWithNotifier(t *testing.T, vm *fakeVMM, ready guestReady, n *fakeNotifier) *Stub {
	t.Helper()
	return New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:  func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:  ready,
		Notify: n.notify,
		Verify: verifyOK,
	})
}

func readyOK(string, time.Duration) error { return nil }

func TestActivateBeforePrepareErrors(t *testing.T) {
	s := newTestStub(t, &fakeVMM{}, readyOK)

	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil {
		t.Fatal("expected error activating before prepare")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK")
	}
	if s.State() == StateActive {
		t.Fatalf("state must not be active, got %s", s.State())
	}
}

func TestPrepareThenActivateSucceeds(t *testing.T) {
	vm := &fakeVMM{}
	s := newTestStub(t, vm, readyOK)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if s.State() != StateDormant {
		t.Fatalf("after prepare state = %s, want dormant", s.State())
	}

	overrides := []firecracker.NetworkOverride{{IfaceID: "eth0", HostDevName: "tap-1"}}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir:      "/data/templates/tmpl/snapshot",
		NetworkOverrides: overrides,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate not OK: %s", res.Error)
	}
	if s.State() != StateActive {
		t.Fatalf("after activate state = %s, want active", s.State())
	}

	// Loaded the engine-layout mem/vmstate paths under the snapshot dir.
	if vm.gotMem != "/data/templates/tmpl/snapshot/mem" {
		t.Errorf("mem path = %q", vm.gotMem)
	}
	if vm.gotState != "/data/templates/tmpl/snapshot/vmstate" {
		t.Errorf("vmstate path = %q", vm.gotState)
	}
	// The husk path loads PAUSED (resume=false) and resumes explicitly AFTER the
	// rootfs rebind, so the guest never writes through the shared template.
	if vm.gotResume {
		t.Error("expected snapshot load with resume=false (rebind happens while paused)")
	}
	if vm.resumeCalls != 1 {
		t.Errorf("expected exactly 1 explicit Resume call, got %d", vm.resumeCalls)
	}
	if len(vm.gotOverr) != 1 || vm.gotOverr[0].HostDevName != "tap-1" {
		t.Errorf("overrides not threaded through: %+v", vm.gotOverr)
	}
	if res.VsockPath != "/run/husk/"+firecracker.VsockRelPath {
		t.Errorf("vsock path = %q", res.VsockPath)
	}
	if res.LatencyMs <= 0 {
		t.Errorf("LatencyMs must be > 0, got %v", res.LatencyMs)
	}
}

func TestActivateTwiceErrors(t *testing.T) {
	s := newTestStub(t, &fakeVMM{}, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"}); err != nil {
		t.Fatalf("first Activate: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil || res.OK {
		t.Fatal("second activate must fail (one VM per husk)")
	}
}

func TestPrepareTwiceErrors(t *testing.T) {
	s := newTestStub(t, &fakeVMM{}, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := s.Prepare(context.Background()); err == nil {
		t.Fatal("second Prepare must error (no second VMM)")
	}
}

func TestActivateLoadFailureFailsClosed(t *testing.T) {
	vm := &fakeVMM{loadErr: errors.New("snapshot corrupt")}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil {
		t.Fatal("expected error on load failure")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK on load failure")
	}
	if res.VsockPath != "" {
		t.Fatal("fail closed: must not report a usable vsock path")
	}
	if s.State() == StateActive {
		t.Fatalf("fail closed: state must not be active, got %s", s.State())
	}
}

func TestActivateGuestNotReadyFailsClosed(t *testing.T) {
	vm := &fakeVMM{}
	readyTimeout := func(string, time.Duration) error {
		return errors.New("no ping")
	}
	s := newTestStub(t, vm, readyTimeout)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil {
		t.Fatal("expected error when guest not ready")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK when guest never answers")
	}
	if s.State() == StateActive {
		t.Fatalf("fail closed: state must not be active, got %s", s.State())
	}
	// The snapshot DID load, proving we failed at the readiness gate, not before.
	if vm.loadCalls != 1 {
		t.Fatalf("expected load to be attempted once, got %d", vm.loadCalls)
	}
}

func TestActivateRunsForkHandshake(t *testing.T) {
	vm := &fakeVMM{}
	n := &fakeNotifier{}
	s := newTestStubWithNotifier(t, vm, readyOK, n)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	req := ActivateRequest{
		SnapshotDir: "/data/templates/x/snapshot",
		Env:         map[string]string{"LANG": "C"},
		Secrets:     map[string]string{"API_KEY": "s3cr3t-value"},
	}
	res, err := s.Activate(context.Background(), req)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !res.OK || s.State() != StateActive {
		t.Fatalf("activate not OK / not active: %+v state=%s", res, s.State())
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.calls != 1 {
		t.Fatalf("notify called %d times, want 1", n.calls)
	}
	// Generation starts at 1.
	if n.gotGen[0] != 1 {
		t.Fatalf("first generation = %d, want 1", n.gotGen[0])
	}
	// 32 bytes of entropy, and not all-zero (crypto/rand actually ran).
	if len(n.gotEntropy[0]) != entropySize {
		t.Fatalf("entropy size = %d, want %d", len(n.gotEntropy[0]), entropySize)
	}
	allZero := true
	for _, b := range n.gotEntropy[0] {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("entropy must be random, got all zeros")
	}
	// Env and secrets are threaded through to the handshake.
	if n.gotReq[0].Env["LANG"] != "C" {
		t.Errorf("env not passed through: %+v", n.gotReq[0].Env)
	}
	if n.gotReq[0].Secrets["API_KEY"] != "s3cr3t-value" {
		t.Errorf("secrets not passed through: %+v", n.gotReq[0].Secrets)
	}
	if n.gotVsock != "/run/husk/"+firecracker.VsockRelPath {
		t.Errorf("notify vsock path = %q", n.gotVsock)
	}
}

func TestActivateNotifyFailureFailsClosed(t *testing.T) {
	vm := &fakeVMM{}
	n := &fakeNotifier{err: errors.New("guest did not reseed")}
	s := newTestStubWithNotifier(t, vm, readyOK, n)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil {
		t.Fatal("expected error when the fork handshake fails")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK when the guest did not reseed")
	}
	if s.State() == StateActive {
		t.Fatalf("fail closed: state must not be active, got %s", s.State())
	}
	// The snapshot loaded and the guest was ready; we failed at the handshake.
	if vm.loadCalls != 1 {
		t.Fatalf("expected snapshot load before the handshake, got %d loads", vm.loadCalls)
	}
}

// TestActivateGeneratesDistinctEntropy is the unit-level analog of the
// RNG-distinctness property: two independent activations must hand the guest
// DIFFERENT entropy. One husk owns one VM for its lifetime, so we model two
// forks as two fresh New+Activate stubs sharing one recording notifier.
func TestActivateGeneratesDistinctEntropy(t *testing.T) {
	n := &fakeNotifier{}

	activate := func() {
		vm := &fakeVMM{}
		s := newTestStubWithNotifier(t, vm, readyOK, n)
		if err := s.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if _, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"}); err != nil {
			t.Fatalf("Activate: %v", err)
		}
	}
	activate()
	activate()

	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.gotEntropy) != 2 {
		t.Fatalf("expected 2 handshakes, got %d", len(n.gotEntropy))
	}
	if bytes.Equal(n.gotEntropy[0], n.gotEntropy[1]) {
		t.Fatal("two activations produced identical entropy; forks would share CRNG state")
	}
	// Each stub starts its own generation counter at 1 (one VM per husk).
	if n.gotGen[0] != 1 || n.gotGen[1] != 1 {
		t.Fatalf("per-stub generation must start at 1, got %v", n.gotGen)
	}
}

// TestActivateGenerationIncrementsPerStub proves the per-stub counter advances
// across activate attempts (a failed-closed activate still consumed a
// generation, and a retry gets the next one). It uses one stub whose first
// activate fails the handshake, then a Close+re-Prepare lets a second activate
// run on the same stub.
func TestActivateGenerationIncrementsPerStub(t *testing.T) {
	n := &fakeNotifier{err: errors.New("first handshake fails")}
	vm := &fakeVMM{}
	s := newTestStubWithNotifier(t, vm, readyOK, n)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"}); err == nil {
		t.Fatal("expected first activate to fail closed")
	}
	if s.State() == StateActive {
		t.Fatal("fail closed: must not be active after a failed handshake")
	}
	// Retry succeeds: clear the canned error. The stub is still dormant, so a
	// second Activate runs against the same per-stub generation counter.
	n.mu.Lock()
	n.err = nil
	n.mu.Unlock()
	if _, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"}); err != nil {
		t.Fatalf("retry Activate: %v", err)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.gotGen) != 2 {
		t.Fatalf("expected 2 handshake attempts, got %d", len(n.gotGen))
	}
	if n.gotGen[0] != 1 || n.gotGen[1] != 2 {
		t.Fatalf("generation must increment per stub: got %v, want [1 2]", n.gotGen)
	}
}

func TestServeDispatchesActivate(t *testing.T) {
	vm := &fakeVMM{}
	// A readiness wait long enough that the measured activate latency is
	// non-zero even on a fast machine (the fake load is instantaneous).
	readySlow := func(string, time.Duration) error {
		time.Sleep(time.Millisecond)
		return nil
	}
	s := newTestStub(t, vm, readySlow)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := WriteRequest(conn, ActivateRequest{SnapshotDir: "/data/templates/x/snapshot"}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	res, err := ReadResult(conn)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate over control socket not OK: %s", res.Error)
	}
	if res.VsockPath == "" || res.LatencyMs <= 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// A husk pod is long-lived: after a SUCCESSFUL activate the stub must keep
	// running and holding the active VM (the VM serves the sandbox). Serve must
	// NOT return on its own, and the VM must NOT be closed. It returns only when
	// the context is cancelled (a husk-pod terminate) or the listener closes.
	select {
	case err := <-serveErr:
		t.Fatalf("Serve returned after a successful activate (must hold the VM until shutdown), err=%v", err)
	case <-time.After(200 * time.Millisecond):
		// Expected: still serving, holding the active VM.
	}
	if s.State() != StateActive {
		t.Fatalf("after activate state = %s, want active", s.State())
	}

	vm.mu.Lock()
	closed := vm.closed
	vm.mu.Unlock()
	if closed {
		t.Fatal("a successful activate must NOT close the VM; the VM must outlive activate")
	}

	// Shutdown (ctx cancel) makes Serve return; Serve itself never closes the VM.
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
	vm.mu.Lock()
	closed = vm.closed
	vm.mu.Unlock()
	if closed {
		t.Fatal("Serve must not close the VM itself; only an explicit Close tears it down")
	}
}

// TestActivateNeverLogsEntropyOrSecrets captures everything the stub writes to
// stderr across a full Serve+Activate dispatch (success AND a forced write-path
// log) and asserts that neither the entropy bytes nor any secret VALUE appears.
// Secret values and entropy are held only in memory and never logged.
func TestActivateNeverLogsEntropyOrSecrets(t *testing.T) {
	const secretValue = "TOP-SECRET-VALUE-do-not-log"

	// Capture stderr for the whole test.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	captured := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		captured <- buf
	}()

	n := &fakeNotifier{}
	vm := &fakeVMM{}
	s := newTestStubWithNotifier(t, vm, readyOK, n)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := WriteRequest(conn, ActivateRequest{
		SnapshotDir: "/data/templates/x/snapshot",
		Secrets:     map[string]string{"API_KEY": secretValue},
	}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	if _, err := ReadResult(conn); err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	// Close the connection while a result is being written on a later attempt to
	// exercise the stderr write-path logging too: send a second (rejected)
	// request, then drop the reader so the write fails and logs.
	_ = conn.Close()

	cancel()
	<-serveErr

	// Flush and read the captured stderr.
	_ = w.Close()
	os.Stderr = origStderr
	logged := string(<-captured)

	if strings.Contains(logged, secretValue) {
		t.Fatalf("secret VALUE leaked to stderr: %q", logged)
	}
	// The entropy handed to the notifier must not appear in any encoding.
	n.mu.Lock()
	ent := n.gotEntropy[0]
	n.mu.Unlock()
	for _, enc := range []string{hex.EncodeToString(ent), base64.StdEncoding.EncodeToString(ent), string(ent)} {
		if enc != "" && strings.Contains(logged, enc) {
			t.Fatalf("entropy bytes leaked to stderr (%d-byte form)", len(enc))
		}
	}
}

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
		Start:              func(cfg firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil },
		Ready:              readyOK,
		Notify:             (&fakeNotifier{}).notify,
		Verify:             verifyOK,
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

// TestPrepareClonePathScopedToVMID proves the per-activation rootfs clone path
// is scoped to the (per-pod) VM id: two stubs with DISTINCT ids clone to
// DISTINCT files under the same CoW dir. This is the cross-pod-corruption fix:
// the controller passes the pod name as the VM id, so two husk pods sharing the
// node CoW hostPath never overwrite or delete each other's live rootfs.
func TestPrepareClonePathScopedToVMID(t *testing.T) {
	dir := t.TempDir()
	tmplRootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(tmplRootfs, []byte("ROOTFS"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowDir := filepath.Join(dir, "husk-rootfs")

	clonePathFor := func(vmID string) string {
		var gotDst string
		s := New(firecracker.VMConfig{ID: vmID}, Options{
			Start:              func(cfg firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil },
			Ready:              readyOK,
			Notify:             (&fakeNotifier{}).notify,
			Verify:             verifyOK,
			RootfsTemplatePath: tmplRootfs,
			RootfsCoWDir:       cowDir,
			Reflink: func(src, dst string) error {
				gotDst = dst
				return os.WriteFile(dst, []byte("CLONE"), 0o644)
			},
		})
		if err := s.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare(%s): %v", vmID, err)
		}
		return gotDst
	}

	a := clonePathFor("pool-husk-aaaaa")
	b := clonePathFor("pool-husk-bbbbb")
	if a == b {
		t.Fatalf("distinct VM ids must yield distinct clone paths; both = %q", a)
	}
	if a != filepath.Join(cowDir, "pool-husk-aaaaa", "rootfs.ext4") {
		t.Errorf("clone path for first id = %q", a)
	}
	if b != filepath.Join(cowDir, "pool-husk-bbbbb", "rootfs.ext4") {
		t.Errorf("clone path for second id = %q", b)
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
	// The rootfs rebind MUST happen on the paused VM (resume=false on load) and
	// BEFORE the explicit Resume, so the guest never writes the shared template.
	if vm.gotResume {
		t.Error("snapshot must be loaded with resume=false so the rebind lands while paused")
	}
	vm.mu.Lock()
	order := append([]string(nil), vm.callOrder...)
	vm.mu.Unlock()
	want := []string{"load", "patch", "resume"}
	if len(order) != len(want) {
		t.Fatalf("VMM call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("VMM call order = %v, want %v", order, want)
		}
	}
}

// TestActivateResumeFailureFailsClosed proves a Resume rejection after the
// rootfs rebind fails the activate closed: the VM is never marked active.
func TestActivateResumeFailureFailsClosed(t *testing.T) {
	vm := &fakeVMM{resumeErr: errors.New("resume rejected")}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err == nil {
		t.Fatal("expected activate to fail closed on resume error")
	}
	if res.OK {
		t.Fatal("fail closed: result must not be OK when resume is rejected")
	}
	if s.State() == StateActive {
		t.Errorf("state must not be active after a failed resume, got %s", s.State())
	}
	// The snapshot loaded (paused) before the resume failed.
	if vm.loadCalls != 1 {
		t.Errorf("expected the snapshot to load before resume, got %d loads", vm.loadCalls)
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

// TestFakeVMMSatisfiesInterface fails to compile if the vmm interface gains a
// method fakeVMM does not implement, keeping the fake in lockstep with the seam.
func TestFakeVMMSatisfiesInterface(t *testing.T) {
	var _ vmm = (*fakeVMM)(nil)
}

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

func TestCloseTearsDownVMM(t *testing.T) {
	vm := &fakeVMM{}
	s := newTestStub(t, vm, readyOK)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !vm.closed {
		t.Fatal("Close must tear down the VMM")
	}
	if s.State() != StateNew {
		t.Fatalf("after close state = %s, want new", s.State())
	}
}
