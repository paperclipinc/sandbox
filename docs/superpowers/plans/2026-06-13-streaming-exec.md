# Streaming Exec Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `exec` stream stdout/stderr incrementally end to end (guest agent to SDK) and support background/long-running processes, while keeping the existing one-shot blocking `exec` working by aggregating the same stream.

**Architecture:** The guest agent runs the command with `cmd.StdoutPipe()`/`cmd.StderrPipe()` and writes a sequence of newline-delimited JSON frames over the existing vsock UDS: zero or more `chunk` frames (`{stream, data}`) followed by exactly one `exit` frame (`{exit_code, error, exec_time_ms}`). A streaming exec uses a DEDICATED vsock connection so its multi-frame reply never interleaves with the shared connection's one-frame request/response calls. The host (`vsock.Client.ExecStream`) reads frames and invokes a callback per chunk, returning the final exit. forkd exposes `POST /v1/exec/stream` as chunked NDJSON (one JSON object per line); the existing `POST /v1/exec` is re-expressed by consuming the same stream and aggregating into the existing `ExecResponse`. Background processes are modeled as a stream the client can stop by closing the HTTP response (which cancels the guest `context`, killing the process group). The Python and TypeScript SDKs gain `exec(..., on_stdout=, on_stderr=)` callbacks plus a background handle with `wait()`/`kill()`, keeping the blocking `exec` intact.

**Tech Stack:** Go (guest agent linux-only, host vsock client, net/http chunked transfer), Python (httpx streaming), TypeScript (fetch + ReadableStream), JSON line framing over vsock UDS.

---

## Design decisions (locked, not deferred)

These are fixed for the whole plan. Every task below assumes them.

### 1. vsock streaming framing

The vsock channel is newline-delimited JSON (`internal/vsock/protocol.go:3-4`; guest reads with `bufio.Scanner` in `guest/agent/main.go:104-119`; host reads one line per `send` in `internal/vsock/client.go:77-92`). Streaming keeps the SAME line framing but sends MULTIPLE response lines for one request:

- A new request type `TypeExecStream = "exec_stream"` carrying the existing `ExecRequest` payload shape.
- The guest replies with a sequence of `ExecStreamFrame` JSON objects, ONE PER LINE:
  - chunk frame: `{"kind":"chunk","stream":"stdout"|"stderr","data":<base64 bytes>}`
  - terminal frame: `{"kind":"exit","exit_code":<int>,"error":"<string>","exec_time_ms":<float>}`
- Exactly one `exit` frame is sent, and it is always last. The host reads lines until it sees `kind:"exit"`.
- `data` is `[]byte` in Go (JSON-base64 encoded), so binary stdout survives intact and no newline inside the data can be confused with a frame boundary.

Why a separate request type and frame type rather than reusing `Response`: `Response` is the one-shot envelope the shared connection expects exactly once per `send`. A streaming reply emits many lines; mixing that into `Response` on the shared connection would desynchronize the scanner for any concurrent one-shot call. `ExecStreamFrame` is a distinct, self-delimiting type carried on a DEDICATED connection (see decision 4).

Frame size: each chunk is bounded by the guest read buffer (32 KiB, below). The existing `vsock.MaxMessageBytes` (96 MiB, `internal/vsock/protocol.go:39`) line cap is far above a base64-encoded 32 KiB chunk (~44 KiB), so no buffer change is needed.

### 2. HTTP transport: chunked NDJSON (not SSE)

forkd's `POST /v1/exec/stream` streams **newline-delimited JSON** (one JSON object per line) over HTTP chunked transfer encoding (`Content-Type: application/x-ndjson`). Each line is a frame:
```
{"stream":"stdout","data":"aGVsbG8K"}
{"stream":"stderr","data":"d2Fybgo="}
{"exit_code":0,"exec_time_ms":12.3}
```
`data` is base64 (JSON cannot hold raw bytes). The terminal line has no `stream` field and carries `exit_code` (plus `error` on transport/spawn failure).

Why NDJSON over SSE: (a) it is a 1:1 mapping of the vsock frame stream, so forkd is a pure proxy with no reframing; (b) base64 `data` already escapes newlines, so a line is always one frame, which SSE's `data:`/blank-line framing would only duplicate; (c) both target SDKs read it with a plain line reader over the response body (`httpx.Client.stream` in Python, `response.body.getReader()` in TS) with no SSE parser dependency; (d) it is symmetric with the existing JSON-everywhere wire style. The handler sets `Content-Type: application/x-ndjson`, calls `http.NewResponseController(w).Flush()` (Go 1.20+) after each line so chunks arrive incrementally.

### 3. Background / long-running process model

A background process is just a stream the caller does not drain to completion synchronously:
- Start: SDK opens `POST /v1/exec/stream` and gets a handle holding the live HTTP response.
- Kill: the SDK CLOSES the HTTP response body. forkd detects `r.Context().Done()` on the request, which cancels the guest-bound vsock stream; the guest runs the command under `exec.CommandContext` with a cancel, and on context cancel kills the **process group** (`Setpgid` + `syscall.Kill(-pgid, SIGKILL)`) so children die too. No separate kill RPC or process-id registry is introduced in THIS plan (that arrives with the PTY plan, follow-up 2).
- Wait: the SDK background handle exposes `wait()` which drains the stream to the `exit` frame and returns the aggregate `ExecResult`.
- Timeout still applies: `ExecRequest.Timeout` bounds the guest `context`; 0 means the streaming default (no timeout for background use is expressed by passing a large timeout, documented in the SDK).

### 4. Ordering, flush, and connection isolation

- Ordering: stdout and stderr are independent OS pipes; the guest copies each with its own goroutine, so cross-stream ordering is NOT guaranteed (same as every competitor). Within ONE stream, bytes are in order because a single goroutine drains that pipe and writes frames under a mutex that also guards the terminal frame.
- Flush: the guest writes each frame with a single `conn.Write` of `json + '\n'`; forkd flushes the HTTP writer after each line. No host-side buffering between guest and HTTP client beyond one frame.
- Connection isolation: `ExecStream` does NOT use the shared `Client.conn`. The `SandboxAPI` opens a fresh dedicated vsock connection per stream (via a new `vsock.DialStream` that performs the Firecracker `CONNECT` preamble and returns a `*StreamConn`), so a long-running stream never blocks or desynchronizes one-shot `Ping`/`Exec`/file calls on the shared connection. The dedicated connection is closed when the stream ends or the HTTP client disconnects.

### 5. One-shot exec re-expressed on the stream

`vsock.Client.Exec` (host) keeps its signature and return type but is reimplemented to call `ExecStream` internally, aggregating chunks into `stdout`/`stderr` strings and returning `*ExecResponse`. The guest's `TypeExec` handler is LEFT IN PLACE unchanged (so old hosts still work and the change is additive), but the host stops calling it; the guest gains a `TypeExecStream` handler. forkd's `POST /v1/exec` is reimplemented to consume `ExecStream` and aggregate, returning the identical JSON `ExecResponse` shape (no SDK-visible change for blocking callers).

---

## File Structure

