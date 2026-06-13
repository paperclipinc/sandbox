//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// readFrames drives handleExecStream over an in-process socketpair: it writes
// req as one line, then reads frames until the exit frame.
func readFrames(t *testing.T, req *vsock.ExecRequest) (stdout, stderr string, exit vsock.ExecStreamFrame) {
	t.Helper()
	server, client := net.Pipe()
	go func() {
		defer server.Close()
		handleExecStream(server, req)
	}()
	defer client.Close()

	sc := bufio.NewScanner(client)
	sc.Buffer(make([]byte, 1<<20), vsock.MaxMessageBytes)
	var out, errb strings.Builder
	for sc.Scan() {
		var f vsock.ExecStreamFrame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if f.Kind == vsock.FrameChunk {
			if f.Stream == vsock.StreamStdout {
				out.Write(f.Data)
			} else {
				errb.Write(f.Data)
			}
			continue
		}
		exit = f
		break
	}
	return out.String(), errb.String(), exit
}

func TestHandleExecStreamStdoutStderrExit(t *testing.T) {
	out, errb, exit := readFrames(t, &vsock.ExecRequest{
		Command: "echo out; echo err 1>&2; exit 5",
		// /workspace is the in-VM default but does not exist in CI/test
		// containers; use the test's working dir so cmd.Start() succeeds.
		WorkingDir: t.TempDir(),
		Timeout:    5,
	})
	if !strings.Contains(out, "out") {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(errb, "err") {
		t.Fatalf("stderr = %q", errb)
	}
	if exit.Kind != vsock.FrameExit || exit.ExitCode != 5 {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestHandleExecStreamTimeoutExitCode(t *testing.T) {
	_, _, exit := readFrames(t, &vsock.ExecRequest{Command: "sleep 5", WorkingDir: t.TempDir(), Timeout: 1})
	if exit.ExitCode != 124 {
		t.Fatalf("timeout exit = %d, want 124", exit.ExitCode)
	}
}
