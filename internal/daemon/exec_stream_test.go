package daemon

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// startFakeStreamUDS serves the Firecracker UDS preamble then writes the given
// frames for any exec_stream request.
func startFakeStreamUDS(t *testing.T, sockPath string, frames []vsock.ExecStreamFrame) {
	t.Helper()
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
				sc.Buffer(make([]byte, 1<<20), vsock.MaxMessageBytes)
				if !sc.Scan() {
					return
				}
				if strings.HasPrefix(sc.Text(), "CONNECT ") {
					c.Write([]byte("OK 52\n"))
				}
				if !sc.Scan() {
					return
				}
				for _, f := range frames {
					b, _ := json.Marshal(f)
					c.Write(append(b, '\n'))
				}
			}(conn)
		}
	}()
}

func newStreamAPI(t *testing.T) (*SandboxAPI, string) {
	t.Helper()
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb1", "vsock.sock")
	startFakeStreamUDS(t, sock, []vsock.ExecStreamFrame{
		{Kind: vsock.FrameChunk, Stream: vsock.StreamStdout, Data: []byte("hello\n")},
		{Kind: vsock.FrameChunk, Stream: vsock.StreamStderr, Data: []byte("warn\n")},
		{Kind: vsock.FrameExit, ExitCode: 0, ExecTimeMs: 1.0},
	})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	// Register the shared connection AND record the stream UDS path.
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	return api, sock
}

func TestHandleExecStreamNDJSON(t *testing.T) {
	api, _ := newStreamAPI(t)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb1", "command": "echo hello"})
	resp, err := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	var sawExit bool
	var out string
	for sc.Scan() {
		var line struct {
			Stream   string `json:"stream"`
			Data     string `json:"data"`
			ExitCode *int   `json:"exit_code"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bad ndjson line %q: %v", sc.Text(), err)
		}
		if line.ExitCode != nil {
			sawExit = true
			if *line.ExitCode != 0 {
				t.Fatalf("exit code = %d", *line.ExitCode)
			}
			continue
		}
		if line.Stream == "stdout" {
			d, _ := base64.StdEncoding.DecodeString(line.Data)
			out += string(d)
		}
	}
	if !sawExit {
		t.Fatal("no exit line")
	}
	if out != "hello\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestHandleExecAggregatesStream(t *testing.T) {
	api, _ := newStreamAPI(t)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb1", "command": "echo hello"})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got vsock.ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "hello\n" || got.Stderr != "warn\n" || got.ExitCode != 0 {
		t.Fatalf("aggregate = %+v", got)
	}
}

func TestExecStreamRequiresToken(t *testing.T) {
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb2", "vsock.sock")
	startFakeStreamUDS(t, sock, []vsock.ExecStreamFrame{{Kind: vsock.FrameExit}})
	api := NewSandboxAPI(dir) // NOT tokenless
	if err := api.RegisterSandbox("sb2", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb2", sock)
	api.RegisterToken("sb2", "secret")
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb2", "command": "x"})
	resp, err := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestStreamPathRegisteredWithSandbox(t *testing.T) {
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb3", "vsock.sock")
	startFakeStreamUDS(t, sock, []vsock.ExecStreamFrame{{Kind: vsock.FrameExit, ExitCode: 0}})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb3", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb3", sock)
	sc, err := api.dialStream("sb3")
	if err != nil {
		t.Fatalf("dialStream after register: %v", err)
	}
	sc.Close()
}