- `internal/vsock/protocol.go` (modify): add `TypeExecStream`, `ExecStreamFrame`, and frame kind/stream constants.
- `internal/vsock/client.go` (modify): add `DialStream`/`StreamConn`, `ExecStream(ctx, req, onChunk)`, reimplement `Exec` on top of `ExecStream`.
- `internal/vsock/stream_test.go` (create): host-side framing + aggregation tests against a fake streaming agent.
- `guest/agent/exec_stream.go` (create): `handleExecStream` streaming the command over pipes with process-group kill on cancel.
- `guest/agent/main.go` (modify): wire `TypeExecStream` into `handleRequest`; streaming needs the raw `net.Conn`, so add a connection-aware dispatch branch.
- `guest/agent/exec_stream_test.go` (create, linux build tag): guest streaming unit test over a unix socket.
- `internal/daemon/sandbox_api.go` (modify): add `handleExecStream` (NDJSON), reimplement `handleExec` to aggregate the stream, register `POST /v1/exec/stream`.
- `internal/daemon/exec_stream_test.go` (create): HTTP NDJSON handler tests + auth-gating test for the stream endpoint.
- `cmd/sandbox-server/main.go` (modify): route `POST /v1/exec/stream` through the same `SandboxAPI` handler.
- `sdk/python/mitos/sandbox.py` (modify): add `exec(..., on_stdout=, on_stderr=)` streaming and `exec_background(...)` returning a `BackgroundProcess`.
- `sdk/python/mitos/types.py` (modify): add `BackgroundProcess` dataclass/handle.
- `sdk/python/tests/test_stream.py` (create): streaming + background tests against a stub NDJSON server.
- `sdk/typescript/src/sandbox.ts` (modify): add `exec(..., {onStdout, onStderr})` and `execBackground(...)` returning a `BackgroundProcess`.
- `sdk/typescript/src/http.ts` (modify): add `postStream` returning the raw `Response` for line reading.
- `sdk/typescript/src/types.ts` (modify): add `BackgroundProcess` interface.
- `sdk/typescript/test/stream.test.ts` (create): streaming + background tests against a mock fetch.
- `proto/forkd.proto` (modify): document that `ExecStream` gRPC stays HTTP-served; align `ExecStreamRequest`/`ExecStreamResponse` comments.
- `docs/threat-model.md` (modify): add the exec-stream surface row.

---

## Task 1: vsock streaming protocol types

**Files:**
- Modify: `internal/vsock/protocol.go:8-20` (request type consts) and after `ExecResponse` (`internal/vsock/protocol.go:198-203`)

- [ ] **Step 1: Write the failing test**

Create `internal/vsock/protocol_stream_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/vsock/ -run TestExecStreamFrameRoundTrip -v`
Expected: FAIL, `undefined: ExecStreamFrame` / `undefined: TypeExecStream`.

- [ ] **Step 3: Add the types**

In `internal/vsock/protocol.go`, add to the `const (...)` request-type block (after `TypeUntarDir`, around line 19):

```go
	TypeExecStream   RequestType = "exec_stream"
```

After `ExecResponse` (after line 203), add:

```go
// FrameKind tags an ExecStreamFrame as a data chunk or the terminal exit.
type FrameKind string

const (
	FrameChunk FrameKind = "chunk"
	FrameExit  FrameKind = "exit"
)

// StreamName identifies which standard stream a chunk came from.
type StreamName string

const (
	StreamStdout StreamName = "stdout"
	StreamStderr StreamName = "stderr"
)

// ExecStreamFrame is one newline-delimited JSON line in a streaming exec reply.
// The guest emits zero or more FrameChunk frames (each carrying a slice of one
// stream's bytes) followed by exactly one FrameExit frame. Data is a []byte so
// the JSON encoder base64s it: binary output survives and no embedded newline
// is mistaken for a frame boundary. The stream uses a dedicated vsock
// connection, so these multi-line replies never interleave with the shared
// connection's one-shot Response calls.
type ExecStreamFrame struct {
	Kind       FrameKind  `json:"kind"`
	Stream     StreamName `json:"stream,omitempty"`
	Data       []byte     `json:"data,omitempty"`
	ExitCode   int        `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	ExecTimeMs float64    `json:"exec_time_ms,omitempty"`
}
```

Also add the `ExecStream` request to the `Request` struct (it reuses `ExecRequest`). In the `Request` struct (lines 41-53), add after the `Exec` field:

```go
	ExecStream   *ExecRequest         `json:"exec_stream,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/vsock/ -run TestExecStreamFrameRoundTrip -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/vsock/protocol.go internal/vsock/protocol_stream_test.go
git commit -m "feat: add vsock exec-stream frame protocol types

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Host vsock ExecStream client over a dedicated connection

**Files:**
- Modify: `internal/vsock/client.go` (add `DialStream`, `StreamConn`, `ExecStream`; reimplement `Exec`)
- Create: `internal/vsock/stream_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/vsock/stream_test.go`:

```go
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
	c, err := dialStreamUnix(sock)
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
	c, err := dialStreamUnix(sock)
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
```

Note: `dialStreamUnix` is a test-only helper added to `client.go` so the unix-socket fake exercises the same `StreamConn` read path as the Firecracker UDS dial.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/vsock/ -run 'TestExecStream|TestExecAggregates' -v`
Expected: FAIL, `c.ExecStream undefined`, `dialStreamUnix undefined`.

- [ ] **Step 3: Implement DialStream, StreamConn, ExecStream, and reimplement Exec**

In `internal/vsock/client.go`, add imports `"context"` and `"strings"` to the import block, then append:

```go
// StreamConn is a DEDICATED vsock connection for one streaming exec. It is kept
// separate from Client.conn so a long-running stream never interleaves with the
// shared connection's one-shot Response calls (Ping, file ops, aggregated Exec).
type StreamConn struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

// DialStream opens a fresh vsock connection to the guest agent for one
// streaming exec, performing the Firecracker UDS CONNECT preamble. The caller
// must Close it when the stream ends or its HTTP client disconnects.
func DialStream(udsPath string, guestPort int) (*StreamConn, error) {
	conn, err := net.DialTimeout("unix", udsPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial stream vsock UDS: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", guestPort))); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
	if !sc.Scan() {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: no response")
	}
	if resp := sc.Text(); len(resp) < 2 || resp[:2] != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %s", resp)
	}
	return &StreamConn{conn: conn, scanner: sc}, nil
}

// dialStreamUnix dials a plain unix socket that already speaks the CONNECT
// preamble (used by tests and the standalone unix-fallback path).
func dialStreamUnix(sockPath string) (*StreamConn, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial stream unix: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", AgentPort))); err != nil {
		conn.Close()
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
	if !sc.Scan() {
		conn.Close()
		return nil, fmt.Errorf("stream unix: no preamble response")
	}
	return &StreamConn{conn: conn, scanner: sc}, nil
}

// Close shuts the dedicated stream connection. Closing it while the guest is
// still running cancels the guest exec (the guest sees the connection drop).
func (s *StreamConn) Close() error {
	return s.conn.Close()
}

// ChunkFunc receives one stream's bytes as they arrive. Returning a non-nil
// error stops the stream early (the caller should then Close the StreamConn).
type ChunkFunc func(stream StreamName, data []byte) error

// ExecStream runs command on the guest and invokes onChunk for each stdout or
// stderr chunk as it arrives, returning the terminal ExecStreamFrame (exit
// code, exec time, and any spawn error). The request is sent once; frames are
// read until the FrameExit line. If ctx is cancelled the connection is closed,
// which the guest observes and uses to kill the process group.
func (s *StreamConn) ExecStream(ctx context.Context, req *ExecRequest, onChunk ChunkFunc) (*ExecStreamFrame, error) {
	data, err := json.Marshal(&Request{Type: TypeExecStream, ExecStream: req})
	if err != nil {
		return nil, err
	}
	if _, err := s.conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("send exec_stream: %w", err)
	}

	// Closing the connection on ctx cancel unblocks the scanner below.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.conn.Close()
		case <-done:
		}
	}()

	for s.scanner.Scan() {
		var f ExecStreamFrame
		if err := json.Unmarshal(s.scanner.Bytes(), &f); err != nil {
			return nil, fmt.Errorf("decode exec_stream frame: %w", err)
		}
		switch f.Kind {
		case FrameChunk:
			if err := onChunk(f.Stream, f.Data); err != nil {
				return nil, err
			}
		case FrameExit:
			return &f, nil
		default:
			return nil, fmt.Errorf("unknown exec_stream frame kind: %q", f.Kind)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, fmt.Errorf("recv exec_stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("exec_stream: connection closed before exit frame")
}
```

Now reimplement `Client.Exec` (replace the existing body at `internal/vsock/client.go:103-117`) so one-shot exec rides the stream. Because `Client` holds only the shared connection and the streaming path needs a dedicated connection, `Exec` keeps using the shared connection's `send` for backward compatibility ONLY when no dedicated dial is available; here we keep `Exec` calling the guest's still-present `TypeExec` handler to avoid opening a second connection for every blocking call. KEEP the existing `Exec` body as-is. The host-side stream aggregation that the test `TestExecAggregates` exercises lives on `StreamConn`, so add this method on `StreamConn`:

