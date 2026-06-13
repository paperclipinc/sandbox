package vsock

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
)

// TestRunCodeClientStreamsFrames drives StreamConn.RunCode against a one-shot
// fake agent on the other end of a net.Pipe: it reads the request line and
// replies with canned NDJSON frames, then closes.
func TestRunCodeClientStreamsFrames(t *testing.T) {
	c1, c2 := net.Pipe()
	sc := &StreamConn{conn: c1, scanner: bufio.NewScanner(c1)}

	go func() {
		br := bufio.NewReader(c2)
		_, _ = br.ReadString('\n')
		frames := []string{
			`{"kind":"chunk","stream":"stdout","data":"aGk="}`,
			`{"kind":"result","result":{"text":"42","data":{"text/plain":"42"}}}`,
			`{"kind":"exit","exit_code":0}`,
		}
		for _, f := range frames {
			_, _ = c2.Write([]byte(f + "\n"))
		}
		c2.Close()
	}()

	var got []ExecStreamFrame
	err := sc.RunCode(context.Background(), &RunCodeRequest{Code: "print(42)", Language: "python", Timeout: 30}, func(fr ExecStreamFrame) {
		got = append(got, fr)
	})
	if err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3: %+v", len(got), got)
	}
	if got[1].Kind != FrameResult || got[1].Result == nil || got[1].Result.Text != "42" {
		t.Fatalf("frame 1 not result: %+v", got[1])
	}
	if got[2].Kind != FrameExit || got[2].ExitCode != 0 {
		t.Fatalf("frame 2 not exit: %+v", got[2])
	}
}

func TestRunCodeRequestRoundTrip(t *testing.T) {
	req := Request{
		Type: TypeRunCode,
		RunCode: &RunCodeRequest{
			Code:     "print(1)",
			Language: "python",
			Timeout:  30,
		},
	}
	b, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != TypeRunCode {
		t.Fatalf("type = %q, want %q", got.Type, TypeRunCode)
	}
	if got.RunCode == nil || got.RunCode.Code != "print(1)" || got.RunCode.Language != "python" {
		t.Fatalf("run_code payload not preserved: %+v", got.RunCode)
	}
}

func TestExecStreamFrameResultAndError(t *testing.T) {
	resFrame := ExecStreamFrame{
		Kind: FrameResult,
		Result: &ResultFrame{
			Text: "42",
			Data: map[string]string{"image/png": "aGVsbG8=", "text/plain": "42"},
		},
	}
	b, err := json.Marshal(&resFrame)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var gotRes ExecStreamFrame
	if err := json.Unmarshal(b, &gotRes); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if gotRes.Result == nil || gotRes.Result.Data["image/png"] != "aGVsbG8=" || gotRes.Result.Text != "42" {
		t.Fatalf("result frame not preserved: %+v", gotRes.Result)
	}

	errFrame := ExecStreamFrame{
		Kind: FrameError,
		ErrorInfo: &ErrorFrame{
			Name:      "ValueError",
			Value:     "bad",
			Traceback: []string{"Traceback (most recent call last):", "ValueError: bad"},
		},
	}
	b, err = json.Marshal(&errFrame)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var gotErr ExecStreamFrame
	if err := json.Unmarshal(b, &gotErr); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if gotErr.ErrorInfo == nil || gotErr.ErrorInfo.Name != "ValueError" || len(gotErr.ErrorInfo.Traceback) != 2 {
		t.Fatalf("error frame not preserved: %+v", gotErr.ErrorInfo)
	}
}
