package vsock

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

var fakeAgentSeq atomic.Int64

// startFakeAgent starts a fake guest agent on a unix socket and returns the
// socket path. Every decoded Request is passed to handler; the returned
// Response is written back. Cleanup is registered on t.
func startFakeAgent(t *testing.T, handler func(req *Request) Response) string {
	t.Helper()

	sockPath := fmt.Sprintf("/tmp/test-agent-%d-%d.sock", os.Getpid(), fakeAgentSeq.Add(1))
	os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
		os.Remove(sockPath)
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
				for scanner.Scan() {
					var req Request
					var resp Response
					if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
						resp = Response{OK: false, Error: err.Error()}
					} else {
						resp = handler(&req)
					}
					data, _ := json.Marshal(resp)
					c.Write(append(data, '\n'))
				}
			}(conn)
		}
	}()

	return sockPath
}

// mockAgentHandler simulates the guest agent's canned responses for testing
// on macOS.
func mockAgentHandler() func(req *Request) Response {
	startTime := time.Now()
	return func(req *Request) Response {
		switch req.Type {
		case TypePing:
			return Response{OK: true, Ping: &PingResponse{Uptime: time.Since(startTime).Seconds()}}
		case TypeExec:
			return Response{OK: true, Exec: &ExecResponse{
				ExitCode:   0,
				Stdout:     fmt.Sprintf("executed: %s\n", req.Exec.Command),
				Stderr:     "",
				ExecTimeMs: 1.0,
			}}
		case TypeReadFile:
			return Response{OK: true, ReadFile: &ReadFileResponse{
				Content: []byte("file content"),
				Size:    12,
			}}
		case TypeWriteFile:
			return Response{OK: true}
		case TypeListDir:
			return Response{OK: true, ListDir: &ListDirResponse{
				Entries: []FileEntry{
					{Name: "test.py", IsDir: false, Size: 100, Mode: 0644},
					{Name: "src", IsDir: true, Size: 0, Mode: 0755},
				},
			}}
		case TypeMkdir:
			return Response{OK: true}
		case TypeRemove:
			return Response{OK: true}
		default:
			return Response{OK: false, Error: "unknown type"}
		}
	}
}

func TestNotifyForked(t *testing.T) {
	var got *NotifyForkedRequest
	before := time.Now().UnixNano()
	sockPath := startFakeAgent(t, func(req *Request) Response {
		if req.Type == TypeNotifyForked {
			got = req.NotifyForked
			return Response{OK: true, NotifyForked: &NotifyForkedResponse{
				AppliedClockStepNanos: 42,
				ReseededRNG:           true,
				SignaledProcesses:     3,
			}}
		}
		return Response{OK: false, Error: "unexpected type"}
	})

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	entropy := []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02}
	resp, err := client.NotifyForked(7, entropy)
	if err != nil {
		t.Fatalf("NotifyForked: %v", err)
	}
	after := time.Now().UnixNano()

	if got == nil {
		t.Fatal("agent received no notify_forked request")
	}
	if got.Generation != 7 {
		t.Errorf("generation = %d, want 7", got.Generation)
	}
	if !bytes.Equal(got.Entropy, entropy) {
		t.Errorf("entropy = %v, want %v", got.Entropy, entropy)
	}
	if got.HostWallClockNanos < before || got.HostWallClockNanos > after {
		t.Errorf("host wall clock %d not in plausible range [%d, %d]",
			got.HostWallClockNanos, before, after)
	}
	if resp == nil || !resp.ReseededRNG || resp.AppliedClockStepNanos != 42 || resp.SignaledProcesses != 3 {
		t.Errorf("response = %+v", resp)
	}
}

func TestNotifyForkedWithNetwork(t *testing.T) {
	var got *NotifyForkedRequest
	sockPath := startFakeAgent(t, func(req *Request) Response {
		if req.Type == TypeNotifyForked {
			got = req.NotifyForked
			return Response{OK: true, NotifyForked: &NotifyForkedResponse{}}
		}
		return Response{OK: false, Error: "unexpected type"}
	})

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	net := &NotifyForkedNetwork{GuestIP: "10.200.0.6", GatewayIP: "10.200.0.5", PrefixLen: 30}
	if _, err := client.NotifyForkedWithNetwork(3, []byte{0x01}, net); err != nil {
		t.Fatalf("NotifyForkedWithNetwork: %v", err)
	}
	if got == nil || got.Network == nil {
		t.Fatal("agent received no network config")
	}
	if *got.Network != *net {
		t.Errorf("network = %+v, want %+v", got.Network, net)
	}
}

func TestClient_Ping(t *testing.T) {
	sockPath := startFakeAgent(t, mockAgentHandler())

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	uptime, err := client.Ping()
	if err != nil {
		t.Fatal(err)
	}
	if uptime < 0 {
		t.Errorf("expected positive uptime, got %f", uptime)
	}
}

func TestClient_Exec(t *testing.T) {
	sockPath := startFakeAgent(t, mockAgentHandler())

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	result, err := client.Exec("echo hello", "/workspace", nil, 10)
	if err != nil {
		t.Fatal(err)
	}

	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Stdout == "" {
		t.Error("expected non-empty stdout")
	}
}

