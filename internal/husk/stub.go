package husk

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/snapcompat"
	"github.com/paperclipinc/mitos/internal/volume"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// entropySize is the number of crypto/rand bytes generated per activation and
// handed to the guest via NotifyForked to reseed the kernel CRNG. It matches
// the fork engine's reseed size (internal/daemon notifyForked uses 32 bytes).
const entropySize = 32

// State is the husk stub lifecycle state.
type State int

const (
	// StateNew is before Prepare: no VMM exists.
	StateNew State = iota
	// StateDormant is after Prepare: the Firecracker process and its API
	// socket are up but no snapshot is loaded and the guest is not running.
	StateDormant
	// StateActive is after a successful Activate: the snapshot is loaded,
	// the VM is resumed, and the guest agent has answered over vsock.
	StateActive
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateDormant:
		return "dormant"
	case StateActive:
		return "active"
	default:
		return "unknown"
	}
}

// vmm is the subset of *firecracker.Client the stub drives. Keeping it behind an
// interface lets the activate state machine be unit-tested with a fake, with no
// real Firecracker process or KVM.
type vmm interface {
	// LoadSnapshotWithOverrides loads the snapshot mem+vmstate files and (when
	// resume is true) resumes the VM, remapping NICs per overrides. The husk
	// activate path loads with resume=false so it can rebind the rootfs drive
	// (PatchDrive) while the VM is PAUSED, before the guest can write anything,
	// then resumes explicitly via Resume.
	LoadSnapshotWithOverrides(mem, snapshot string, resume bool, overrides []firecracker.NetworkOverride) error
	// VsockHostPath resolves a relative vsock uds_path to its host location.
	VsockHostPath(rel string) string
	// PatchDrive rebinds an existing baked drive (by drive id) to a host backing
	// file via PATCH /drives, on the loaded-but-PAUSED restored VM (before Resume)
	// so the guest never touches the shared template backing. Firecracker's runtime
	// API controller accepts a drive path_on_host PATCH in the Paused state with no
	// root-device restriction (verified against the pinned v1.15 rpc_interface). The
	// husk activate path uses it to point the rootfs drive at this activation's CoW
	// clone, the same rebind the fork engine applies to volume drives.
	PatchDrive(driveID, pathOnHost string) error
	// Resume transitions the loaded VM from Paused to Running (PATCH /vm Resumed).
	// The husk activate path calls it AFTER the rootfs drive rebind so the guest
	// resumes already bound to its own per-activation rootfs clone.
	Resume() error
	// Close tears the VMM down.
	Close() error
}

// starter brings up a DORMANT Firecracker VMM (process + API socket, not
// booted) and returns it behind the vmm interface. The production starter wraps
// firecracker.StartVM; tests inject a fake.
type starter func(cfg firecracker.VMConfig) (vmm, error)

// guestReady blocks until the guest agent answers a ping over the vsock UDS at
// vsockPath, or the timeout elapses. The production seam connects via
// internal/vsock and pings; tests inject a fake.
type guestReady func(vsockPath string, timeout time.Duration) error

// notifier runs the post-restore fork-correctness handshake against the guest
// agent at vsockPath: it delivers the fresh generation + entropy via
// NotifyForked (so the guest reseeds its CRNG, steps its clock, and re-addresses
// its NIC) and then delivers the claim-time env/secrets, mirroring the daemon's
// deliverConfig. It FAILS CLOSED: it returns an error when the reseed handshake
// fails or the guest reports it did not reseed, so a VM that still shares its
// siblings' CRNG state is never served. The production seam connects via
// internal/vsock; tests inject a fake. The entropy and secret VALUES are never
// logged by any implementation.
type notifier func(vsockPath string, generation uint64, entropy []byte, req ActivateRequest) error

