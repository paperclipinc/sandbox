//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// runPty drives handlePtyStream over an in-process pipe: it returns a writer to
// send frames to the guest and a channel of frames the guest emits.
func runPty(t *testing.T, req *vsock.PtyRequest) (send func(vsock.PtyFrame), frames <-chan vsock.PtyFrame, closeConn func()) {
	t.Helper()
	server, client := net.Pipe()
	go func() {
		defer server.Close()
		sc := bufio.NewScanner(server)
		sc.Buffer(make([]byte, 1<<20), vsock.MaxMessageBytes)
		handlePtyStream(server, sc, req)
	}()

	out := make(chan vsock.PtyFrame, 256)
	go func() {
		sc := bufio.NewScanner(client)
		sc.Buffer(make([]byte, 1<<20), vsock.MaxMessageBytes)
		for sc.Scan() {
			var f vsock.PtyFrame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				return
			}
			out <- f
		}
		close(out)
	}()

	send = func(f vsock.PtyFrame) {
		b, _ := json.Marshal(f)
		if _, err := client.Write(append(b, '\n')); err != nil {
			t.Errorf("write frame: %v", err)
		}
	}
	return send, out, func() { client.Close() }
}

func TestHandlePtyEchoAndExit(t *testing.T) {
	send, frames, closeConn := runPty(t, &vsock.PtyRequest{
		Command:    "/bin/sh",
		WorkingDir: t.TempDir(),
		Cols:       80,
		Rows:       24,
	})
	defer closeConn()

	// Type a command, then exit the shell.
	send(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("echo pty_marker_42\n")})

	var collected strings.Builder
	exitSent := false
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("stream closed before marker; got %q", collected.String())
			}
			if f.Kind == vsock.PtyOutput {
				collected.Write(f.Data)
				// The marker shows up in several output frames (the terminal echo
				// of the typed line and the command output); send exit exactly
				// once so a late write does not race the post-exit pipe close.
				if !exitSent && strings.Contains(collected.String(), "pty_marker_42") {
					exitSent = true
					send(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("exit\n")})
				}
			}
			if f.Kind == vsock.PtyExit {
				return // success: shell exited and emitted a terminal frame
			}
		case <-deadline:
			t.Fatalf("timeout; collected %q", collected.String())
		}
	}
}

// TestHandleConnectionPtyCoalescedInput drives the REAL handleConnection
// dispatch path (not handlePtyStream directly) and writes the PTY open request
// AND a first input frame in a SINGLE write to the conn. bufio.Scanner reads in
// chunks, so the dispatcher's outer scanner buffers both lines; the input frame
// must still reach the PTY (it is not dropped by a fresh scanner). The input
// echoes a unique marker which must appear in the guest output.
func TestHandleConnectionPtyCoalescedInput(t *testing.T) {
	server, client := net.Pipe()
	go func() {
		defer server.Close()
		handleConnection(server)
	}()

	// Collect guest output frames.
	out := make(chan vsock.PtyFrame, 256)
	go func() {
		sc := bufio.NewScanner(client)
		sc.Buffer(make([]byte, 1<<20), vsock.MaxMessageBytes)
		for sc.Scan() {
			var f vsock.PtyFrame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				return
			}
			out <- f
		}
		close(out)
	}()

	// Build the open request line and the first input frame, then write BOTH in
	// a single Write so they coalesce into one read on the guest side.
	openReq, _ := json.Marshal(vsock.Request{
		Type: vsock.TypePty,
		Pty:  &vsock.PtyRequest{Command: "/bin/sh", WorkingDir: t.TempDir(), Cols: 80, Rows: 24},
	})
	inputFrame, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("echo coalesced_marker_99\n")})
	var batch []byte
	batch = append(batch, openReq...)
	batch = append(batch, '\n')
	batch = append(batch, inputFrame...)
	batch = append(batch, '\n')
	if _, err := client.Write(batch); err != nil {
		t.Fatalf("write batch: %v", err)
	}

	send := func(f vsock.PtyFrame) {
		b, _ := json.Marshal(f)
		if _, err := client.Write(append(b, '\n')); err != nil {
			t.Errorf("write frame: %v", err)
		}
	}

	var collected strings.Builder
	exitSent := false
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-out:
			if !ok {
				t.Fatalf("stream closed before coalesced marker; got %q", collected.String())
			}
			if f.Kind == vsock.PtyOutput {
				collected.Write(f.Data)
				if !exitSent && strings.Contains(collected.String(), "coalesced_marker_99") {
					exitSent = true
					send(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("exit\n")})
				}
			}
			if f.Kind == vsock.PtyExit {
				if !strings.Contains(collected.String(), "coalesced_marker_99") {
					t.Fatalf("coalesced input frame was dropped; output %q", collected.String())
				}
				client.Close()
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for coalesced marker; collected %q", collected.String())
		}
	}
}

func TestHandlePtyResizeNoCrash(t *testing.T) {
	send, frames, closeConn := runPty(t, &vsock.PtyRequest{
		Command:    "/bin/sh",
		WorkingDir: t.TempDir(),
		Cols:       80,
		Rows:       24,
	})
	defer closeConn()

	send(vsock.PtyFrame{Kind: vsock.PtyResize, Cols: 132, Rows: 50})
	// stty size should report the new rows/cols (busybox/coreutils stty).
	send(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("stty size; exit\n")})

	var collected strings.Builder
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return // shell exited; resize did not crash the pump
			}
			if f.Kind == vsock.PtyOutput {
				collected.Write(f.Data)
			}
			if f.Kind == vsock.PtyExit {
				return
			}
		case <-deadline:
			t.Fatalf("timeout; collected %q", collected.String())
		}
	}
}
