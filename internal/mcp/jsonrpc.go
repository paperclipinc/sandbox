package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// JSON-RPC 2.0 over the MCP stdio transport. The stdio transport frames
// messages as newline-delimited JSON: each message is a single JSON object on
// its own line, with no embedded newlines.

const jsonrpcVersion = "2.0"

// Standard JSON-RPC 2.0 error codes (subset used by this server).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Request is an incoming JSON-RPC request or notification. A notification has a
// nil ID.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the request carries no id (and so expects no
// response).
func (r *Request) IsNotification() bool {
	return len(r.ID) == 0
}

// Response is an outgoing JSON-RPC response. Exactly one of Result or Error is
// set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object. This is a transport-level error
// (malformed request, unknown method); tool-call failures are reported inside a
// successful result via isError, not here.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// frameReader reads newline-delimited JSON-RPC messages.
type frameReader struct {
	sc *bufio.Scanner
}

func newFrameReader(r io.Reader) *frameReader {
	sc := bufio.NewScanner(r)
	// Allow large messages (file contents may be sizeable).
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &frameReader{sc: sc}
}

// Read returns the next raw message line, or io.EOF when the stream is
// exhausted. Blank lines are skipped.
func (fr *frameReader) Read() ([]byte, error) {
	for fr.sc.Scan() {
		line := fr.sc.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		// Copy: Scanner reuses its buffer on the next Scan.
		out := make([]byte, len(line))
		copy(out, line)
		return out, nil
	}
	if err := fr.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}

// frameWriter writes newline-delimited JSON-RPC messages.
type frameWriter struct {
	w io.Writer
}

func newFrameWriter(w io.Writer) *frameWriter {
	return &frameWriter{w: w}
}

// Write marshals v and writes it as a single newline-terminated line.
func (fw *frameWriter) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal jsonrpc message: %w", err)
	}
	b = append(b, '\n')
	if _, err := fw.w.Write(b); err != nil {
		return fmt.Errorf("write jsonrpc message: %w", err)
	}
	return nil
}