// productionNotifier connects the vsock client to the guest agent at vsockPath
// (the same AgentPort productionGuestReady pings) and runs the handshake in the
// same order the daemon's deliverConfig does: NotifyForkedWithConfig first
// (generation + entropy + per-fork network + volume table), then Configure with
// env+secrets. It fails closed: any connect/handshake error, or a guest that
// reports ReseededRNG=false, returns an error so the stub leaves the VM unserved.
//
// Entropy and secret VALUES never appear in any log line or error here: errors
// carry only the operation and the underlying transport error.
func productionNotifier(vsockPath string, generation uint64, entropy []byte, req ActivateRequest) error {
	client, err := vsock.Connect(vsockPath, vsock.AgentPort)
	if err != nil {
		return fmt.Errorf("connect guest agent for fork handshake: %w", err)
	}
	defer client.Close()

	resp, err := client.NotifyForkedWithConfig(generation, entropy, req.Network, req.Volumes)
	if err != nil {
		return fmt.Errorf("notify guest of fork: %w", err)
	}
	// Fail closed: a guest that did not reseed shares CRNG state with its
	// siblings, which is incorrect (not merely degraded). Do not serve it.
	if resp == nil || !resp.ReseededRNG {
		return fmt.Errorf("guest did not reseed its RNG after restore; refusing to serve a fork that shares CRNG state")
	}

	// Deliver claim-time env+secrets exactly as deliverConfig does: skip when
	// there is nothing to deliver, otherwise hand them to the guest. Secret
	// values are never logged.
	if len(req.Env) == 0 && len(req.Secrets) == 0 {
		return nil
	}
	if err := client.Configure(req.Env, req.Secrets); err != nil {
		return fmt.Errorf("configure guest env/secrets: %w", err)
	}
	return nil
}

// reflinker copies a source file to a destination with copy-on-write semantics
// (reflink where the filesystem supports it, full copy otherwise). The husk
// stub clones the template rootfs to a per-activation file through it. The
// production seam is volume.Backend.ReflinkCopy; tests inject a fake. src and
// dst carry no secrets.
type reflinker func(src, dst string) error

// productionStarter wraps firecracker.StartVM. *firecracker.Client satisfies
// vmm (it has LoadSnapshotWithOverrides, VsockHostPath, and we adapt Kill to
// Close below).
func productionStarter(cfg firecracker.VMConfig) (vmm, error) {
	client, err := firecracker.StartVM(cfg)
	if err != nil {
		return nil, err
	}
	return &clientVMM{Client: client}, nil
}

// clientVMM adapts *firecracker.Client to the vmm interface. Close maps to Kill
// so the husk teardown reaps the Firecracker process.
type clientVMM struct {
	*firecracker.Client
}

func (c *clientVMM) Close() error {
	return c.Client.Kill()
}