```go
// Exec runs command to completion over the stream and returns the aggregated
// stdout/stderr and exit code, matching the one-shot ExecResponse shape. It is
// the streaming-native equivalent of Client.Exec and is what the HTTP /v1/exec
// handler uses so blocking and streaming share one guest code path.
func (s *StreamConn) Exec(command, workingDir string, env map[string]string, timeout int) (*ExecResponse, error) {
	var out, errb strings.Builder
	exit, err := s.ExecStream(context.Background(), &ExecRequest{
		Command:    command,
		WorkingDir: workingDir,
		Env:        env,
		Timeout:    timeout,
	}, func(stream StreamName, data []byte) error {
		if stream == StreamStdout {
			out.Write(data)
		} else {
			errb.Write(data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if exit.Error != "" {
		return nil, fmt.Errorf("exec_stream: %s", exit.Error)
	}
	return &ExecResponse{
		ExitCode:   exit.ExitCode,
		Stdout:     out.String(),
		Stderr:     errb.String(),
		ExecTimeMs: exit.ExecTimeMs,
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/vsock/ -run 'TestExecStream|TestExecAggregates' -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/vsock/client.go internal/vsock/stream_test.go
git commit -m "feat: add host vsock ExecStream over a dedicated connection

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Guest agent streaming exec handler

**Files:**
- Create: `guest/agent/exec_stream.go`
- Modify: `guest/agent/main.go:101-120` (connection dispatch), `guest/agent/main.go:122-199` (request switch)
- Create: `guest/agent/exec_stream_test.go`

- [ ] **Step 1: Write the failing test**

Create `guest/agent/exec_stream_test.go`:

```go
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
		Timeout: 5,
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
	_, _, exit := readFrames(t, &vsock.ExecRequest{Command: "sleep 5", Timeout: 1})
	if exit.ExitCode != 124 {
		t.Fatalf("timeout exit = %d, want 124", exit.ExitCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOOS=linux GOARCH=amd64 go test ./guest/agent/ -run TestHandleExecStream` (on darwin this only compiles under linux build tag; if not on linux run the compile check)
Run alt: `GOOS=linux GOARCH=amd64 go vet ./guest/agent/`
Expected: FAIL/compile error, `undefined: handleExecStream`.

- [ ] **Step 3: Implement handleExecStream**

Create `guest/agent/exec_stream.go`:

```go
//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	cmd.Env = guestenv.Merge(envEnviron(), configured, req.Env)

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
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			exitCode = 124
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

// envEnviron is os.Environ wrapped so the test can keep it tiny without
// importing os everywhere; it returns the live process environment.
func envEnviron() []string { return execEnviron() }

// drainPreamble is unused here; streaming dispatch reads the single request
// line in handleConnection before calling handleExecStream.
var _ = bufio.NewScanner
```

Add to `guest/agent/main.go` near the top-level helpers a tiny shim so `execEnviron` resolves (it wraps `os.Environ`); add this function in `main.go` (it already imports `os`):

```go
// execEnviron returns the current process environment. It exists as an
// indirection so exec_stream.go does not import os directly.
func execEnviron() []string { return os.Environ() }
```

- [ ] **Step 4: Wire TypeExecStream into the connection dispatch**

The streaming handler needs the raw `net.Conn`. Modify `handleConnection` in `guest/agent/main.go:110-119` so an `exec_stream` request is dispatched with the connection and consumes the rest of that connection (a stream owns its connection):

```go
	for scanner.Scan() {
		var req vsock.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			writeResponse(conn, vsock.Response{OK: false, Error: fmt.Sprintf("invalid request: %v", err)})
			continue
		}

		if req.Type == vsock.TypeExecStream {
			if req.ExecStream == nil {
				writeResponse(conn, vsock.Response{OK: false, Error: "exec_stream request is nil"})
				return
			}
			// A stream owns its connection: write frames, then close.
			handleExecStream(conn, req.ExecStream)
			return
		}

		resp := handleRequest(&req)
		writeResponse(conn, resp)
	}
```

(The existing `TypeExec` case in `handleRequest` stays untouched for backward compatibility.)

- [ ] **Step 5: Run test to verify it passes**

Run: `GOOS=linux GOARCH=amd64 go test ./guest/agent/ -run TestHandleExecStream -v`
(If the dev box cannot run linux binaries, this is covered by the KVM CI verification at the end; at minimum run `GOOS=linux GOARCH=amd64 go build ./guest/agent/` and `GOOS=linux GOARCH=amd64 go vet ./guest/agent/` and confirm they pass.)
Expected: PASS (both subtests) or clean build+vet.

- [ ] **Step 6: Commit**

```bash
git add guest/agent/exec_stream.go guest/agent/exec_stream_test.go guest/agent/main.go
git commit -m "feat: stream guest exec stdout/stderr over vsock with pgroup kill

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: forkd HTTP NDJSON stream endpoint + aggregated /v1/exec

**Files:**
- Modify: `internal/daemon/sandbox_api.go` (add `vsockDir`-based stream dial, `handleExecStream`, reimplement `handleExec`, register route)
- Create: `internal/daemon/exec_stream_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/exec_stream_test.go`:

```go
package daemon

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// startFakeStreamUDS serves the Firecracker UDS preamble then writes the given
// frames for any exec_stream request.
func startFakeStreamUDS(t *testing.T, sockPath string, frames []vsock.ExecStreamFrame) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), vsock.MaxMessageBytes)
				if !sc.Scan() {
					return
				}
				if strings.HasPrefix(sc.Text(), "CONNECT ") {
					c.Write([]byte("OK 52\n"))
				}
				if !sc.Scan() {
					return
				}
				for _, f := range frames {
					b, _ := json.Marshal(f)
					c.Write(append(b, '\n'))
				}
			}(conn)
		}
	}()
}

func newStreamAPI(t *testing.T) (*SandboxAPI, string) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "sb1", "vsock.sock")
	startFakeStreamUDS(t, sock, []vsock.ExecStreamFrame{
		{Kind: vsock.FrameChunk, Stream: vsock.StreamStdout, Data: []byte("hello\n")},
		{Kind: vsock.FrameChunk, Stream: vsock.StreamStderr, Data: []byte("warn\n")},
		{Kind: vsock.FrameExit, ExitCode: 0, ExecTimeMs: 1.0},
	})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	// Register the shared connection AND record the stream UDS path.
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	return api, sock
}

func TestHandleExecStreamNDJSON(t *testing.T) {
	api, _ := newStreamAPI(t)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb1", "command": "echo hello"})
	resp, err := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	var sawExit bool
	var out string
	for sc.Scan() {
		var line struct {
			Stream   string `json:"stream"`
			Data     string `json:"data"`
			ExitCode *int   `json:"exit_code"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bad ndjson line %q: %v", sc.Text(), err)
		}
		if line.ExitCode != nil {
			sawExit = true
			if *line.ExitCode != 0 {
				t.Fatalf("exit code = %d", *line.ExitCode)
			}
			continue
		}
		if line.Stream == "stdout" {
			d, _ := base64.StdEncoding.DecodeString(line.Data)
			out += string(d)
		}
	}
	if !sawExit {
		t.Fatal("no exit line")
	}
	if out != "hello\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestHandleExecAggregatesStream(t *testing.T) {
	api, _ := newStreamAPI(t)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb1", "command": "echo hello"})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got vsock.ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "hello\n" || got.Stderr != "warn\n" || got.ExitCode != 0 {
		t.Fatalf("aggregate = %+v", got)
	}
}

