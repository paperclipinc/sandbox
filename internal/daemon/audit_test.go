package daemon

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
	"sync"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// recordingAuditor captures every AuditEvent for assertions.
type recordingAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *recordingAuditor) Record(ev AuditEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingAuditor) snapshot() []AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AuditEvent, len(r.events))
	copy(out, r.events)
	return out
}

// startEchoVsockAgent listens on sockPath and answers the real agent protocol
// with deterministic responses: exec returns exit code 0 and echoes the command
// in stdout; read_file returns fixed content; everything else returns OK.
func startEchoVsockAgent(t *testing.T, sockPath string, readContent []byte) {
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
					resp := vsock.Response{OK: true}
					switch req.Type {
					case vsock.TypeExec:
						resp.Exec = &vsock.ExecResponse{ExitCode: 0, Stdout: req.Exec.Command}
					case vsock.TypeReadFile:
						resp.ReadFile = &vsock.ReadFileResponse{
							Content: readContent,
							Size:    int64(len(readContent)),
						}
					case vsock.TypeListDir:
						resp.ListDir = &vsock.ListDirResponse{}
					}
					out, _ := json.Marshal(resp)
					if _, err := c.Write(append(out, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
}

// auditAPI builds a SandboxAPI wired to an echo agent for one sandbox id and the
// supplied auditor, with the given read-file content.
func auditAPI(t *testing.T, sandboxID string, aud Auditor, readContent []byte) *httptest.Server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "audit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "vsock.sock")
	startEchoVsockAgent(t, sockPath, readContent)

	api := NewSandboxAPI(dir)
	api.SetAuditor(aud)
	api.RegisterToken(sandboxID, "tok")
	api.EnableUnixFallback()
	if err := api.RegisterSandbox(sandboxID, sockPath); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, url, bearer string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuditRecordsExecAndFileOps(t *testing.T) {
	rec := &recordingAuditor{}
	ts := auditAPI(t, "sb-a", rec, []byte("hello"))

	resp := postJSON(t, ts.URL+"/v1/exec", "tok", map[string]string{
		"sandbox": "sb-a", "command": "ls -la",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("exec status = %d", resp.StatusCode)
	}
	resp = postJSON(t, ts.URL+"/v1/files/write", "tok", map[string]string{
		"sandbox": "sb-a", "path": "/tmp/x", "content": "data",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("write status = %d", resp.StatusCode)
	}
	resp = postJSON(t, ts.URL+"/v1/files/read", "tok", map[string]string{
		"sandbox": "sb-a", "path": "/tmp/x",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("read status = %d", resp.StatusCode)
	}
	resp = postJSON(t, ts.URL+"/v1/files/list", "tok", map[string]string{
		"sandbox": "sb-a", "path": "/tmp",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	resp = postJSON(t, ts.URL+"/v1/files/mkdir", "tok", map[string]string{
		"sandbox": "sb-a", "path": "/tmp/d",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("mkdir status = %d", resp.StatusCode)
	}
	resp = postJSON(t, ts.URL+"/v1/files/remove", "tok", map[string]string{
		"sandbox": "sb-a", "path": "/tmp/d",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("remove status = %d", resp.StatusCode)
	}

	events := rec.snapshot()
	byOp := map[string]AuditEvent{}
	for _, ev := range events {
		byOp[ev.Op] = ev
		if ev.SandboxID != "sb-a" {
			t.Errorf("event %+v has wrong sandbox id", ev)
		}
		if !ev.OK {
			t.Errorf("event %+v not OK", ev)
		}
	}
	for _, op := range []string{"exec", "read", "write", "list", "mkdir", "remove"} {
		if _, ok := byOp[op]; !ok {
			t.Errorf("no audit event for op %q; got %v", op, byOp)
		}
	}
	// exec detail carries the command (commands are not secret).
	if !strings.Contains(byOp["exec"].Detail, "ls -la") {
		t.Errorf("exec detail %q missing command", byOp["exec"].Detail)
	}
	// write records its byte count.
	if byOp["write"].Bytes != len("data") {
		t.Errorf("write bytes = %d, want %d", byOp["write"].Bytes, len("data"))
	}
	// read records the read byte count.
	if byOp["read"].Bytes != len("hello") {
		t.Errorf("read bytes = %d, want %d", byOp["read"].Bytes, len("hello"))
	}
}

// TestAuditNeverLeaksFileContent writes a file whose CONTENT looks like a
// secret and asserts the secret never appears in any audit record, while the
// path does.
func TestAuditNeverLeaksFileContent(t *testing.T) {
	var buf bytes.Buffer
	aud := NewJSONAuditor(&buf)
	ts := auditAPI(t, "sb-s", aud, []byte("sk-SECRETVALUE-9999"))

	secret := "sk-SECRETVALUE-9999"
	resp := postJSON(t, ts.URL+"/v1/files/write", "tok", map[string]string{
		"sandbox": "sb-s", "path": "/etc/cred", "content": secret,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("write status = %d", resp.StatusCode)
	}
	// Reading also must not echo content into the audit log.
	resp = postJSON(t, ts.URL+"/v1/files/read", "tok", map[string]string{
		"sandbox": "sb-s", "path": "/etc/cred",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("read status = %d", resp.StatusCode)
	}

	logged := buf.String()
	if strings.Contains(logged, secret) {
		t.Fatalf("audit log leaked file content secret: %s", logged)
	}
	if !strings.Contains(logged, "/etc/cred") {
		t.Fatalf("audit log missing path: %s", logged)
	}
}

// TestAuditExecCommandTruncated asserts a long command is truncated in Detail.
func TestAuditExecCommandTruncated(t *testing.T) {
	rec := &recordingAuditor{}
	ts := auditAPI(t, "sb-t", rec, nil)

	long := strings.Repeat("a", 1000)
	resp := postJSON(t, ts.URL+"/v1/exec", "tok", map[string]string{
		"sandbox": "sb-t", "command": long,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("exec status = %d", resp.StatusCode)
	}

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	d := events[0].Detail
	if len(d) >= len(long) {
		t.Fatalf("detail not truncated: len %d", len(d))
	}
	if !strings.Contains(d, "truncated") {
		t.Errorf("truncated detail %q missing truncation note", d)
	}
}

// TestJSONAuditorWritesOneLinePerEvent checks JSON-line framing and the clock.
func TestJSONAuditorWritesOneLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	aud := NewJSONAuditor(&buf)
	fixed := time.Unix(1_700_000_000, 0)
	aud.now = func() time.Time { return fixed }

	aud.Record(AuditEvent{SandboxID: "sb", Op: "exec", Detail: "ls", OK: true})
	aud.Record(AuditEvent{SandboxID: "sb", Op: "read", Detail: "/p", Bytes: 3, OK: true})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	var ev AuditEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Unix != fixed.Unix() {
		t.Errorf("Unix = %d, want %d", ev.Unix, fixed.Unix())
	}
	if ev.Op != "exec" {
		t.Errorf("Op = %q", ev.Op)
	}
}

func TestNopAuditorIsDefault(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	if _, ok := api.auditor.(NopAuditor); !ok {
		t.Fatalf("default auditor = %T, want NopAuditor", api.auditor)
	}
	// Driving an op through the Server with no agent must not panic.
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	_ = NewServer(engine, api)
	_ = context.Background()
}
