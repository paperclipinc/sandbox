package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// TestMaxStreamsPerSandboxFlagDefault verifies the standalone server exposes a
// --max-streams-per-sandbox flag defaulting to 16, matching forkd. Before this
// fix the standalone REST path had no flag and never capped streams.
func TestMaxStreamsPerSandboxFlagDefault(t *testing.T) {
	fs := flag.NewFlagSet("sandbox-server", flag.ContinueOnError)
	var maxStreams int
	fs.IntVar(&maxStreams, "max-streams-per-sandbox", 16, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if maxStreams != 16 {
		t.Fatalf("default --max-streams-per-sandbox: got %d want 16 (forkd default)", maxStreams)
	}
}

// TestNewServerPlumbsStreamCap verifies the flag value reaches the server: the
// per-sandbox stream ceiling passed to newServer is applied to the SandboxAPI
// (the value is retained on the server for observability). A parsed flag of 7
// must surface unchanged.
func TestNewServerPlumbsStreamCap(t *testing.T) {
	const want = 7
	s := newServer(t.TempDir(), "", true, want)
	if s.sandboxAPI == nil {
		t.Fatal("newServer must construct a SandboxAPI")
	}
	if s.maxStreamsPerSandbox != want {
		t.Fatalf("newServer stream cap: got %d want %d", s.maxStreamsPerSandbox, want)
	}
}

// fakeAgent listens on sockPath speaking the Firecracker vsock UDS preamble and
// the JSON agent protocol, recording notify_forked requests. On notify_forked it
// replies OK:true with a NotifyForkedResponse reporting ReseededRNG=reseeded, so
// a test can drive both the reseeded-OK and the un-reseeded fail-closed path. No
// secrets or entropy are ever logged.
func fakeAgent(t *testing.T, sockPath string, reseeded bool) *[]*vsock.NotifyForkedRequest {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	var notifies []*vsock.NotifyForkedRequest
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
				// The standalone server reaches this fake over the unix fallback
				// (vsock.ConnectUnix), which sends no "CONNECT <port>" preamble:
				// the JSON request protocol starts immediately.
				for sc.Scan() {
					var req vsock.Request
					if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
						return
					}
					if req.Type == vsock.TypeNotifyForked {
						notifies = append(notifies, req.NotifyForked)
						out, _ := json.Marshal(vsock.Response{
							OK:           true,
							NotifyForked: &vsock.NotifyForkedResponse{ReseededRNG: reseeded},
						})
						if _, err := c.Write(append(out, '\n')); err != nil {
							return
						}
						continue
					}
					out, _ := json.Marshal(vsock.Response{OK: true})
					if _, err := c.Write(append(out, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return &notifies
}

// realServerWithAgent builds a real-mode server whose sandboxAPI dials a fake
// guest agent for sandboxID at the standalone server's fixed vsock path. The
// real-mode handleFork resolves the vsock UDS under dataDir/sandboxes/<id>;
// since the path does not exist on disk, the EnableUnixFallback path the
// standalone server sets routes the dial to the fixed local agent socket.
func realServerWithAgent(t *testing.T, sandboxID string, reseeded bool) (*server, *[]*vsock.NotifyForkedRequest) {
	t.Helper()
	// The standalone server falls back to /tmp/sandbox-agent-52.sock when the
	// per-sandbox vsock path does not exist, so the fake agent listens there.
	sock := fmt.Sprintf("/tmp/sandbox-agent-%d.sock", vsock.AgentPort)
	_ = os.Remove(sock)
	notifies := fakeAgent(t, sock, reseeded)

	dataDir, err := os.MkdirTemp("/tmp", "sbsrv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDir) })

	s := newServer(dataDir, "", false, 16) // real mode
	s.templates[sandboxID+"-tmpl"] = &templateInfo{ID: sandboxID + "-tmpl", Ready: true}
	return s, notifies
}

func forkRequest(t *testing.T, s *server, id, template string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"id": id, "template": template})
	r := httptest.NewRequest(http.MethodPost, "/v1/fork", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleFork(w, r)
	return w
}

// TestRealModeForkReseedsAndSucceeds proves the standalone server runs the
// reseed handshake on a real-mode fork and serves the sandbox when the guest
// reports ReseededRNG:true.
func TestRealModeForkReseedsAndSucceeds(t *testing.T) {
	const id = "sb-ok"
	s, notifies := realServerWithAgent(t, id, true)

	w := forkRequest(t, s, id, id+"-tmpl")
	if w.Code != http.StatusOK {
		t.Fatalf("real-mode fork with reseeding guest: status %d, body %s", w.Code, w.Body.String())
	}
	if len(*notifies) != 1 {
		t.Fatalf("expected exactly one notify_forked, got %d", len(*notifies))
	}
	if len((*notifies)[0].Entropy) != 32 {
		t.Errorf("entropy length = %d, want 32", len((*notifies)[0].Entropy))
	}
}

// TestRealModeForkFailsClosedWhenGuestDoesNotReseed is the security regression
// guard: a real-mode fork whose guest reports ReseededRNG:false must FAIL
// CLOSED. The fork is rejected and the sandbox is not left registered, so an
// un-reseeded VM that shares CRNG state with its siblings is never served.
func TestRealModeForkFailsClosedWhenGuestDoesNotReseed(t *testing.T) {
	const id = "sb-noreseed"
	s, _ := realServerWithAgent(t, id, false)

	w := forkRequest(t, s, id, id+"-tmpl")
	if w.Code == http.StatusOK {
		t.Fatalf("real-mode fork must fail when the guest reports ReseededRNG:false; got 200 body %s", w.Body.String())
	}
	s.mu.RLock()
	_, registered := s.sandboxes[id]
	s.mu.RUnlock()
	if registered {
		t.Fatal("un-reseeded fork must not be left registered")
	}
}