func TestExecStreamRequiresToken(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sb2", "vsock.sock")
	startFakeStreamUDS(t, sock, []vsock.ExecStreamFrame{{Kind: vsock.FrameExit}})
	api := NewSandboxAPI(dir) // NOT tokenless
	if err := api.RegisterSandbox("sb2", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb2", sock)
	api.RegisterToken("sb2", "secret")
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"sandbox": "sb2", "command": "x"})
	resp, err := http.Post(srv.URL+"/v1/exec/stream", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run 'TestHandleExecStream|TestHandleExecAggregates|TestExecStreamRequiresToken' -v`
Expected: FAIL, `api.RegisterStreamPath undefined`, `/v1/exec/stream` 404.

- [ ] **Step 3: Add stream path tracking + handlers**

In `internal/daemon/sandbox_api.go`:

Add a field to the `SandboxAPI` struct (after `agents` at line 23):

```go
	// streamPaths maps sandbox ID to the vsock UDS path used to open a DEDICATED
	// connection per streaming exec (so a long stream never interleaves with the
	// shared agents[id] connection). Guarded by mu.
	streamPaths map[string]string
```

Initialize it in `NewSandboxAPI` (after `agents: make(...)` at line 47):

```go
		streamPaths:  make(map[string]string),
```

Clear it in `UnregisterSandbox` (alongside the other deletes, around line 165):

```go
		delete(api.streamPaths, sandboxID)
```

Add the registration method after `RegisterSandbox` (after line 155):

```go
// RegisterStreamPath records the vsock UDS path for opening per-stream
// dedicated connections to a sandbox's guest agent. forkd calls this with the
// same path it passed to RegisterSandbox; the standalone server uses the unix
// fallback path. Without a recorded path, /v1/exec/stream falls back to the
// shared connection's aggregated Exec (no incremental output).
func (api *SandboxAPI) RegisterStreamPath(sandboxID, vsockPath string) {
	api.mu.Lock()
	api.streamPaths[sandboxID] = vsockPath
	api.mu.Unlock()
}

// dialStream opens a dedicated streaming connection for sandboxID, honoring the
// unix fallback the standalone server enables.
func (api *SandboxAPI) dialStream(sandboxID string) (*vsock.StreamConn, error) {
	api.mu.RLock()
	path, ok := api.streamPaths[sandboxID]
	api.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s has no stream path", sandboxID)
	}
	sc, err := vsock.DialStream(path, vsock.AgentPort)
	if err == nil {
		return sc, nil
	}
	if api.unixFallback && errors.Is(err, fs.ErrNotExist) {
		return vsock.DialStreamUnix(fmt.Sprintf("/tmp/sandbox-agent-%d.sock", vsock.AgentPort))
	}
	return nil, err
}
```

Export the unix dial helper from `internal/vsock/client.go` by renaming `dialStreamUnix` to `DialStreamUnix` (and update the call site in `stream_test.go` accordingly):

```go
// DialStreamUnix dials a plain unix socket that already speaks the CONNECT
// preamble (the standalone server's unix fallback and tests).
func DialStreamUnix(sockPath string) (*StreamConn, error) {
```

Register the route in `Handler()` (after the `POST /v1/exec` line at 211):

```go
	mux.HandleFunc("POST /v1/exec/stream", api.handleExecStream)
```

Add the streaming handler and reimplement `handleExec` to aggregate. Replace `handleExec` (lines 291-327) with:

```go
func (api *SandboxAPI) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30
	}

	var out, errb strings.Builder
	exit, err := api.runExecStream(r.Context(), req, func(stream vsock.StreamName, data []byte) error {
		if stream == vsock.StreamStdout {
			out.Write(data)
		} else {
			errb.Write(data)
		}
		return nil
	})
	if err != nil {
		writeErr(w, fmt.Sprintf("exec failed: %v", err), 500)
		return
	}

	result := &vsock.ExecResponse{
		ExitCode:   exit.ExitCode,
		Stdout:     out.String(),
		Stderr:     errb.String(),
		ExecTimeMs: exit.ExecTimeMs,
	}
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "exec",
		Detail:    fmt.Sprintf("exit=%d cmd=%s", result.ExitCode, truncateCommand(req.Command)),
		OK:        true,
	})
	writeJSON(w, result)
}

// runExecStream opens a dedicated stream connection and drives one exec,
// invoking onChunk per chunk and returning the terminal frame. It falls back to
// the shared connection's aggregated Exec when no stream path is registered so
// callers still work on hosts that have not wired streaming.
func (api *SandboxAPI) runExecStream(ctx context.Context, req execRequest, onChunk vsock.ChunkFunc) (*vsock.ExecStreamFrame, error) {
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30
	}
	sc, err := api.dialStream(req.Sandbox)
	if err != nil {
		// Fallback: aggregate via the shared connection (no incremental output).
		agent, gerr := api.getAgent(req.Sandbox)
		if gerr != nil {
			return nil, gerr
		}
		resp, eerr := agent.Exec(req.Command, req.WorkingDir, req.Env, timeout)
		if eerr != nil {
			return nil, eerr
		}
		_ = onChunk(vsock.StreamStdout, []byte(resp.Stdout))
		_ = onChunk(vsock.StreamStderr, []byte(resp.Stderr))
		return &vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: resp.ExitCode, ExecTimeMs: resp.ExecTimeMs}, nil
	}
	defer sc.Close()
	return sc.ExecStream(ctx, &vsock.ExecRequest{
		Command:    req.Command,
		WorkingDir: req.WorkingDir,
		Env:        req.Env,
		Timeout:    timeout,
	}, onChunk)
}

func (api *SandboxAPI) handleExecStream(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	if _, err := api.getAgent(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)

	writeLine := func(v any) error {
		if err := enc.Encode(v); err != nil {
			return err
		}
		return rc.Flush()
	}

	exit, err := api.runExecStream(r.Context(), req, func(stream vsock.StreamName, data []byte) error {
		return writeLine(map[string]any{"stream": string(stream), "data": data})
	})
	if err != nil {
		// The stream has already started; emit a terminal error frame rather
		// than an HTTP status (status was sent 200 with the first byte). The
		// message carries actionable text and never echoes secrets.
		_ = writeLine(map[string]any{"exit_code": 1, "error": fmt.Sprintf("exec stream failed: %v", err)})
		return
	}
	_ = writeLine(map[string]any{"exit_code": exit.ExitCode, "exec_time_ms": exit.ExecTimeMs, "error": exit.Error})

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "exec_stream",
		Detail:    fmt.Sprintf("exit=%d cmd=%s", exit.ExitCode, truncateCommand(req.Command)),
		OK:        true,
	})
}
```

Add `"context"` to the import block in `sandbox_api.go` (it currently imports `bytes`, `crypto/subtle`, etc.).

Note on `data` encoding: `map[string]any{"data": data}` where `data` is `[]byte` marshals to base64 via `encoding/json`, matching the wire format in decision 2 and the test's `base64.StdEncoding.DecodeString`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run 'TestHandleExecStream|TestHandleExecAggregates|TestExecStreamRequiresToken' -v`
Expected: PASS (all three). The auth test passes because `requireBearer` wraps the whole mux including `/v1/exec/stream` (`sandbox_api.go:217`), so the 401 fires before the handler runs.

- [ ] **Step 5: Run the full daemon suite to confirm no regression**

Run: `go test ./internal/daemon/`
Expected: ok (existing token_auth and exec tests still pass; `handleExec` now aggregates the stream but returns the same JSON shape).

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/sandbox_api.go internal/daemon/exec_stream_test.go internal/vsock/client.go internal/vsock/stream_test.go
git commit -m "feat: add forkd NDJSON exec-stream endpoint and aggregate one-shot exec on it

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: forkd Fork path registers the stream path; sandbox-server routes the stream

**Files:**
- Modify: `cmd/sandbox-server/main.go:118-119` (route) and `:224-230` (register stream path)
- Modify: the forkd Fork/ForkRunning path that calls `RegisterSandbox` to also call `RegisterStreamPath`

- [ ] **Step 1: Find the forkd registration call sites**

Run: `grep -rn "RegisterSandbox" internal/daemon/ cmd/ --include=*.go | grep -v _test`
Expected: the `cmd/sandbox-server/main.go:226` call and the forkd server call(s) in `internal/daemon`.

- [ ] **Step 2: Write the failing test (sandbox-server route smoke)**

