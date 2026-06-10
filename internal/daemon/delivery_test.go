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

	"github.com/paperclipinc/sandbox/internal/fork"
	"github.com/paperclipinc/sandbox/internal/vsock"
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
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	_, err := srv.Fork(context.Background(), "py", "sb-secret", nil,
		map[string]string{"API_KEY": "v"})
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

func TestForkEnvOnlyIsBestEffortWhenAgentUnreachable(t *testing.T) {
	engine := &kvmReportingEngine{MockEngine: fork.NewMockEngine()}
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	result, err := srv.Fork(context.Background(), "py", "sb-env",
		map[string]string{"SESSION": "abc"}, nil)
	if err != nil {
		t.Fatalf("env-only fork should succeed best-effort: %v", err)
	}
	if result.SandboxID != "sb-env" {
		t.Fatalf("got %q", result.SandboxID)
	}
}

func TestForkMockEngineSkipsDelivery(t *testing.T) {
	engine := fork.NewMockEngine() // KVMAvailable=false
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	if _, err := srv.Fork(context.Background(), "py", "sb-mock", nil,
		map[string]string{"API_KEY": "v"}); err != nil {
		t.Fatalf("mock-mode fork must not require delivery: %v", err)
	}
}

// startFakeVsockAgent listens on sockPath, speaks the Firecracker vsock UDS
// preamble, then the JSON agent protocol, recording configure payloads.
func startFakeVsockAgent(t *testing.T, sockPath string) *recordedConfig {
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
	mu  sync.Mutex
	got *vsock.ConfigureRequest
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
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}
	// The mock will report this exact path for sandbox "sb-ok".
	rec := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-ok", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	if _, err := srv.Fork(context.Background(), "py", "sb-ok",
		map[string]string{"SESSION": "abc"},
		map[string]string{"API_KEY": "v"}); err != nil {
		t.Fatalf("fork with reachable agent: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.got == nil || rec.got.Env["SESSION"] != "abc" || rec.got.Secrets["API_KEY"] != "v" {
		t.Fatalf("agent saw %+v", rec.got)
	}
}
