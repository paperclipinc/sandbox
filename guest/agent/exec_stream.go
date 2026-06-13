//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/paperclipinc/mitos/internal/guestenv"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// streamChunkBytes bounds one stdout/stderr read before it is framed. 32 KiB
// keeps a frame small relative to vsock.MaxMessageBytes and flushes output
// promptly to the host.
const streamChunkBytes = 32 << 10

// handleExecStream runs req.Command and writes ExecStreamFrame lines (chunk
// frames per stream, then one exit frame) directly to conn. It is invoked on a
// DEDICATED connection: the whole reply is this stream, so writing many lines
// is safe. The command runs in its own process group so a context cancel
// (connection drop) kills the whole tree.
func handleExecStream(conn net.Conn, req *vsock.ExecRequest) {
	start := time.Now()

	timeout := time.Duration(req.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", req.Command)
	cmd.Dir = req.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = "/workspace"
	}
	// New process group so cancel/timeout kills children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Kill the whole group (negative pid).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	configuredMu.Lock()
	configured := make(map[string]string, len(configuredEnv))
	for k, v := range configuredEnv {
		configured[k] = v
	}
	configuredMu.Unlock()
	cmd.Env = guestenv.Merge(os.Environ(), configured, req.Env)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		writeFrame(conn, vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: 1, Error: fmt.Sprintf("stdout pipe: %v", err)})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		writeFrame(conn, vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: 1, Error: fmt.Sprintf("stderr pipe: %v", err)})
		return
	}

	if err := cmd.Start(); err != nil {
		writeFrame(conn, vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: 1, Error: fmt.Sprintf("start: %v", err)})
		return
	}

	// One write mutex guards all frames so stdout, stderr, and the exit frame
	// never interleave mid-line on the wire.
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	pump := func(r io.Reader, stream vsock.StreamName) {
		defer wg.Done()
		buf := make([]byte, streamChunkBytes)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				writeMu.Lock()
				writeFrame(conn, vsock.ExecStreamFrame{Kind: vsock.FrameChunk, Stream: stream, Data: append([]byte(nil), buf[:n]...)})
				writeMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdoutPipe, vsock.StreamStdout)
	go pump(stderrPipe, vsock.StreamStderr)
	wg.Wait()

	waitErr := cmd.Wait()
	elapsed := time.Since(start)
	exitCode := 0
	if waitErr != nil {
		// Check the deadline first: a timed-out command is SIGKILLed by the
		// cancel below, which surfaces as an ExitError with code -1, so the
		// DeadlineExceeded check must win to report the conventional 124.
		if ctx.Err() == context.DeadlineExceeded {
			exitCode = 124
		} else if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	writeMu.Lock()
	writeFrame(conn, vsock.ExecStreamFrame{
		Kind:       vsock.FrameExit,
		ExitCode:   exitCode,
		ExecTimeMs: float64(elapsed.Microseconds()) / 1000.0,
	})
	writeMu.Unlock()
}

// writeFrame marshals one frame and writes it as a single newline-delimited
// line. A write error means the host hung up; the caller's pumps will end when
// the pipes close.
func writeFrame(conn net.Conn, f vsock.ExecStreamFrame) {
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(b, '\n'))
}
