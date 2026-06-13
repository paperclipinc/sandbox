package daemon

import (
	"bufio"
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

	"github.com/coder/websocket"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// startFakePtyUDS serves the CONNECT preamble, reads the pty request line, then
// echoes any input frame as an output frame and exits on "exit\n".
func startFakePtyUDS(t *testing.T, sockPath string) {
	t.Helper()
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
				sc.Buffer(make([]byte, 1<<20), vsock.MaxMessageBytes)
				if !sc.Scan() {
					return
				}
				if strings.HasPrefix(sc.Text(), "CONNECT ") {
					c.Write([]byte("OK 52\n"))
				}
				if !sc.Scan() {
					return // pty request line
				}
				for sc.Scan() {
					var f vsock.PtyFrame
					if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
						return
					}
					if f.Kind == vsock.PtyInput {
						if string(f.Data) == "exit\n" {
							b, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: 0})
							c.Write(append(b, '\n'))
							return
						}
						b, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyOutput, Data: f.Data})
						c.Write(append(b, '\n'))
					}
				}
			}(conn)
		}
	}()
}

func newPtyAPI(t *testing.T, token string) (*SandboxAPI, *httptest.Server) {
	t.Helper()
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb1", "vsock.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	startFakePtyUDS(t, sock)
	api := NewSandboxAPI(dir)
	if token != "" {
		api.RegisterToken("sb1", token)
	} else {
		api.AllowTokenless()
	}
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return api, srv
}

func wsURL(httpURL, sandbox string) string {
	return strings.Replace(httpURL, "http://", "ws://", 1) + "/v1/pty?sandbox=" + sandbox
}

func TestPtyWebSocketEchoExit(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "sb1"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer sekret"}},
		Subprotocols: []string{"mitos.pty.v1"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	in, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("hello-pty\n")})
	if err := c.Write(ctx, websocket.MessageText, in); err != nil {
		t.Fatalf("write input: %v", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var out vsock.PtyFrame
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Kind != vsock.PtyOutput || string(out.Data) != "hello-pty\n" {
		t.Fatalf("output = %+v", out)
	}

	exitFrame, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("exit\n")})
	_ = c.Write(ctx, websocket.MessageText, exitFrame)
	_, data, err = c.Read(ctx)
	if err != nil {
		t.Fatalf("read exit: %v", err)
	}
	var ex vsock.PtyFrame
	_ = json.Unmarshal(data, &ex)
	if ex.Kind != vsock.PtyExit {
		t.Fatalf("expected exit frame, got %+v", ex)
	}
}

func TestPtyWebSocketRejectsBadToken(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL, "sb1"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}},
	})
	if err == nil {
		t.Fatal("expected dial to fail on bad token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v", resp)
	}
}

func TestPtyWebSocketRejectsMissingToken(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL, "sb1"), nil)
	if err == nil {
		t.Fatal("expected dial to fail without a token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v", resp)
	}
}

// TestPtyWebSocketRejectsCrossSandboxToken registers two sandboxes A and B with
// distinct tokens, then attempts to open B's PTY using A's token. The auth gate
// compares the presented token against the token of the ?sandbox= id (B), so
// A's token must not drive B's PTY: the upgrade must be rejected with 401.
func TestPtyWebSocketRejectsCrossSandboxToken(t *testing.T) {
	dir := shortVsockDir(t)
	api := NewSandboxAPI(dir)
	for _, sb := range []struct{ id, token string }{{"sbA", "tokenA"}, {"sbB", "tokenB"}} {
		sock := filepath.Join(dir, sb.id, "vsock.sock")
		if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
			t.Fatal(err)
		}
		startFakePtyUDS(t, sock)
		api.RegisterToken(sb.id, sb.token)
		if err := api.RegisterSandbox(sb.id, sock); err != nil {
			t.Fatal(err)
		}
		api.RegisterStreamPath(sb.id, sock)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Present A's token while targeting B's PTY.
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL, "sbB"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer tokenA"}},
	})
	if err == nil {
		t.Fatal("expected dial to fail: A's token must not drive B's pty")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v", resp)
	}
}

func TestPtyWebSocketTokenlessAllowed(t *testing.T) {
	_, srv := newPtyAPI(t, "") // AllowTokenless, like sandbox-server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "sb1"), &websocket.DialOptions{
		Subprotocols: []string{"mitos.pty.v1"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	exitFrame, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("exit\n")})
	_ = c.Write(ctx, websocket.MessageText, exitFrame)
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var ex vsock.PtyFrame
	_ = json.Unmarshal(data, &ex)
	if ex.Kind != vsock.PtyExit {
		t.Fatalf("expected exit, got %+v", ex)
	}
}
