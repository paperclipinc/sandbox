package daemon

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// newRunCodeAPI wires a fake UDS stream agent that replies to the run_code
// request with canned ExecStreamFrames (a stdout chunk, a result, an exit),
// driven through the real dialStream + StreamConn.RunCode path.
func newRunCodeAPI(t *testing.T) *SandboxAPI {
	t.Helper()
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb1", "vsock.sock")
	startFakeStreamUDS(t, sock, []vsock.ExecStreamFrame{
		{Kind: vsock.FrameChunk, Stream: vsock.StreamStdout, Data: []byte("hi")},
		{Kind: vsock.FrameResult, Result: &vsock.ResultFrame{Text: "42", Data: map[string]string{"text/plain": "42"}}},
		{Kind: vsock.FrameExit, ExitCode: 0},
	})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	return api
}

func TestRunCodeStreamHTTP(t *testing.T) {
	api := newRunCodeAPI(t)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body := `{"sandbox":"sb1","code":"print(42)","language":"python"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/run_code/stream", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q", ct)
	}

	type wireFrame struct {
		Kind   string `json:"kind"`
		Stdout string `json:"stdout"`
		Result *struct {
			Text string            `json:"text"`
			Data map[string]string `json:"data"`
		} `json:"result"`
		ExitCode *int `json:"exit_code"`
	}
	var frames []wireFrame
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var fr wireFrame
		if err := json.Unmarshal(sc.Bytes(), &fr); err != nil {
			t.Fatalf("decode frame %q: %v", sc.Text(), err)
		}
		frames = append(frames, fr)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %+v", len(frames), frames)
	}
	if frames[0].Kind != "stdout" {
		t.Fatalf("frame 0 kind = %q", frames[0].Kind)
	}
	gotOut, _ := base64.StdEncoding.DecodeString(frames[0].Stdout)
	if string(gotOut) != "hi" {
		t.Fatalf("frame 0 stdout = %q", gotOut)
	}
	if frames[1].Kind != "result" || frames[1].Result == nil || frames[1].Result.Text != "42" {
		t.Fatalf("frame 1 = %+v", frames[1])
	}
	if frames[2].Kind != "exit" || frames[2].ExitCode == nil || *frames[2].ExitCode != 0 {
		t.Fatalf("frame 2 = %+v", frames[2])
	}
}

// TestRunCodeStreamRequiresToken proves the per-sandbox bearer token gates the
// run_code surface (it is a new in-guest code-execution path; it must not be
// reachable without the token).
func TestRunCodeStreamRequiresToken(t *testing.T) {
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

	body := `{"sandbox":"sb2","code":"1+1"}`
	resp, err := http.Post(srv.URL+"/v1/run_code/stream", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