// productionGuestReady retries a vsock connect + ping until the guest answers or
// the timeout elapses. It mirrors how cmd/bench waits for a restored guest: the
// agent listens on vsock.AgentPort and answers Ping once the VM is resumed.
func productionGuestReady(vsockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		client, err := vsock.Connect(vsockPath, vsock.AgentPort)
		if err == nil {
			_, perr := client.Ping()
			client.Close()
			if perr == nil {
				return nil
			}
			lastErr = perr
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return fmt.Errorf("guest agent not ready within %s: %w", timeout, lastErr)
}

// Options configures a Stub. Zero values select the production seams, so the
// daemon constructs New(cfg, Options{}). Tests inject fakes.
type Options struct {
	// Start brings up the dormant VMM. Nil uses the production starter.
	Start starter
	// Ready waits for the guest agent. Nil uses the production seam.
	Ready guestReady
	// Notify runs the post-restore fork-correctness handshake. Nil uses the
	// production seam (connect the vsock client and NotifyForked + Configure).
	Notify notifier
	// Verify re-verifies the snapshot at activate time BEFORE it is loaded
	// (digest integrity + snapcompat, fail-closed). Nil uses the production
	// verifier built from ManifestPath, Env, and AllowUnverified below. Tests
	// inject a no-op (or a failing) verifier so they need no on-disk manifest.
	Verify snapshotVerifier
	// ManifestPath is the on-disk path of the recorded CAS manifest mounted into
	// the husk pod read-only; the production verifier decodes it, binds it to the
	// request's ExpectedDigest, and re-hashes the loaded files against it. Empty
	// is only valid with AllowUnverified (development).
	ManifestPath string
	// Env is the detected host environment the production verifier checks snapshot
	// compatibility against (Firecracker version, CPU model, kernel, formats).
	Env snapcompat.Environment
	// AllowUnverified is the development escape hatch mirroring forkd's
	// --allow-unverified-snapshots: when true the verifier warns once and proceeds
	// on a missing-digest or failed check. Default false keeps verify enforced.
	AllowUnverified bool
	// ReadyTimeout bounds the guest-readiness wait during Activate. Zero uses
	// DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// OnActivated is invoked exactly once, after a SUCCESSFUL Activate, with the
	// activated guest agent's host vsock UDS path and the per-sandbox bearer
	// token delivered in the ActivateRequest. The husk pod uses it to register
	// the activated VM with a daemon.SandboxAPI and serve the token-gated sandbox
	// HTTP API (exec/files) on the sandbox port, so the endpoint the claim
	// advertises is actually reachable. The token is a SECRET; the hook must
	// never log it. Nil disables the hook (the control-socket CI driver and unit
	// tests that do not need the sandbox API leave it nil).
	OnActivated func(vsockPath, token string) error
	// PrepareSnapshotDir and PrepareExpectedDigest, when both set, move the
	// fail-closed snapshot verification (the ~680 MiB mem+rootfs re-hash) OFF the
	// Activate hot path and INTO Prepare, where it runs during the pre-paid
	// dormant warm period. The snapshot is a read-only, content-addressed,
	// immutable mount, so verifying it once at Prepare is equivalent to verifying
	// at Activate, and Activate then only confirms the request names the same
	// (dir, digest) it already verified before loading. This is what makes the
	// claim->Ready latency the engine cost (~tens of ms) instead of the hash cost
	// (~1.3 s on a slow CPU). Empty (or AllowUnverified) keeps the verify on the
	// Activate path as before. The values are content addresses, not secrets.
	PrepareSnapshotDir    string
	PrepareExpectedDigest string
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
}

// DefaultReadyTimeout bounds how long Activate waits for the guest agent to
// answer after the snapshot is resumed before failing closed.
const DefaultReadyTimeout = 10 * time.Second

// Stub is a single-VM husk: Prepare brings up a dormant VMM, Activate loads a
// snapshot into it in place, and Serve dispatches one activate request from a
// control socket. It owns exactly one VM for its lifetime.
type Stub struct {
	start        starter
	ready        guestReady
	notify       notifier
	verify       snapshotVerifier
	onActivated  func(vsockPath, token string) error
	cfg          firecracker.VMConfig
	readyTimeout time.Duration

	// prepareSnapshotDir / prepareExpectedDigest are the snapshot the dormant
	// pod verified at Prepare; prepareVerified records that the re-hash passed.
	// Activate skips its own re-hash when the request names this exact snapshot.
	prepareSnapshotDir    string
	prepareExpectedDigest string

	// rootfsTemplatePath / rootfsCoWDir configure the per-activation rootfs CoW;
	// reflink performs the clone; rootfsClonePath records the clone Prepare made so
	// Activate rebinds the drive to it and Close removes it. Empty rootfsClonePath
	// means no per-activation rootfs was prepared (prior behavior).
	rootfsTemplatePath string
	rootfsCoWDir       string
	reflink            reflinker
	rootfsClonePath    string

	mu              sync.Mutex
	state           State
	vm              vmm
	generation      uint64
	prepareVerified bool
}

// New builds a Stub for the given VMConfig. By default it uses the production
// starter and guest-readiness seam; opts may inject fakes for tests.
func New(cfg firecracker.VMConfig, opts Options) *Stub {
	s := &Stub{
		start:        opts.Start,
		ready:        opts.Ready,
		notify:       opts.Notify,
		verify:       opts.Verify,
		onActivated:  opts.OnActivated,
		cfg:          cfg,
		readyTimeout: opts.ReadyTimeout,
		state:        StateNew,

		prepareSnapshotDir:    opts.PrepareSnapshotDir,
		prepareExpectedDigest: opts.PrepareExpectedDigest,

		rootfsTemplatePath: opts.RootfsTemplatePath,
		rootfsCoWDir:       opts.RootfsCoWDir,
		reflink:            opts.Reflink,
	}
	if s.start == nil {
		s.start = productionStarter
	}
	if s.ready == nil {
		s.ready = productionGuestReady
	}
	if s.notify == nil {
		s.notify = productionNotifier
	}
	if s.verify == nil {
		s.verify = productionVerifier(verifyConfig{
			manifestPath:    opts.ManifestPath,
			env:             opts.Env,
			allowUnverified: opts.AllowUnverified,
		})
	}
	if s.readyTimeout == 0 {
		s.readyTimeout = DefaultReadyTimeout
	}
	if s.reflink == nil {
		s.reflink = volume.New("").ReflinkCopy
	}
	return s
}

// Prepare brings up a DORMANT Firecracker VMM (process + API socket, not
// booted) and stores it. It is not idempotent across states: calling it once
// the stub is already dormant or active is an error, so a husk never silently
// leaks a second VMM.
func (s *Stub) Prepare(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateNew {
		return fmt.Errorf("husk: prepare in state %s: already prepared", s.state)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	vm, err := s.start(s.cfg)
	if err != nil {
		return fmt.Errorf("husk: prepare dormant VMM: %w", err)
	}
	s.vm = vm

	// Verify the snapshot NOW, while dormant, instead of on the Activate hot
	// path. When the controller passes the snapshot dir + expected digest at
	// startup we run the full fail-closed re-hash here, during the warm period a
	// claim has not arrived yet. The snapshot is read-only and content-addressed,
	// so this is the same gate Activate would run, just pre-paid. Prepare fails
	// closed: a tampered or incompatible snapshot keeps the pod out of StateDormant
	// so the pool never offers it for a claim. When the inputs are absent (e.g.
	// AllowUnverified / a pre-digest pool) we skip this and Activate verifies as
	// before.
	if s.prepareSnapshotDir != "" && s.prepareExpectedDigest != "" {
		if err := s.verify(ActivateRequest{
			SnapshotDir:    s.prepareSnapshotDir,
			ExpectedDigest: s.prepareExpectedDigest,
		}); err != nil {
			_ = s.vm.Close()
			s.vm = nil
			return fmt.Errorf("husk: prepare-time snapshot verification failed: %w", err)
		}
		s.prepareVerified = true
	}

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
		// Create the clone's parent directory before handing the path to the
		// reflinker. The production seam (volume.ReflinkCopy) also MkdirAlls
		// (idempotent), but doing it here keeps the stub the owner of the clone
		// location so any reflinker, including a test fake, writes to a real dir.
		if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
			_ = s.vm.Close()
			s.vm = nil
			return fmt.Errorf("husk: create per-activation rootfs dir: %w", err)
		}
		if err := s.reflink(s.rootfsTemplatePath, clonePath); err != nil {
			_ = s.vm.Close()
			s.vm = nil
			return fmt.Errorf("husk: clone per-activation rootfs: %w", err)
		}
		s.rootfsClonePath = clonePath
	}

	s.state = StateDormant
	return nil
}