func TestClient_ReadFile(t *testing.T) {
	sockPath := startFakeAgent(t, mockAgentHandler())

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	content, err := client.ReadFile("/workspace/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Error("expected non-empty content")
	}
}

func TestClient_WriteFile(t *testing.T) {
	sockPath := startFakeAgent(t, mockAgentHandler())

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	err = client.WriteFile("/workspace/out.txt", []byte("hello"), 0644)
	if err != nil {
		t.Fatal(err)
	}
}

func TestClient_ListDir(t *testing.T) {
	sockPath := startFakeAgent(t, mockAgentHandler())

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	entries, err := client.ListDir("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Name != "test.py" {
		t.Errorf("expected test.py, got %s", entries[0].Name)
	}
	if !entries[1].IsDir {
		t.Error("expected src to be a directory")
	}
}

func TestClient_Mkdir(t *testing.T) {
	sockPath := startFakeAgent(t, mockAgentHandler())

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.Mkdir("/workspace/newdir"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_Remove(t *testing.T) {
	sockPath := startFakeAgent(t, mockAgentHandler())

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.Remove("/workspace/old.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_MultipleCommands(t *testing.T) {
	sockPath := startFakeAgent(t, mockAgentHandler())

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Send multiple commands on the same connection
	for i := 0; i < 10; i++ {
		_, err := client.Exec(fmt.Sprintf("echo %d", i), "", nil, 5)
		if err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
	}
}

func TestConfigure(t *testing.T) {
	var got *ConfigureRequest
	// fake agent server on a unix socket, same pattern as the other tests in
	// this file: accept, scan lines, unmarshal Request, respond.
	sockPath := startFakeAgent(t, func(req *Request) Response {
		if req.Type == TypeConfigure {
			got = req.Configure
			return Response{OK: true}
		}
		return Response{OK: false, Error: "unexpected type"}
	})

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	err = client.Configure(
		map[string]string{"SESSION": "abc"},
		map[string]string{"API_KEY": "v"},
	)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got == nil || got.Env["SESSION"] != "abc" || got.Secrets["API_KEY"] != "v" {
		t.Fatalf("agent saw %+v", got)
	}
}

func TestTarDirClient(t *testing.T) {
	var gotPath string
	want := []byte("fake-tar-bytes")
	sockPath := startFakeAgent(t, func(req *Request) Response {
		if req.Type != TypeTarDir || req.TarDir == nil {
			return Response{OK: false, Error: "unexpected type"}
		}
		gotPath = req.TarDir.Path
		return Response{OK: true, TarDir: &TarDirResponse{Tar: want}}
	})

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	got, err := client.TarDir("/workspace")
	if err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	if gotPath != "/workspace" {
		t.Fatalf("agent saw path %q", gotPath)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("TarDir returned %q, want %q", got, want)
	}
}

func TestUntarDirClient(t *testing.T) {
	var gotPath string
	var gotTar []byte
	sockPath := startFakeAgent(t, func(req *Request) Response {
		if req.Type != TypeUntarDir || req.UntarDir == nil {
			return Response{OK: false, Error: "unexpected type"}
		}
		gotPath = req.UntarDir.Path
		gotTar = req.UntarDir.Tar
		return Response{OK: true}
	})

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	payload := []byte("tar-payload")
	if err := client.UntarDir("/workspace", payload); err != nil {
		t.Fatalf("UntarDir: %v", err)
	}
	if gotPath != "/workspace" {
		t.Fatalf("agent saw path %q", gotPath)
	}
	if !bytes.Equal(gotTar, payload) {
		t.Fatalf("agent saw tar %q, want %q", gotTar, payload)
	}
}

func TestUntarDirClientRejectsOversize(t *testing.T) {
	client := &Client{}
	err := client.UntarDir("/workspace", make([]byte, MaxTarBytes+1))
	if err == nil {
		t.Fatal("UntarDir accepted an oversize tar; want rejection")
	}
}

// startStallingAgent starts a fake agent that accepts a connection and then
// never sends a response, so a host caller with no read deadline would block
// forever. It returns the socket path.
func startStallingAgent(t *testing.T) string {
	t.Helper()
	sockPath := fmt.Sprintf("/tmp/test-stall-agent-%d-%d.sock", os.Getpid(), fakeAgentSeq.Add(1))
	os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
		os.Remove(sockPath)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without ever writing a response.
			t.Cleanup(func() { conn.Close() })
		}
	}()
	return sockPath
}

// TestSendReadDeadlineUnblocksOnStall proves a malicious or wedged guest that
// connects then never responds causes a one-shot request to return a timeout
// error rather than hang the host caller goroutine indefinitely.
func TestSendReadDeadlineUnblocksOnStall(t *testing.T) {
	sockPath := startStallingAgent(t)
	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetRequestTimeout(150 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := client.Ping()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error from a stalled agent, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ping hung past the read deadline; host caller is not bounded")
	}
}
