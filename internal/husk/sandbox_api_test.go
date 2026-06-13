package husk

// Integration coverage for the in-pod sandbox HTTP API the husk stub serves
// after a successful activate (issue #18, slice 2, Fix B). After Activate the
// stub registers the activated VM and its per-sandbox bearer token with a
// daemon.SandboxAPI and serves the token-gated exec/files API on the sandbox
// port, exactly as forkd does. This test proves:
//
//   - after Activate, an HTTP exec carrying the per-sandbox bearer token reaches
//     the (fake) guest agent over vsock and returns the guest's reply;
//   - an exec WITHOUT the token (or with the wrong token) is rejected (401);
//   - the bearer token VALUE is never written to the captured stub log.
//
// The activate path runs end to end through the Stub OnActivated hook (the same
// hook cmd/husk-stub wires), with a fake VMM, a fake fork-correctness notifier,
// and a REAL fake vsock guest agent on a unix socket, so the exec genuinely
// traverses RegisterSandbox -> vsock -> agent.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/daemon"
	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// fakeVsockAgent listens on sockPath, speaks the Firecracker vsock UDS CONNECT
// preamble, then answers every request with a canned OK exec reply.
func fakeVsockAgent(t *testing.T, sockPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })

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
				if !sc.Scan() { // "CONNECT <port>"
					return
				}
				if _, err := c.Write([]byte("OK 52\n")); err != nil {
					return
				}
				for sc.Scan() {
					resp, _ := json.Marshal(vsock.Response{
						OK:   true,
						Exec: &vsock.ExecResponse{ExitCode: 0, Stdout: "husk-exec-ok\n"},
					})
					if _, err := c.Write(append(resp, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
}

// pathVMM is a vmm whose VsockHostPath returns a fixed host UDS path (the fake
// agent socket), so the OnActivated hook registers the sandbox against a real
// reachable agent.
type pathVMM struct {
	vsockPath string
}

func (m *pathVMM) LoadSnapshotWithOverrides(_, _ string, _ bool, _ []firecracker.NetworkOverride) error {
	return nil
}
func (m *pathVMM) VsockHostPath(string) string { return m.vsockPath }
func (m *pathVMM) Close() error                { return nil }

func postHuskExec(t *testing.T, url, sandbox, bearer string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"sandbox": sandbox, "command": "echo hi"})
	req, err := http.NewRequest(http.MethodPost, url+"/v1/exec", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func TestActivateServesTokenGatedSandboxAPI(t *testing.T) {
	const sandboxID = "husk"
	const token = "per-sandbox-bearer-CANARY-do-not-log"

	// A short /tmp dir: a unix socket path must stay under the OS sun_path limit
	// (104 bytes on darwin), which t.TempDir's long path can exceed.
	dir, err := os.MkdirTemp("/tmp", "husk-sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "vsock.sock")
	fakeVsockAgent(t, sockPath)

	// A real daemon.SandboxAPI; the OnActivated hook registers the activated VM
	// and the delivered token, then we serve its Handler over httptest. This
	// mirrors cmd/husk-stub's makeSandboxServer wiring (register sandbox+token,
	// serve the bearer-gated Handler), without binding the fixed sandbox port.
	api := daemon.NewSandboxAPI(dir)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	logged := captureStderr(t, func() {
		onActivated := func(vsockPath, tok string) error {
			if err := api.RegisterSandbox(sandboxID, vsockPath); err != nil {
				return err
			}
			api.RegisterToken(sandboxID, tok)
			return nil
		}

		vm := &pathVMM{vsockPath: sockPath}
		stub := New(firecracker.VMConfig{ID: sandboxID}, Options{
			Start:       func(firecracker.VMConfig) (vmm, error) { return vm, nil },
			Ready:       func(string, time.Duration) error { return nil },
			Notify:      func(string, uint64, []byte, ActivateRequest) error { return nil },
			Verify:      verifyOK,
			OnActivated: onActivated,
		})

		if err := stub.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		res, err := stub.Activate(context.Background(), ActivateRequest{
			SnapshotDir: "/data/templates/tmpl/snapshot",
			Token:       token,
		})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if !res.OK {
			t.Fatalf("activate not OK: %s", res.Error)
		}

		// A tokened exec reaches the guest and returns its reply.
		code, body := postHuskExec(t, ts.URL, sandboxID, token)
		if code != 200 {
			t.Fatalf("tokened exec status = %d, body = %s, want 200", code, body)
		}
		if !strings.Contains(body, "husk-exec-ok") {
			t.Fatalf("tokened exec did not reach the guest agent: %s", body)
		}

		// An untokened exec is rejected.
		code, _ = postHuskExec(t, ts.URL, sandboxID, "")
		if code != 401 {
			t.Fatalf("untokened exec status = %d, want 401", code)
		}

		// A wrong-token exec is rejected.
		code, _ = postHuskExec(t, ts.URL, sandboxID, "wrong-token")
		if code != 401 {
			t.Fatalf("wrong-token exec status = %d, want 401", code)
		}
	})

	// The per-sandbox bearer token VALUE must never appear in any stub log line.
	if strings.Contains(logged, token) {
		t.Fatalf("bearer token value leaked into stub logs:\n%s", logged)
	}
}
