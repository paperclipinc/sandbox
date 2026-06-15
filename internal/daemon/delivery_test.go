package daemon

// Delivery-policy tests for Server.Fork.
//
// Fallback scoping note: SandboxAPI.RegisterSandbox used to fall back to the
// fixed unix socket /tmp/sandbox-agent-52.sock on ANY vsock dial failure,
// which made the "unreachable agent" tests here ambiguous: a stray local
// agent listening on that socket would have let them accidentally connect.
// The fallback is now opt-in (SandboxAPI.EnableUnixFallback, used only by the
// standalone sandbox-server) and additionally only attempted when the vsock
// UDS path does not exist. forkd never enables it, so these tests are
// deterministic: a missing vsock path is always "unreachable".

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/vsock"
	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
)

// kvmReportingEngine wraps MockEngine but claims to be a real KVM engine,
// so Server.Fork applies the strict delivery policy.
type kvmReportingEngine struct {
	*fork.MockEngine
	terminated []string
}

func (e *kvmReportingEngine) GetCapacity() fork.Capacity {
	c := e.MockEngine.GetCapacity()
	c.KVMAvailable = true
	return c
}

func (e *kvmReportingEngine) Terminate(id string) error {
	e.terminated = append(e.terminated, id)
	return e.MockEngine.Terminate(id)
}

func TestForkWithSecretsFailsWhenAgentUnreachable(t *testing.T) {
	engine := &kvmReportingEngine{MockEngine: fork.NewMockEngine()}
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	_, err := srv.Fork(context.Background(), "py", "sb-secret", nil,
		map[string]string{"API_KEY": "v"}, nil, nil, "test-token")
	if err == nil {
		t.Fatal("fork with undeliverable secrets must fail")
	}
	if len(engine.terminated) != 1 || engine.terminated[0] != "sb-secret" {
		t.Fatalf("sandbox not reaped after failed delivery: %v", engine.terminated)
	}
	if got := engine.GetCapacity().ActiveSandboxes; got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}
}

// Env delivery is still best-effort, but the agent connection and NotifyForked
// are not: a real-engine fork whose guest is unreachable cannot reseed its RNG,
// so it fails closed and is reaped even when only env (no secrets) was
// requested.
func TestForkFailsWhenAgentUnreachableEvenEnvOnly(t *testing.T) {
	engine := &kvmReportingEngine{MockEngine: fork.NewMockEngine()}
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	_, err := srv.Fork(context.Background(), "py", "sb-env",
		map[string]string{"SESSION": "abc"}, nil, nil, nil, "test-token")
	if err == nil {
		t.Fatal("real-engine fork with unreachable guest must fail (cannot reseed RNG)")
	}
	if len(engine.terminated) != 1 || engine.terminated[0] != "sb-env" {
		t.Fatalf("sandbox not reaped: %v", engine.terminated)
	}
}

func TestForkMockEngineSkipsDelivery(t *testing.T) {
	engine := fork.NewMockEngine() // KVMAvailable=false
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	if _, err := srv.Fork(context.Background(), "py", "sb-mock", nil,
		map[string]string{"API_KEY": "v"}, nil, nil, "test-token"); err != nil {
		t.Fatalf("mock-mode fork must not require delivery: %v", err)
	}
}

// startFakeVsockAgent listens on sockPath, speaks the Firecracker vsock UDS
// preamble, then the JSON agent protocol, recording configure and
// notify_forked payloads. On notify_forked it replies OK with a
// NotifyForkedResponse reporting ReseededRNG:true, the success the daemon's
// fail-closed gate requires.
func startFakeVsockAgent(t *testing.T, sockPath string) *recordedConfig {
	return startFakeVsockAgentErr(t, sockPath, false)
}

// startFakeVsockAgentNoReseed replies to notify_forked with OK:true but
// ReseededRNG:false, the real un-reseeded-fork failure mode: the transport
// succeeds, yet the guest signals it did not reseed its CRNG. The daemon must
// FAIL CLOSED on this and never mark the sandbox Ready.
func startFakeVsockAgentNoReseed(t *testing.T, sockPath string) *recordedConfig {
	return startFakeVsockAgentReseed(t, sockPath, false, false)
}

