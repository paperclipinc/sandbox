package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/paperclipinc/sandbox/internal/vsock"
)

// test-agent connects to a guest agent via Firecracker vsock UDS
// and runs a series of tests (ping, exec, file write/read, list dir).
// Used by CI to verify the full host→guest data path.
//
// Usage: test-agent <vsock-uds-path>

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: test-agent <vsock-uds-path>")
		os.Exit(1)
	}
	udsPath := os.Args[1]

	// Retry connection with timeout: guest agent may take a few seconds to start
	var client *vsock.Client
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		client, err = vsock.Connect(udsPath, vsock.AgentPort)
		if err == nil {
			break
		}
		fmt.Printf("connect attempt %d failed: %v (retrying in 2s)\n", attempt+1, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed after 10 attempts: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	// Test ping
	uptime, err := client.Ping()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL ping: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PASS ping: uptime=%.2fs\n", uptime)

	// Test exec
	result, err := client.Exec("echo hello from sandbox", "/workspace", nil, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL exec: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PASS exec: exit_code=%d stdout=%q exec_time=%.2fms\n",
		result.ExitCode, result.Stdout, result.ExecTimeMs)

	// Test write + read file
	err = client.WriteFile("/workspace/test.txt", []byte("hello sandbox"), 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL write: %v\n", err)
		os.Exit(1)
	}
	content, err := client.ReadFile("/workspace/test.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL read: %v\n", err)
		os.Exit(1)
	}
	if string(content) != "hello sandbox" {
		fmt.Fprintf(os.Stderr, "FAIL read: expected %q, got %q\n", "hello sandbox", string(content))
		os.Exit(1)
	}
	fmt.Printf("PASS files: wrote and read back %q\n", string(content))

	// Test list dir
	entries, err := client.ListDir("/workspace")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL listdir: %v\n", err)
		os.Exit(1)
	}
	data, _ := json.Marshal(entries)
	fmt.Printf("PASS listdir: %s\n", string(data))

	// Test configure: claim-time env+secrets must reach exec sessions.
	if err := client.Configure(
		map[string]string{"CFG_VAR": "cfg"},
		map[string]string{"TEST_SECRET": "s3cr3t-canary"},
	); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL configure: %v\n", err)
		os.Exit(1)
	}
	result, err = client.Exec(`echo -n "$CFG_VAR:$TEST_SECRET"`, "/workspace", nil, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL exec after configure: %v\n", err)
		os.Exit(1)
	}
	if result.Stdout != "cfg:s3cr3t-canary" {
		fmt.Fprintf(os.Stderr, "FAIL configure: stdout=%q, want cfg:s3cr3t-canary\n", result.Stdout)
		os.Exit(1)
	}
	// Per-request env must override configured values.
	result, err = client.Exec(`echo -n "$CFG_VAR"`, "/workspace", map[string]string{"CFG_VAR": "override"}, 10)
	if err != nil || result.Stdout != "override" {
		fmt.Fprintf(os.Stderr, "FAIL configure precedence: err=%v stdout=%q\n", err, result.Stdout)
		os.Exit(1)
	}
	fmt.Println("PASS configure: env+secrets visible to exec, request overrides configured")

	fmt.Println("")
	fmt.Println("================================")
	fmt.Println("  All guest agent tests passed!")
	fmt.Println("================================")
}
