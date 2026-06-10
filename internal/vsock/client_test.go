package vsock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// mockAgent simulates the guest agent for testing on macOS.
func mockAgent(t *testing.T, sockPath string) (net.Listener, func()) {
	t.Helper()
	os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	startTime := time.Now()

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
					if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
						resp, _ := json.Marshal(Response{OK: false, Error: err.Error()})
						c.Write(append(resp, '\n'))
						continue
					}

					var resp Response
					switch req.Type {
					case TypePing:
						resp = Response{OK: true, Ping: &PingResponse{Uptime: time.Since(startTime).Seconds()}}
					case TypeExec:
						resp = Response{OK: true, Exec: &ExecResponse{
							ExitCode:   0,
							Stdout:     fmt.Sprintf("executed: %s\n", req.Exec.Command),
							Stderr:     "",
							ExecTimeMs: 1.0,
						}}
					case TypeReadFile:
						resp = Response{OK: true, ReadFile: &ReadFileResponse{
							Content: []byte("file content"),
							Size:    12,
						}}
					case TypeWriteFile:
						resp = Response{OK: true}
					case TypeListDir:
						resp = Response{OK: true, ListDir: &ListDirResponse{
							Entries: []FileEntry{
								{Name: "test.py", IsDir: false, Size: 100, Mode: 0644},
								{Name: "src", IsDir: true, Size: 0, Mode: 0755},
							},
						}}
					case TypeMkdir:
						resp = Response{OK: true}
					case TypeRemove:
						resp = Response{OK: true}
					default:
						resp = Response{OK: false, Error: "unknown type"}
					}

					data, _ := json.Marshal(resp)
					c.Write(append(data, '\n'))
				}
			}(conn)
		}
	}()

	return listener, func() {
		listener.Close()
		os.Remove(sockPath)
	}
}

func TestClient_Ping(t *testing.T) {
	sockPath := fmt.Sprintf("/tmp/test-agent-%d.sock", os.Getpid())
	_, cleanup := mockAgent(t, sockPath)
	defer cleanup()

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
	sockPath := fmt.Sprintf("/tmp/test-agent-%d.sock", os.Getpid())
	_, cleanup := mockAgent(t, sockPath)
	defer cleanup()

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
	sockPath := fmt.Sprintf("/tmp/test-agent-%d.sock", os.Getpid())
	_, cleanup := mockAgent(t, sockPath)
	defer cleanup()

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
	sockPath := fmt.Sprintf("/tmp/test-agent-%d.sock", os.Getpid())
	_, cleanup := mockAgent(t, sockPath)
	defer cleanup()

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
	sockPath := fmt.Sprintf("/tmp/test-agent-%d.sock", os.Getpid())
	_, cleanup := mockAgent(t, sockPath)
	defer cleanup()

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
	sockPath := fmt.Sprintf("/tmp/test-agent-%d.sock", os.Getpid())
	_, cleanup := mockAgent(t, sockPath)
	defer cleanup()

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
	sockPath := fmt.Sprintf("/tmp/test-agent-%d.sock", os.Getpid())
	_, cleanup := mockAgent(t, sockPath)
	defer cleanup()

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
	sockPath := fmt.Sprintf("/tmp/test-agent-%d.sock", os.Getpid())
	_, cleanup := mockAgent(t, sockPath)
	defer cleanup()

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