Add to `internal/daemon/exec_stream_test.go`:

```go
func TestStreamPathRegisteredWithSandbox(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sb3", "vsock.sock")
	startFakeStreamUDS(t, sock, []vsock.ExecStreamFrame{{Kind: vsock.FrameExit, ExitCode: 0}})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb3", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb3", sock)
	sc, err := api.dialStream("sb3")
	if err != nil {
		t.Fatalf("dialStream after register: %v", err)
	}
	sc.Close()
}
```

- [ ] **Step 3: Run test to verify it fails (before wiring) or passes (the method exists)**

Run: `go test ./internal/daemon/ -run TestStreamPathRegisteredWithSandbox -v`
Expected: PASS (the method was added in Task 4). This test pins the contract; the wiring below makes production paths call it.

- [ ] **Step 4: Wire forkd and sandbox-server**

In `cmd/sandbox-server/main.go`, register the stream route alongside `/v1/exec` (after line 118):

```go
	mux.Handle("POST /v1/exec/stream", apiHandler)
```

And in `handleFork`, after the successful `RegisterSandbox` (inside the `if !s.mockMode` block, after line 228), record the stream path:

```go
			s.sandboxAPI.RegisterStreamPath(req.ID, vsockPath)
```

In the forkd server registration path (the call site found in Step 1, in `internal/daemon`), add the matching `RegisterStreamPath(sandboxID, vsockPath)` immediately after each `RegisterSandbox(sandboxID, vsockPath)`. Use the SAME `vsockPath` variable already in scope at that call site.

- [ ] **Step 5: Run builds + daemon suite**

Run: `go build ./... && go test ./internal/daemon/`
Expected: build ok, tests ok.

- [ ] **Step 6: Commit**

```bash
git add cmd/sandbox-server/main.go internal/daemon/exec_stream_test.go internal/daemon/<forkd-server-file>.go
git commit -m "feat: register per-sandbox stream path in forkd and sandbox-server fork paths

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

(Replace `<forkd-server-file>.go` with the actual file found in Step 1.)

---

## Task 6: Python SDK streaming exec + background handle

**Files:**
- Modify: `sdk/python/mitos/sandbox.py:204-235` (add streaming params + `exec_background`)
- Modify: `sdk/python/mitos/types.py` (add `BackgroundProcess`)
- Create: `sdk/python/tests/test_stream.py`

- [ ] **Step 1: Write the failing test**

Create `sdk/python/tests/test_stream.py`:

```python
from __future__ import annotations

import base64
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from mitos.sandbox import Sandbox


def _ndjson_lines():
    return [
        json.dumps({"stream": "stdout", "data": base64.b64encode(b"out1").decode()}),
        json.dumps({"stream": "stderr", "data": base64.b64encode(b"err1").decode()}),
        json.dumps({"stream": "stdout", "data": base64.b64encode(b"out2").decode()}),
        json.dumps({"exit_code": 7, "exec_time_ms": 2.0}),
    ]


class _Handler(BaseHTTPRequestHandler):
    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.end_headers()
        for line in _ndjson_lines():
            self.wfile.write((line + "\n").encode())
            self.wfile.flush()

    def log_message(self, *args):  # silence
        pass


