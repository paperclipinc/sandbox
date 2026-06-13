package vsock

import (
	"encoding/json"
	"testing"
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