func startFakeVsockAgentErr(t *testing.T, sockPath string, notifyErr bool) *recordedConfig {
	// notifyErr replies with a transport-level OK:false; otherwise the guest
	// reports a successful reseed (ReseededRNG:true).
	return startFakeVsockAgentReseed(t, sockPath, notifyErr, true)
}

func startFakeVsockAgentReseed(t *testing.T, sockPath string, notifyErr, reseeded bool) *recordedConfig {
	t.Helper()
	rec := &recordedConfig{}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				if !sc.Scan() { // "CONNECT 52"
					return
				}
				if _, err := c.Write([]byte("OK 52\n")); err != nil {
					return
				}
				for sc.Scan() {
					var req vsock.Request
					if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
						return
					}
					if req.Type == vsock.TypeConfigure {
						rec.mu.Lock()
						rec.got = req.Configure
						rec.mu.Unlock()
					}
					if req.Type == vsock.TypeNotifyForked {
						rec.mu.Lock()
						rec.notifies = append(rec.notifies, req.NotifyForked)
						rec.mu.Unlock()
						if notifyErr {
							resp, _ := json.Marshal(vsock.Response{OK: false, Error: "reseed failed"})
							if _, err := c.Write(append(resp, '\n')); err != nil {
								return
							}
							continue
						}
						// Transport OK; the guest reports whether it actually
						// reseeded its CRNG. reseeded=false is the silent
						// un-reseeded fork the daemon must fail closed on.
						resp, _ := json.Marshal(vsock.Response{
							OK:           true,
							NotifyForked: &vsock.NotifyForkedResponse{ReseededRNG: reseeded},
						})
						if _, err := c.Write(append(resp, '\n')); err != nil {
							return
						}
						continue
					}
					resp, _ := json.Marshal(vsock.Response{OK: true})
					if _, err := c.Write(append(resp, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return rec
}

type recordedConfig struct {
	mu       sync.Mutex
	got      *vsock.ConfigureRequest
	notifies []*vsock.NotifyForkedRequest
}

func TestForkDeliversConfigureToAgent(t *testing.T) {
	// Short tempdir: unix socket paths must fit in sun_path (~104 bytes on
	// macOS), which t.TempDir() can exceed.
	dir, err := os.MkdirTemp("/tmp", "fcv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	mock := fork.NewMockEngine()
	mock.ForkDelay = 0
	mock.VsockDir = dir
	engine := &kvmReportingEngine{MockEngine: mock}
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	// The mock will report this exact path for sandbox "sb-ok".
	rec := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-ok", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	if _, err := srv.Fork(context.Background(), "py", "sb-ok",
		map[string]string{"SESSION": "abc"},
		map[string]string{"API_KEY": "v"}, nil, nil, "test-token"); err != nil {
		t.Fatalf("fork with reachable agent: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.got == nil || rec.got.Env["SESSION"] != "abc" || rec.got.Secrets["API_KEY"] != "v" {
		t.Fatalf("agent saw %+v", rec.got)
	}
}

// shortVsockDir returns a /tmp-rooted dir; unix socket paths must fit in
// sun_path (~104 bytes on macOS), which t.TempDir() can exceed.
func shortVsockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fcv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func kvmEngineWithTemplate(t *testing.T, dir string) *kvmReportingEngine {
	t.Helper()
	mock := fork.NewMockEngine()
	mock.ForkDelay = 0
	mock.VsockDir = dir
	engine := &kvmReportingEngine{MockEngine: mock}
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	return engine
}

func isAllZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func TestForkNotifiesAgentWithFreshEntropy(t *testing.T) {
	dir := shortVsockDir(t)
	engine := kvmEngineWithTemplate(t, dir)
	rec := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-ok", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	if _, err := srv.Fork(context.Background(), "py", "sb-ok", nil, nil, nil, nil, "test-token"); err != nil {
		t.Fatalf("fork: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.notifies) != 1 {
		t.Fatalf("expected exactly one notify_forked, got %d", len(rec.notifies))
	}
	n := rec.notifies[0]
	if len(n.Entropy) != 32 {
		t.Errorf("entropy length = %d, want 32", len(n.Entropy))
	}
	if isAllZero(n.Entropy) {
		t.Error("entropy is all zero")
	}
}

// TestForkDeliversVolumeMountTable proves the daemon plumbs the engine's
// per-fork volume mount table into the notify_forked message the guest agent
// receives: the i-th volume drive is /dev/vd{b+i}, the mount paths come from the
// specs, and the Share volume is delivered read-only even though its spec did
// not set ReadOnly (the resolved drive policy).
func TestForkDeliversVolumeMountTable(t *testing.T) {
	dir := shortVsockDir(t)
	engine := kvmEngineWithTemplate(t, dir)
	rec := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-vol", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	volumes := []*forkdpb.VolumeMount{
		{Name: "data", MountPath: "/data", Size: "64Mi", ForkPolicy: "Fresh"},
		{Name: "shared", MountPath: "/shared", Size: "64Mi", ForkPolicy: "Share"},
	}
	if _, err := srv.Fork(context.Background(), "py", "sb-vol", nil, nil, nil, volumes, "t"); err != nil {
		t.Fatalf("fork: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.notifies) != 1 {
		t.Fatalf("expected one notify_forked, got %d", len(rec.notifies))
	}
	mounts := rec.notifies[0].Volumes
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mount entries, got %d: %+v", len(mounts), mounts)
	}
	if mounts[0].Device != "/dev/vdb" || mounts[0].MountPath != "/data" || mounts[0].ReadOnly {
		t.Errorf("entry[0] = %+v, want {/dev/vdb /data ro=false}", mounts[0])
	}
	if mounts[1].Device != "/dev/vdc" || mounts[1].MountPath != "/shared" || !mounts[1].ReadOnly {
		t.Errorf("entry[1] = %+v, want {/dev/vdc /shared ro=true}", mounts[1])
	}
}

func TestForkGenerationIncrementsAcrossForks(t *testing.T) {
	dir := shortVsockDir(t)
	engine := kvmEngineWithTemplate(t, dir)
	rec1 := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-1", "vsock.sock"))
	rec2 := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-2", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	if _, err := srv.Fork(context.Background(), "py", "sb-1", nil, nil, nil, nil, "t"); err != nil {
		t.Fatalf("fork 1: %v", err)
	}
	if _, err := srv.Fork(context.Background(), "py", "sb-2", nil, nil, nil, nil, "t"); err != nil {
		t.Fatalf("fork 2: %v", err)
	}

	rec1.mu.Lock()
	rec2.mu.Lock()
	defer rec1.mu.Unlock()
	defer rec2.mu.Unlock()
	if len(rec1.notifies) != 1 || len(rec2.notifies) != 1 {
		t.Fatalf("notifies: sb-1=%d sb-2=%d", len(rec1.notifies), len(rec2.notifies))
	}
	if rec1.notifies[0].Generation == rec2.notifies[0].Generation {
		t.Errorf("generations not distinct: both %d", rec1.notifies[0].Generation)
	}
	if rec2.notifies[0].Generation <= rec1.notifies[0].Generation {
		t.Errorf("generation did not increment: %d then %d",
			rec1.notifies[0].Generation, rec2.notifies[0].Generation)
	}
}

func TestForkFailsWhenNotifyForkedErrors(t *testing.T) {
	dir := shortVsockDir(t)
	engine := kvmEngineWithTemplate(t, dir)
	startFakeVsockAgentErr(t, filepath.Join(dir, "sandboxes", "sb-bad", "vsock.sock"), true)

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	_, err := srv.Fork(context.Background(), "py", "sb-bad", nil, nil, nil, nil, "test-token")
	if err == nil {
		t.Fatal("fork must fail when the guest cannot reseed RNG state")
	}
	if len(engine.terminated) != 1 || engine.terminated[0] != "sb-bad" {
		t.Fatalf("sandbox not reaped after failed notify: %v", engine.terminated)
	}
	if got := engine.GetCapacity().ActiveSandboxes; got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}
}