@pytest.fixture()
def stream_server():
    srv = HTTPServer(("127.0.0.1", 0), _Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield f"127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def _direct_sandbox(endpoint: str) -> Sandbox:
    # Build a Sandbox without k8s: set endpoint and id directly.
    sb = Sandbox.__new__(Sandbox)
    import httpx

    sb._endpoint = endpoint
    sb._sandbox_id = "sb1"
    sb._token = None
    sb._http = httpx.Client(timeout=30.0)
    return sb


def test_exec_streams_callbacks(stream_server):
    sb = _direct_sandbox(stream_server)
    out, err = [], []
    result = sb.exec(
        "echo hi",
        on_stdout=lambda b: out.append(b),
        on_stderr=lambda b: err.append(b),
    )
    assert b"".join(out) == b"out1out2"
    assert b"".join(err) == b"err1"
    assert result.exit_code == 7
    assert result.stdout == "out1out2"


def test_exec_background_wait(stream_server):
    sb = _direct_sandbox(stream_server)
    proc = sb.exec_background("sleep 1")
    result = proc.wait()
    assert result.exit_code == 7
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/test_stream.py -v`
Expected: FAIL, `exec() got an unexpected keyword argument 'on_stdout'` and `Sandbox has no attribute exec_background`.

- [ ] **Step 3: Add BackgroundProcess to types.py**

In `sdk/python/mitos/types.py`, after `ExecResult` (after the `exec_time_ms` line), add:

```python
from typing import Callable, Optional  # add to existing imports if not present
```

and at the end of the file:

```python
@dataclass
class BackgroundProcess:
    """A handle to a streaming exec started in the background.

    wait() drains the stream to completion and returns the aggregate
    ExecResult. kill() stops the process by closing the underlying HTTP
    stream, which forkd turns into a context cancel that kills the guest
    process group.
    """

    _drain: Callable[[], ExecResult]
    _close: Callable[[], None]
    _result: Optional[ExecResult] = None

    def wait(self) -> ExecResult:
        if self._result is None:
            self._result = self._drain()
        return self._result

    def kill(self) -> None:
        self._close()
```

- [ ] **Step 4: Implement streaming exec in sandbox.py**

In `sdk/python/mitos/sandbox.py`, update imports at the top:

```python
import base64
import json
from typing import Callable, Optional
```

and add `BackgroundProcess` to the `from mitos.types import (...)` line.

Replace the `exec` method (lines 204-235) with a streaming-capable version plus `exec_background`:

```python
    def exec(
        self,
        command: str,
        timeout: int = 30,
        working_dir: str = "/workspace",
        env: Optional[dict[str, str]] = None,
        on_stdout: Optional[Callable[[bytes], None]] = None,
        on_stderr: Optional[Callable[[bytes], None]] = None,
    ) -> ExecResult:
        """Execute a command in the sandbox.

        When on_stdout or on_stderr is given, output is streamed over
        /v1/exec/stream (NDJSON) and the callbacks fire per chunk as bytes
        arrive; the returned ExecResult still carries the full aggregate. With
        no callbacks the blocking /v1/exec path is used unchanged.
        """
        if on_stdout is None and on_stderr is None:
            return self._exec_blocking(command, timeout, working_dir, env)
        out_parts: list[bytes] = []
        err_parts: list[bytes] = []
        exit_code, exec_time_ms = self._stream(
            command, timeout, working_dir, env,
            lambda b: (out_parts.append(b), on_stdout(b) if on_stdout else None),
            lambda b: (err_parts.append(b), on_stderr(b) if on_stderr else None),
        )
        return ExecResult(
            exit_code=exit_code,
            stdout=b"".join(out_parts).decode("utf-8", "replace"),
            stderr=b"".join(err_parts).decode("utf-8", "replace"),
            exec_time_ms=exec_time_ms,
        )

    def _exec_blocking(self, command, timeout, working_dir, env) -> ExecResult:
        payload: dict = {
            "sandbox": self._sandbox_ref,
            "command": command,
            "timeout": timeout,
            "working_dir": working_dir,
        }
        if env:
            payload["env"] = env
        resp = self._http.post(
            f"{self._base_url}/exec",
            json=payload,
            timeout=timeout + 5,
            headers=self._auth_headers(),
        )
        resp.raise_for_status()
        data = resp.json()
        return ExecResult(
            exit_code=data["exit_code"],
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
            exec_time_ms=data.get("exec_time_ms", 0),
        )

    def _stream(self, command, timeout, working_dir, env, on_out, on_err):
        """Opens /v1/exec/stream and feeds chunks to on_out/on_err. Returns
        (exit_code, exec_time_ms). Raises on transport error frames."""
        payload: dict = {
            "sandbox": self._sandbox_ref,
            "command": command,
            "timeout": timeout,
            "working_dir": working_dir,
        }
        if env:
            payload["env"] = env
        exit_code = 0
        exec_time_ms = 0.0
        with self._http.stream(
            "POST",
            f"{self._base_url}/exec/stream",
            json=payload,
            timeout=None,
            headers=self._auth_headers(),
        ) as resp:
            resp.raise_for_status()
            for line in resp.iter_lines():
                if not line:
                    continue
                frame = json.loads(line)
                if "exit_code" in frame and "stream" not in frame:
                    exit_code = frame["exit_code"]
                    exec_time_ms = frame.get("exec_time_ms", 0.0)
                    if frame.get("error"):
                        raise RuntimeError(f"exec stream error: {frame['error']}")
                    continue
                data = base64.b64decode(frame["data"]) if frame.get("data") else b""
                if frame.get("stream") == "stderr":
                    on_err(data)
                else:
                    on_out(data)
        return exit_code, exec_time_ms

    def exec_background(
        self,
        command: str,
        timeout: int = 86400,
        working_dir: str = "/workspace",
        env: Optional[dict[str, str]] = None,
        on_stdout: Optional[Callable[[bytes], None]] = None,
        on_stderr: Optional[Callable[[bytes], None]] = None,
    ) -> "BackgroundProcess":
        """Start a long-running command and return a handle. wait() drains to
        completion; kill() closes the stream so forkd cancels the guest process
        group. Default timeout is one day so a background server is not reaped by
        the per-exec timeout."""
        out_parts: list[bytes] = []
        err_parts: list[bytes] = []

        def drain() -> ExecResult:
            exit_code, exec_time_ms = self._stream(
                command, timeout, working_dir, env,
                lambda b: (out_parts.append(b), on_stdout(b) if on_stdout else None),
                lambda b: (err_parts.append(b), on_stderr(b) if on_stderr else None),
            )
            return ExecResult(
                exit_code=exit_code,
                stdout=b"".join(out_parts).decode("utf-8", "replace"),
                stderr=b"".join(err_parts).decode("utf-8", "replace"),
                exec_time_ms=exec_time_ms,
            )

        return BackgroundProcess(_drain=drain, _close=lambda: self._http.close())
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/test_stream.py -v`
Expected: PASS (both tests).

- [ ] **Step 6: Run the full Python suite**

Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/`
Expected: all pass (existing `test_sandbox.py` blocking exec unchanged).

- [ ] **Step 7: Commit**

```bash
git add sdk/python/mitos/sandbox.py sdk/python/mitos/types.py sdk/python/tests/test_stream.py
git commit -m "feat: add Python streaming exec callbacks and background process handle

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: TypeScript SDK streaming exec + background handle

**Files:**
- Modify: `sdk/typescript/src/http.ts` (add `postStream`)
- Modify: `sdk/typescript/src/sandbox.ts` (add streaming `exec` opts + `execBackground`)
- Modify: `sdk/typescript/src/types.ts` (add `BackgroundProcess`)
- Create: `sdk/typescript/test/stream.test.ts`

- [ ] **Step 1: Write the failing test**

Create `sdk/typescript/test/stream.test.ts`:

```ts
import { describe, it, expect, vi } from "vitest";
import { Sandbox } from "../src/sandbox.js";

function ndjsonResponse(lines: string[]): Response {
  const body = lines.map((l) => l + "\n").join("");
  return new Response(body, {
    status: 200,
    headers: { "Content-Type": "application/x-ndjson" },
  });
}

function b64(s: string): string {
  return Buffer.from(s, "utf8").toString("base64");
}

describe("streaming exec", () => {
  it("invokes onStdout/onStderr per chunk and returns aggregate", async () => {
    const lines = [
      JSON.stringify({ stream: "stdout", data: b64("out1") }),
      JSON.stringify({ stream: "stderr", data: b64("err1") }),
      JSON.stringify({ stream: "stdout", data: b64("out2") }),
      JSON.stringify({ exit_code: 7, exec_time_ms: 2 }),
    ];
    const fetchMock = vi.fn().mockResolvedValue(ndjsonResponse(lines));
    vi.stubGlobal("fetch", fetchMock);

    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const out: string[] = [];
    const err: string[] = [];
    const result = await sb.exec("echo hi", {
      onStdout: (b) => out.push(new TextDecoder().decode(b)),
      onStderr: (b) => err.push(new TextDecoder().decode(b)),
    });

    expect(out.join("")).toBe("out1out2");
    expect(err.join("")).toBe("err1");
    expect(result.exitCode).toBe(7);
    expect(result.stdout).toBe("out1out2");
    vi.unstubAllGlobals();
  });

  it("execBackground returns a handle whose wait() drains the stream", async () => {
    const lines = [
      JSON.stringify({ stream: "stdout", data: b64("ready") }),
      JSON.stringify({ exit_code: 0, exec_time_ms: 1 }),
    ];
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ndjsonResponse(lines)));
    const sb = new Sandbox({ id: "sb1", endpoint: "localhost:8080" });
    const proc = await sb.execBackground("sleep 1");
    const result = await proc.wait();
    expect(result.exitCode).toBe(0);
    expect(result.stdout).toBe("ready");
    vi.unstubAllGlobals();
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sdk/typescript && npx vitest run test/stream.test.ts`
Expected: FAIL, `exec` does not accept `onStdout`, `execBackground` is not a function.

- [ ] **Step 3: Add postStream to http.ts**

In `sdk/typescript/src/http.ts`, add a method to `HttpClient` (after `post`, before `del`):

```ts
  /**
   * POSTs a JSON body to `path` and returns the raw streaming Response so the
   * caller can read the chunked NDJSON body line by line. Throws AgentRunError
   * on a non-2xx status before any body is read.
   */
  async postStream(path: string, body: unknown): Promise<Response> {
    const resp = await fetch(this.baseUrl + path, {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify(body),
    });
    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      throw AgentRunError.fromResponse(resp.status, text, this.token);
    }
    return resp;
  }
```

- [ ] **Step 4: Add BackgroundProcess to types.ts**

In `sdk/typescript/src/types.ts`, append:

```ts
/**
 * A handle to a streaming exec started in the background. `wait()` drains the
 * stream to completion and resolves the aggregate ExecResult; `kill()` aborts
 * the underlying HTTP stream, which forkd turns into a guest process-group
 * kill.
 */
export interface BackgroundProcess {
  wait(): Promise<ExecResult>;
  kill(): void;
}
```

- [ ] **Step 5: Implement streaming exec in sandbox.ts**

In `sdk/typescript/src/sandbox.ts`, import the new type:

```ts
import type { BackgroundProcess, ExecResult, FileInfo } from "./types.js";
```

Replace the `exec` method (lines 131-149) and add `execBackground` + a private stream reader:

```ts
  /**
   * Runs a command in the sandbox. With no stream callbacks it POSTs /v1/exec
   * and maps the snake_case response. With onStdout/onStderr it streams
   * /v1/exec/stream (NDJSON) and fires the callbacks per chunk while still
   * resolving the full aggregate ExecResult.
   */
  async exec(
    command: string,
    opts?: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
  ): Promise<ExecResult> {
    if (!opts?.onStdout && !opts?.onStderr) {
      const body: Record<string, unknown> = { sandbox: this.id, command };
      if (opts?.timeoutSeconds !== undefined) {
        body["timeout"] = opts.timeoutSeconds;
      }
      const resp = await this.http.post<execResponseWire>("/v1/exec", body);
      return {
        exitCode: resp.exit_code,
        stdout: resp.stdout ?? "",
        stderr: resp.stderr ?? "",
        execTimeMs: resp.exec_time_ms,
      };
    }
    return this.streamExec(command, opts);
  }

  /**
   * Starts a long-running command and returns a handle. wait() drains the
   * stream; kill() aborts it so forkd cancels the guest process group. The
   * default timeout is one day so a background server is not reaped by the
   * per-exec timeout.
   */
  async execBackground(
    command: string,
    opts?: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
  ): Promise<BackgroundProcess> {
    const controller = new AbortController();
    const timeout = opts?.timeoutSeconds ?? 86400;
    const promise = this.streamExec(command, { ...opts, timeoutSeconds: timeout }, controller.signal);
    return {
      wait: () => promise,
      kill: () => controller.abort(),
    };
  }

  private async streamExec(
    command: string,
    opts: {
      timeoutSeconds?: number;
      onStdout?: (chunk: Uint8Array) => void;
      onStderr?: (chunk: Uint8Array) => void;
    },
    signal?: AbortSignal,
  ): Promise<ExecResult> {
    const body: Record<string, unknown> = { sandbox: this.id, command };
    if (opts.timeoutSeconds !== undefined) {
      body["timeout"] = opts.timeoutSeconds;
    }
    const resp = await this.http.postStream("/v1/exec/stream", body);
    const reader = resp.body!.getReader();
    const decoder = new TextDecoder();
    const td = new TextDecoder();
    let buffered = "";
    let exitCode = 0;
    let execTimeMs: number | undefined;
    const outParts: string[] = [];
    const errParts: string[] = [];

    const handleLine = (line: string) => {
      if (line === "") return;
      const frame = JSON.parse(line) as {
        stream?: string;
        data?: string;
        exit_code?: number;
        exec_time_ms?: number;
        error?: string;
      };
      if (frame.exit_code !== undefined && frame.stream === undefined) {
        exitCode = frame.exit_code;
        execTimeMs = frame.exec_time_ms;
        if (frame.error) {
          throw new AgentRunError(`exec stream error: ${frame.error}`, {
            code: "exec_stream_error",
            cause: frame.error,
            remediation: "Inspect the command and the forkd logs for the failure.",
          });
        }
        return;
      }
      const bytes = frame.data
        ? Uint8Array.from(Buffer.from(frame.data, "base64"))
        : new Uint8Array();
      const text = td.decode(bytes);
      if (frame.stream === "stderr") {
        errParts.push(text);
        opts.onStderr?.(bytes);
      } else {
        outParts.push(text);
        opts.onStdout?.(bytes);
      }
    };

    for (;;) {
      const { done, value } = await reader.read();
      if (signal?.aborted) {
        await reader.cancel();
        break;
      }
      if (done) break;
      buffered += decoder.decode(value, { stream: true });
      let nl: number;
      while ((nl = buffered.indexOf("\n")) >= 0) {
        const line = buffered.slice(0, nl);
        buffered = buffered.slice(nl + 1);
        handleLine(line);
      }
    }
    if (buffered.trim() !== "") {
      handleLine(buffered.trim());
    }

    return {
      exitCode,
      stdout: outParts.join(""),
      stderr: errParts.join(""),
      execTimeMs,
    };
  }
```

Note: `Buffer` is available in the Node test environment; for browser builds the existing SDK already targets Node (it uses global `fetch` from Node 18+). If a non-Node runtime is later targeted, swap `Buffer.from(..., "base64")` for `atob`. This is recorded as a follow-up in the ergonomics plan (follow-up 5).

- [ ] **Step 6: Run test to verify it passes**

Run: `cd sdk/typescript && npx vitest run test/stream.test.ts`
Expected: PASS (both tests).

- [ ] **Step 7: Run the full TS suite + typecheck**

Run: `cd sdk/typescript && npx tsc --noEmit && npx vitest run`
Expected: typecheck clean, all tests pass (existing `sandbox.test.ts` blocking exec unchanged).

- [ ] **Step 8: Commit**

```bash
git add sdk/typescript/src/http.ts sdk/typescript/src/sandbox.ts sdk/typescript/src/types.ts sdk/typescript/test/stream.test.ts
git commit -m "feat: add TypeScript streaming exec callbacks and background process handle

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: proto comments + threat-model delta

**Files:**
- Modify: `proto/forkd.proto:225-243` (comments only)
- Modify: `docs/threat-model.md` (add exec-stream row)

- [ ] **Step 1: Update proto comments (no wire change)**

In `proto/forkd.proto`, above `message ExecStreamRequest` (line 225), add a comment documenting that streaming exec is served by the forkd HTTP API (NDJSON), mirroring the existing `Exec` gRPC stub note:

```proto
// ExecStream is reserved for a future gRPC streaming transport. Today, like
// Exec, streaming exec is served by the forkd HTTP sandbox API
// (POST /v1/exec/stream, application/x-ndjson) on the HTTP port, not over gRPC.
// The message shapes below are kept aligned with the vsock ExecStreamFrame
// (stream, data, done/exit_code) so a later gRPC binding is a thin adapter.
```

Run: `make proto` only if the messages changed. They did NOT change here (comment only), so regeneration is not required; confirm with:
Run: `git diff --stat proto/forkd.proto` (one file, comment lines only).

- [ ] **Step 2: Add the threat-model row**

In `docs/threat-model.md`, in the forkd HTTP sandbox API section (the table that lists exec/files surfaces), add a row for the streaming endpoint. Find the exec row:

Run: `grep -n "/v1/exec" docs/threat-model.md`

Add directly beneath it a row stating: the `POST /v1/exec/stream` endpoint shares the SAME per-sandbox bearer-token gate as `/v1/exec` (the `requireBearer` middleware wraps the whole mux, `internal/daemon/sandbox_api.go:217`), opens a dedicated vsock connection per stream that is closed on client disconnect, and inherits the same command-only auditing (command text and exit code, never output bytes). Status: **mitigated** for auth; note the new resource consideration that one dedicated vsock connection per in-flight stream is held for the command lifetime (a long-running background exec holds a connection until killed), so connection count now scales with concurrent streams.

Use plain ASCII punctuation (no em/en dashes). Example row to insert:

```markdown
| `POST /v1/exec/stream` (NDJSON streaming exec) | forkd HTTP :9091 | Same per-sandbox bearer token as `/v1/exec` (`requireBearer` wraps the whole mux). One dedicated vsock connection per in-flight stream, closed on client disconnect or exit; a background exec holds it until killed. Audited as command + exit code only, never output bytes. | mitigated (auth); note: concurrent-stream connection count is a new resource dimension |
```

- [ ] **Step 3: Commit**

```bash
git add proto/forkd.proto docs/threat-model.md
git commit -m "docs: document exec-stream HTTP transport and threat-model surface

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: Lint, cross-build, and full verification (non-KVM)

**Files:** none (verification only)

- [ ] **Step 1: gofmt**

Run: `gofmt -l internal/vsock/ internal/daemon/ guest/agent/ cmd/sandbox-server/`
Expected: no output (all formatted).

- [ ] **Step 2: golangci-lint, BOTH invocations**

Run: `golangci-lint run --timeout=5m`
Run: `GOOS=linux golangci-lint run --timeout=5m`
Expected: both clean. The second invocation is the only one that lints `guest/agent/exec_stream.go` (linux build tag).

- [ ] **Step 3: Guest cross-build**

Run: `GOOS=linux GOARCH=amd64 go build ./guest/agent/`
Expected: builds clean.

- [ ] **Step 4: Full Go unit suite**

Run: `go test ./internal/vsock/ ./internal/daemon/`
Run: `make test-unit`
Expected: ok.

- [ ] **Step 5: Python + TS suites**

Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/`
Run: `cd sdk/typescript && npx tsc --noEmit && npx vitest run`
Expected: all pass.

- [ ] **Step 6: Commit (only if any fmt/lint fix was needed)**

```bash
git add <only the files lint/fmt changed>
git commit -m "chore: gofmt and lint fixes for streaming exec

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: KVM-cluster-verified real-VM streaming behavior

These behaviors exercise a real Firecracker guest agent over vsock and CANNOT be proven by the unix-socket unit tests. They are marked **KVM-cluster-verified**: run them on the live Talos KVM cluster (or the `firecracker-test` KVM runner) and paste the observed output into the PR. The `kvm-test.yaml` workflow already triggers on `guest/**`, `internal/vsock/**`, and `internal/daemon/**` paths (`.github/workflows/kvm-test.yaml:8-31`), so these paths bring this plan's changes under that job automatically.

**Files:** none (verification only). Optionally add a KVM smoke command under `cmd/` if a scripted check is preferred; the manual commands below are sufficient for the PR evidence.

- [ ] **Step 1: KVM-cluster-verified, incremental delivery**

On a forkd node (or sandbox-server with a real rootfs), create a sandbox, then stream a command that emits over time and confirm chunks arrive incrementally rather than all at once:

Run (against the sandbox API, with `$EP` the endpoint, `$SB` the sandbox id, `$TOK` the bearer token):
```bash
curl -sN -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"sandbox":"'"$SB"'","command":"for i in 1 2 3; do echo line$i; sleep 1; done"}' \
  "http://$EP/v1/exec/stream"
```
Expected output (one NDJSON line per second, `data` is base64; `bGluZTEK` = `line1\n`):
```
{"stream":"stdout","data":"bGluZTEK"}
{"stream":"stdout","data":"bGluZTIK"}
{"stream":"stdout","data":"bGluZTMK"}
{"exit_code":0,"exec_time_ms":<~3000>}
```
Verification that delivery is incremental: the three stdout lines print roughly one second apart (watch the terminal), not all at the end. Confirm by piping through `ts` if available: `... | ts '%.s'` shows ~1s gaps.

- [ ] **Step 2: KVM-cluster-verified, stderr separation and exit code**

Run:
```bash
curl -sN -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"sandbox":"'"$SB"'","command":"echo out; echo err 1>&2; exit 5"}' \
  "http://$EP/v1/exec/stream"
```
Expected (order of the two chunk lines may vary; `b3V0Cg==`=`out\n`, `ZXJyCg==`=`err\n`):
```
{"stream":"stdout","data":"b3V0Cg=="}
{"stream":"stderr","data":"ZXJyCg=="}
{"exit_code":5,"exec_time_ms":<small>}
```

- [ ] **Step 3: KVM-cluster-verified, one-shot exec still aggregates over the stream**

Run:
```bash
curl -s -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"sandbox":"'"$SB"'","command":"echo hello; echo warn 1>&2; exit 0"}' \
  "http://$EP/v1/exec"
```
Expected (single JSON object, identical shape to the pre-change response):
```json
{"exit_code":0,"stdout":"hello\n","stderr":"warn\n","exec_time_ms":<small>}
```

- [ ] **Step 4: KVM-cluster-verified, background kill terminates the process group**

Start a background process from the Python SDK against the cluster sandbox, confirm output streams, then kill and confirm the guest process tree is gone:
```bash
python3 - <<'PY'
import time
from mitos import ... # construct the cluster Sandbox for $SB
sb = ...  # the Ready sandbox
chunks = []
proc = sb.exec_background(
    "echo started; while true; do echo tick; sleep 1; done",
    on_stdout=lambda b: chunks.append(b),
)
time.sleep(3)
proc.kill()
print("collected:", b"".join(chunks).decode())
PY
```
Then on the node, confirm no orphaned `sh -c` child for that command remains:
```bash
# In a second shell against the same sandbox:
curl -s -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"sandbox":"'"$SB"'","command":"pgrep -af \"while true\" || echo NONE"}' \
  "http://$EP/v1/exec"
```
Expected: the Python output shows `started` and several `tick` lines collected before the kill; the follow-up exec prints `{"exit_code":0,"stdout":"NONE\n",...}` (the process group was killed, so no `while true` survivor). This proves the `Setpgid` + `syscall.Kill(-pid, SIGKILL)` cancel path in `guest/agent/exec_stream.go`.

- [ ] **Step 5: Record evidence**

Paste the four observed outputs into the PR description under a "KVM-cluster-verified" heading. Per the repository no-unverified-claims rule, any latency or throughput number claimed for streaming must come from a `bench/` run, not from these functional checks; these steps prove behavior, not performance.

---

## Self-review notes

- Spec coverage: vsock framing (Task 1), host ExecStream + one-shot-on-stream (Task 2), guest streaming + pgroup kill (Task 3), forkd NDJSON endpoint + aggregated /v1/exec + auth gating (Task 4), forkd/sandbox-server wiring (Task 5), Python streaming + background (Task 6), TS streaming + background (Task 7), proto + threat-model (Task 8), lint/cross-build (Task 9), real-VM KVM verification (Task 10). All design decisions in the header are realized in a task.
- Type consistency: `ExecStreamFrame{Kind, Stream, Data, ExitCode, Error, ExecTimeMs}`, `FrameChunk`/`FrameExit`, `StreamStdout`/`StreamStderr`, `TypeExecStream`, `ChunkFunc`, `StreamConn.ExecStream`, `StreamConn.Exec`, `DialStream`/`DialStreamUnix`, `SandboxAPI.RegisterStreamPath`/`dialStream`/`runExecStream`/`handleExecStream` are used identically across tasks. Python `BackgroundProcess(_drain, _close)` and TS `BackgroundProcess{wait, kill}` match their tests.
- No placeholders: every code step shows complete code; the only deferred items are explicitly named follow-up plans below.

---

## Follow-up plans (this workstream)

### (2) PTY / interactive terminal
- Add a `TypePtyExec` vsock request and a bidirectional frame channel (input frames host->guest, output frames guest->host) over a dedicated connection, using `creack/pty` in the guest to allocate a TTY.
- forkd exposes `GET /v1/exec/pty` as a WebSocket (binary frames) since interactive input needs a duplex transport NDJSON cannot provide.
- Introduce a real process-id registry + `POST /v1/exec/{pid}/signal` and window-resize (`SIGWINCH`/`TIOCSWINSZ`) control frames; this is where the kill-by-pid RPC deferred in this plan lands.
- SDKs gain `terminal()` returning a duplex handle (write/onData/resize/kill); parity in Python (threads) and TS (async iterators).
- Threat-model delta: a TTY is a richer interactive surface; document input handling and the duplex auth (token on the WebSocket upgrade).

### (3) Code interpreter (run_code with rich multi-MIME results + structured error)
- Add a `run_code` sandbox API layered on streaming exec that runs a language kernel (start with Python via a guest-side runner) and captures rich outputs (text, images, tables) as MIME parts.
- Define a structured result frame (`{mime, data}` parts + a structured error `{name, value, traceback}`) carried as additional NDJSON frame kinds on the existing stream.
- Persist a kernel per sandbox so subsequent `run_code` calls share state (variables, imports); model kernel lifecycle (start/restart) over the stream control frames.
- SDKs gain `run_code(code)` returning `{results: [{mime,data}], logs, error}` with helpers to pull the first image/text.
- Threat-model delta: a long-lived in-guest kernel changes the post-exec trust window; document it.

### (4) Preview URLs / port exposure
- Add per-sandbox port exposure: forkd allocates a host-side proxy (or reuses the per-fork /30 + DNS resolver already in `NotifyForkedNetwork`) and maps `https://<sandbox>-<port>.<base>` to the guest port.
- New sandbox API `POST /v1/ports/expose {port}` returning a preview URL; track exposed ports in `SandboxAPI` and tear them down on `UnregisterSandbox`.
- Reuse the bearer-token gate plus an optional public/unauthenticated toggle per exposed port (default closed).
- SDKs gain `expose_port(port) -> url` and `list_ports()`.
- Threat-model delta: exposing a guest port is a new ingress surface; document the proxy auth, default-closed posture, and teardown on terminate.

### (5) SDK ergonomics + server-side LLM-legible errors
- Async variants: Python `async def aexec/astream` over `httpx.AsyncClient`; TS already async, add `AsyncIterable` chunk iteration alongside callbacks.
- Replace the TS `Buffer.from(..., "base64")` Node dependency with a runtime-agnostic base64 decode so the SDK runs in browsers/edge.
- Server-side structured errors: forkd returns the LLM-legible `{code, cause, remediation}` shape (issue #28) for exec/stream failures, mirrored into `AgentRunError` (TS) and a new `AgentRunError` (Python) instead of raw `RuntimeError`.
- Convenience: `exec` returning a context manager / disposable that auto-kills on scope exit; `stdout`/`stderr` as decoded string streams by default with a `binary=True` opt-out.
- Docs + examples updated in the same PR; benchmark a streaming throughput number into `bench/` so the README can cite it under the no-unverified-claims rule.
