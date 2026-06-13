package vsock

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPtyFrameInputRoundTrip(t *testing.T) {
	in := PtyFrame{Kind: PtyInput, Data: []byte("ls -la\n")}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out PtyFrame
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Kind != PtyInput || string(out.Data) != "ls -la\n" {
		t.Fatalf("round trip = %+v", out)
	}
}

func TestPtyFrameResizeAndExit(t *testing.T) {
	rb, _ := json.Marshal(PtyFrame{Kind: PtyResize, Cols: 120, Rows: 40})
	var rz PtyFrame
	if err := json.Unmarshal(rb, &rz); err != nil {
		t.Fatalf("unmarshal resize: %v", err)
	}
	if rz.Cols != 120 || rz.Rows != 40 {
		t.Fatalf("resize = %+v", rz)
	}
	eb, _ := json.Marshal(PtyFrame{Kind: PtyExit, ExitCode: 7})
	var ex PtyFrame
	if err := json.Unmarshal(eb, &ex); err != nil {
		t.Fatalf("unmarshal exit: %v", err)
	}
	if ex.Kind != PtyExit || ex.ExitCode != 7 {
		t.Fatalf("exit = %+v", ex)
	}
}

func TestPtyRequestType(t *testing.T) {
	r := Request{Type: TypePty, Pty: &PtyRequest{Command: "/bin/sh", Cols: 80, Rows: 24}}
	b, _ := json.Marshal(&r)
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != TypePty || got.Pty == nil || got.Pty.Cols != 80 {
		t.Fatalf("req = %+v pty=%+v", got, got.Pty)
	}
}

// fakePtyGuest serves the CONNECT preamble, reads the pty request, echoes any
// input frame back as an output frame, records resize frames, and on receiving
// input "quit\n" emits a terminal exit frame.
func fakePtyGuest(t *testing.T, resizes *[]PtyFrame, mu *sync.Mutex) *StreamConn {
	t.Helper()
	srv, cli := net.Pipe()
	// outgoing decouples the guest's frame writes from its read loop so the
	// synchronous net.Pipe cannot deadlock (a blocked write would otherwise
	// stall the reader that the client is waiting on).
	outgoing := make(chan []byte, 64)
	go func() {
		// Drain every queued frame, then close srv: this guarantees the
		// terminal exit frame reaches the client before the pipe closes.
		defer srv.Close()
		for b := range outgoing {
			if _, err := srv.Write(b); err != nil {
				return
			}
		}
	}()
	go func() {
		defer close(outgoing)
		sc := bufio.NewScanner(srv)
		sc.Buffer(make([]byte, 1<<20), MaxMessageBytes)
		// First line is the pty Request.
		if !sc.Scan() {
			return
		}
		for sc.Scan() {
			var f PtyFrame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				return
			}
			switch f.Kind {
			case PtyResize:
				mu.Lock()
				*resizes = append(*resizes, f)
				mu.Unlock()
			case PtyInput:
				if string(f.Data) == "quit\n" {
					b, _ := json.Marshal(PtyFrame{Kind: PtyExit, ExitCode: 0})
					outgoing <- append(b, '\n')
					return
				}
				b, _ := json.Marshal(PtyFrame{Kind: PtyOutput, Data: f.Data})
				outgoing <- append(b, '\n')
			}
		}
	}()
	return &StreamConn{conn: cli, scanner: bufioScanner(cli)}
}

func bufioScanner(c net.Conn) *bufio.Scanner {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), MaxMessageBytes)
	return sc
}

func TestStreamConnPtyEchoResizeExit(t *testing.T) {
	var resizes []PtyFrame
	var mu sync.Mutex
	sc := fakePtyGuest(t, &resizes, &mu)
	defer sc.Close()

	var got []byte
	var gotMu sync.Mutex
	done := make(chan *PtyFrame, 1)
	go func() {
		exit, err := sc.Pty(context.Background(), &PtyRequest{Command: "/bin/sh", Cols: 80, Rows: 24},
			func(data []byte) error {
				gotMu.Lock()
				got = append(got, data...)
				gotMu.Unlock()
				return nil
			})
		if err != nil {
			t.Errorf("pty: %v", err)
		}
		done <- exit
	}()

	if err := sc.Resize(120, 40); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if err := sc.SendInput([]byte("echo hi\n")); err != nil {
		t.Fatalf("input: %v", err)
	}
	// Wait for the echo to arrive.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gotMu.Lock()
		n := len(got)
		gotMu.Unlock()
		if n >= len("echo hi\n") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := sc.SendInput([]byte("quit\n")); err != nil {
		t.Fatalf("quit: %v", err)
	}

	exit := <-done
	if exit == nil || exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	gotMu.Lock()
	if string(got) != "echo hi\n" {
		t.Fatalf("echo = %q", got)
	}
	gotMu.Unlock()
	mu.Lock()
	if len(resizes) != 1 || resizes[0].Cols != 120 || resizes[0].Rows != 40 {
		t.Fatalf("resizes = %+v", resizes)
	}
	mu.Unlock()
}