// Activate loads the snapshot into the dormant VMM in place and waits for the
// guest agent to answer.
//
// It FAILS CLOSED: the stub must be dormant (else error and no result), and any
// snapshot-load or guest-readiness failure returns OK=false plus an error and
// leaves the stub NOT active. A failed Activate never reports a usable VM; the
// caller must treat the husk as unusable.
func (s *Stub) Activate(ctx context.Context, req ActivateRequest) (ActivateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateDormant {
		return ActivateResult{OK: false, Error: fmt.Sprintf("activate in state %s: must be dormant", s.state)},
			fmt.Errorf("husk: activate in state %s: must be dormant", s.state)
	}
	if err := ctx.Err(); err != nil {
		return ActivateResult{OK: false, Error: err.Error()}, err
	}
	if req.SnapshotDir == "" {
		return ActivateResult{OK: false, Error: "activate: empty snapshot dir"},
			fmt.Errorf("husk: activate: empty snapshot dir")
	}

	// Same snapshot file layout the fork engine writes: SnapshotDir/mem and
	// SnapshotDir/vmstate.
	memFile := filepath.Join(req.SnapshotDir, "mem")
	vmStateFile := filepath.Join(req.SnapshotDir, "vmstate")

	start := time.Now()

	// Verify-on-activate gate: re-verify the snapshot BEFORE loading it, the same
	// fail-closed integrity + compatibility gate forkd's Fork path applies (digest
	// verify, issue #9, and snapcompat.Check, issue #32). A snapshot tampered on
	// the node disk after forkd's build-time verification, or one incompatible
	// with this node, is refused here and never restored. Runs before any VMM
	// load, so an unverified snapshot never touches the guest.
	//
	// Fast path: if Prepare already verified THIS exact snapshot (same dir + the
	// same content-addressed digest) during the dormant period, the read-only
	// immutable files cannot have changed, so we skip the ~680 MiB re-hash and go
	// straight to load. Any mismatch (a different dir/digest than prepared, or no
	// prepare-time verification) re-verifies here, fail-closed, exactly as before.
	if !(s.prepareVerified && req.SnapshotDir == s.prepareSnapshotDir && req.ExpectedDigest == s.prepareExpectedDigest) {
		if err := s.verify(req); err != nil {
			werr := fmt.Errorf("husk: snapshot verification failed: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	// Load the snapshot PAUSED (resume=false). The rootfs drive rebind below MUST
	// happen before the guest runs, and PATCH /drives on the ROOT device of an
	// already-RESUMED VM both leaves a write window (any writeback between resume
	// and the rebind hits the SHARED template rootfs) and may be rejected by
	// Firecracker. Loading paused lets us rebind while the guest is frozen, then
	// resume explicitly. nil overrides restores exactly as before.
	if err := s.vm.LoadSnapshotWithOverrides(memFile, vmStateFile, false, req.NetworkOverrides); err != nil {
		// Fail closed: the snapshot did not load; the VM is not usable. Leave
		// state dormant so a retry (or teardown) can decide what to do.
		werr := fmt.Errorf("husk: load snapshot from %s: %w", req.SnapshotDir, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	// Rebind the baked "rootfs" drive to THIS activation's CoW clone while the VM
	// is still PAUSED (loaded, not yet resumed), so the guest never writes a single
	// block through the shared template rootfs. This is the husk analog of the fork
	// engine's per-fork volume drive rebind: the snapshot bakes the rootfs block
	// device at path_on_host, and Firecracker's runtime API controller accepts a
	// drive path_on_host PATCH in the Paused state with no root-device restriction.
	// Skipped when no per-activation clone was prepared (the prior shared-rootfs
	// behavior). Fail closed: a rebind failure means the VM is still pointed at the
	// shared template rootfs, which is exactly the corruption hazard this prevents,
	// so do NOT resume or mark active. The drive id and path carry no secrets.
	if s.rootfsClonePath != "" {
		if err := s.vm.PatchDrive("rootfs", s.rootfsClonePath); err != nil {
			werr := fmt.Errorf("husk: rebind rootfs drive to per-activation clone: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	// Resume the VM only AFTER the rootfs drive is rebound, so the guest comes up
	// already bound to its own per-activation rootfs clone, never the shared
	// template. Fail closed: if the resume is rejected the VM never runs, so do NOT
	// mark active.
	if err := s.vm.Resume(); err != nil {
		werr := fmt.Errorf("husk: resume VM after rootfs rebind: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	vsockPath := s.vm.VsockHostPath(firecracker.VsockRelPath)
	if err := s.ready(vsockPath, s.readyTimeout); err != nil {
		// Fail closed: the snapshot loaded but the guest never answered, so we
		// cannot vouch for the VM. Do NOT mark active or report a usable VM.
		werr := fmt.Errorf("husk: guest not ready after activate: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	// Fork-correctness handshake. The restored guest is a byte-for-byte copy of
	// the snapshot, so it shares the snapshot's CRNG and clock state. Reseed it
	// with fresh entropy and deliver claim-time env/secrets BEFORE marking the
	// VM active. The entropy and secret values are held only in memory here and
	// are NEVER logged.
	entropy := make([]byte, entropySize)
	if _, err := rand.Read(entropy); err != nil {
		// Fail closed: without fresh entropy we cannot reseed, so the VM is not
		// safe to serve. The error mentions no entropy bytes.
		werr := fmt.Errorf("husk: generate fork entropy: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}
	s.generation++
	if err := s.notify(vsockPath, s.generation, entropy, req); err != nil {
		// Fail closed: the guest did not complete the reseed handshake, so it may
		// still share its siblings' CRNG state. Leave the VM NOT active. The
		// error carries no entropy or secret values.
		werr := fmt.Errorf("husk: fork-correctness handshake failed: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	// Wire the activated VM into the in-pod sandbox HTTP API (exec/files) before
	// reporting success, so the endpoint the claim advertises is reachable the
	// moment the claim goes Ready. The hook registers the sandbox + its bearer
	// token with a daemon.SandboxAPI and serves it on the sandbox port. FAIL
	// CLOSED: if the sandbox API cannot be served, the VM is not actually usable
	// by a tenant, so do NOT mark active or report OK. The token is a secret and
	// is never logged here. The hook is nil for the control-socket CI driver and
	// unit paths that do not serve the sandbox API.
	if s.onActivated != nil {
		if err := s.onActivated(vsockPath, req.Token); err != nil {
			werr := fmt.Errorf("husk: serve sandbox API for activated VM: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	latency := time.Since(start)
	s.state = StateActive
	return ActivateResult{
		OK:        true,
		VsockPath: vsockPath,
		LatencyMs: float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// Serve accepts control connections on ln and dispatches each to Activate,
// replying with the ActivateResult.
//
// A husk pod is LONG-LIVED: it holds its single active VM until the pod is
// terminated. So a SUCCESSFUL activate does NOT end Serve. After the VM is
// active Serve keeps running, holding the live VM (which now serves the
// sandbox) and rejecting further activate attempts via Activate's state check,
// until ctx is cancelled or the listener closes. Before a successful activate
// it likewise keeps serving so a failed-closed activate can be retried.
//
// Serve never tears the VM down: it returns nil on ctx cancel / listener close
// and leaves the VM running. The caller (cmd/husk-stub) calls Close on real
// shutdown to kill the VM. Per-connection errors are returned to the peer in
// the result and do not stop the server.
func (s *Stub) Serve(ctx context.Context, ln net.Listener) error {
	// Unblock Accept when the context is cancelled.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("husk: accept control connection: %w", err)
		}
		// The activate result is sent to the peer; whether it succeeded or not,
		// the husk keeps serving and holding its VM until shutdown.
		s.handleConn(ctx, conn)
	}
}

// handleConn reads one ActivateRequest, runs Activate, and writes the result.
// Connection-level read/write failures are logged to stderr (paths only, no
// secrets) and do not propagate; the server keeps running.
func (s *Stub) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	req, err := ReadRequest(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "husk: read activate request: %v\n", err)
		return
	}
	res, _ := s.Activate(ctx, req)
	if werr := WriteResult(conn, res); werr != nil {
		fmt.Fprintf(os.Stderr, "husk: write activate result: %v\n", werr)
		// The result may not have reached the peer, but the VM state is what it
		// is; the husk holds the VM per the result we computed.
	}
}

// State returns the current lifecycle state.
func (s *Stub) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Close tears down the VMM if one was prepared. It is safe to call in any state.
func (s *Stub) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Best effort: remove this activation's rootfs CoW clone so it does not
	// outlive the pod. A reflink clone shares extents with the template until
	// written, so removing it frees only the activation's own divergent blocks.
	// Path only is logged on failure; the clone carries no secrets. Done before
	// the vm == nil early return so a clone is reaped even when no VMM is held.
	if s.rootfsClonePath != "" {
		if rmErr := os.Remove(s.rootfsClonePath); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Fprintf(os.Stderr, "husk: remove per-activation rootfs clone %s: %v\n", s.rootfsClonePath, rmErr)
		}
		s.rootfsClonePath = ""
	}

	if s.vm == nil {
		return nil
	}
	err := s.vm.Close()
	s.vm = nil
	s.state = StateNew
	return err
}