// TestForkFailsWhenGuestDoesNotReseed is the regression guard for the real
// un-reseeded-fork failure mode (security review blocker 2): the transport
// SUCCEEDS (OK:true) but the guest reports ReseededRNG:false. A fork whose
// guest did not reseed its CRNG shares RNG state with its siblings (duplicate
// TLS keys / tokens / nonces), so the daemon must FAIL CLOSED: the fork must
// error and be reaped, never marked Ready. Distinct from
// TestForkFailsWhenNotifyForkedErrors, which only covers a transport OK:false.
func TestForkFailsWhenGuestDoesNotReseed(t *testing.T) {
	dir := shortVsockDir(t)
	engine := kvmEngineWithTemplate(t, dir)
	startFakeVsockAgentNoReseed(t, filepath.Join(dir, "sandboxes", "sb-noreseed", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	_, err := srv.Fork(context.Background(), "py", "sb-noreseed", nil, nil, nil, nil, "test-token")
	if err == nil {
		t.Fatal("fork must fail when the guest reports ReseededRNG:false")
	}
	if len(engine.terminated) != 1 || engine.terminated[0] != "sb-noreseed" {
		t.Fatalf("sandbox not reaped after un-reseeded fork: %v", engine.terminated)
	}
	if got := engine.GetCapacity().ActiveSandboxes; got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}
}

// TestForkRunningFailsWhenGuestDoesNotReseed is the live-fork (ForkRunning)
// counterpart: a live fork whose guest reports ReseededRNG:false shares its
// parent's CRNG state and must be reaped, not served.
func TestForkRunningFailsWhenGuestDoesNotReseed(t *testing.T) {
	dir := shortVsockDir(t)
	engine := kvmEngineWithTemplate(t, dir)
	if _, err := engine.Fork("py", "sb-src", fork.ForkOpts{}); err != nil {
		t.Fatal(err)
	}
	startFakeVsockAgentNoReseed(t, filepath.Join(dir, "sandboxes", "sb-live-noreseed", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	_, err := srv.ForkRunning(context.Background(), "sb-src", "sb-live-noreseed", false, "t")
	if err == nil {
		t.Fatal("live fork must fail when the guest reports ReseededRNG:false")
	}
	if len(engine.terminated) != 1 || engine.terminated[0] != "sb-live-noreseed" {
		t.Fatalf("live fork not reaped after un-reseeded fork: %v", engine.terminated)
	}
}

func TestForkMockEngineSendsNoNotify(t *testing.T) {
	dir := shortVsockDir(t)
	mock := fork.NewMockEngine() // KVMAvailable=false
	mock.ForkDelay = 0
	mock.VsockDir = dir
	if err := mock.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	rec := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-mock", "vsock.sock"))

	srv := NewServer(mock, NewSandboxAPI(t.TempDir()))
	if _, err := srv.Fork(context.Background(), "py", "sb-mock", nil, nil, nil, nil, "t"); err != nil {
		t.Fatalf("mock fork: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.notifies) != 0 {
		t.Fatalf("mock engine must not notify guests, got %d", len(rec.notifies))
	}
}

func TestForkRunningNotifiesAgent(t *testing.T) {
	dir := shortVsockDir(t)
	engine := kvmEngineWithTemplate(t, dir)
	// Seed a source sandbox to fork from.
	if _, err := engine.Fork("py", "sb-src", fork.ForkOpts{}); err != nil {
		t.Fatal(err)
	}
	rec := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-live", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	if _, err := srv.ForkRunning(context.Background(), "sb-src", "sb-live", false, "t"); err != nil {
		t.Fatalf("fork running: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.notifies) != 1 {
		t.Fatalf("live fork must notify guest, got %d", len(rec.notifies))
	}
	if len(rec.notifies[0].Entropy) != 32 {
		t.Errorf("entropy length = %d, want 32", len(rec.notifies[0].Entropy))
	}
}
