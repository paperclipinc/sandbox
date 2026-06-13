package vsock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

var streamSeq atomic.Int64

// startFakeStreamAgent serves the Firecracker vsock UDS preamble then, for an
// exec_stream request, writes the frames produced by frames(req) one per line.
func startFakeStreamAgent(t *testing.T, frames func(req *ExecRequest) []ExecStreamFrame) string {
	t.Helper()
	sockPath := fmt.Sprintf("/tmp/test-stream-agent-%d-%d.sock", os.Getpid(), streamSeq.Add(1))
	os.Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close(); os.Remove(sockPath) })

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), MaxMessageBytes)
				// Firecracker UDS preamble: read "CONNECT <port>\n", reply "OK <port>\n".
				if !sc.Scan() {
					return
				}
				line := sc.Text()
				if strings.HasPrefix(line, "CONNECT ") {
					fmt.Fprintf(c, "OK %s\n", strings.TrimPrefix(line, "CONNECT "))
				}
				if !sc.Scan() {
					return
				}
				var req Request
				if err := json.Unmarshal(sc.Bytes(), &req); err != nil || req.ExecStream == nil {
					return
				}
				for _, f := range frames(req.ExecStream) {
					b, _ := json.Marshal(f)
					c.Write(append(b, '\n'))
				}
			}(conn)
		}
	}()
	return sockPath
}

func TestExecStreamYieldsChunksThenExit(t *testing.T) {
	sock := startFakeStreamAgent(t, func(req *ExecRequest) []ExecStreamFrame {
		return []ExecStreamFrame{
			{Kind: FrameChunk, Stream: StreamStdout, Data: []byte("out1")},
			{Kind: FrameChunk, Stream: StreamStderr, Data: []byte("err1")},
			{Kind: FrameChunk, Stream: StreamStdout, Data: []byte("out2")},
			{Kind: FrameExit, ExitCode: 3, ExecTimeMs: 4.2},
		}
	})
	c, err := DialStreamUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var out, errb strings.Builder
	exit, err := c.ExecStream(context.Background(), &ExecRequest{Command: "x"}, func(stream StreamName, data []byte) error {
		if stream == StreamStdout {
			out.Write(data)
		} else {
			errb.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if out.String() != "out1out2" || errb.String() != "err1" {
		t.Fatalf("got out=%q err=%q", out.String(), errb.String())
	}
	if exit.ExitCode != 3 || exit.ExecTimeMs != 4.2 {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestExecAggregatesStream(t *testing.T) {
	sock := startFakeStreamAgent(t, func(req *ExecRequest) []ExecStreamFrame {
		return []ExecStreamFrame{
			{Kind: FrameChunk, Stream: StreamStdout, Data: []byte("hello\n")},
			{Kind: FrameChunk, Stream: StreamStderr, Data: []byte("warn\n")},
			{Kind: FrameExit, ExitCode: 0, ExecTimeMs: 1.0},
		}
	})
	c, err := DialStreamUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Exec("echo hello", "/workspace", nil, 5)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Stdout != "hello\n" || resp.Stderr != "warn\n" || resp.ExitCode != 0 {
		t.Fatalf("aggregate = %+v", resp)
	}
}
