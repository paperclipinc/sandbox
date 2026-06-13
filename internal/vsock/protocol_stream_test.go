package vsock

import (
	"encoding/json"
	"testing"
)

func TestExecStreamFrameRoundTrip(t *testing.T) {
	chunk := ExecStreamFrame{Kind: FrameChunk, Stream: StreamStdout, Data: []byte("hello\n")}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	var got ExecStreamFrame
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal chunk: %v", err)
	}
	if got.Kind != FrameChunk || got.Stream != StreamStdout || string(got.Data) != "hello\n" {
		t.Fatalf("chunk round trip mismatch: %+v", got)
	}

	exit := ExecStreamFrame{Kind: FrameExit, ExitCode: 7, ExecTimeMs: 1.5}
	b, err = json.Marshal(exit)
	if err != nil {
		t.Fatalf("marshal exit: %v", err)
	}
	var gotExit ExecStreamFrame
	if err := json.Unmarshal(b, &gotExit); err != nil {
		t.Fatalf("unmarshal exit: %v", err)
	}
	if gotExit.Kind != FrameExit || gotExit.ExitCode != 7 || gotExit.ExecTimeMs != 1.5 {
		t.Fatalf("exit round trip mismatch: %+v", gotExit)
	}
	if TypeExecStream != "exec_stream" {
		t.Fatalf("TypeExecStream = %q", TypeExecStream)
	}
}
