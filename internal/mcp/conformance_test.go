package mcp

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
)

// mcpClient speaks the MCP protocol over a pair of pipes as a real client would:
// it writes newline-delimited JSON-RPC requests to the server and reads the
// server's newline-delimited responses back. It uses the same framing the
// server uses (frameReader/frameWriter) to prove wire compatibility.
type mcpClient struct {
	t      *testing.T
	w      *frameWriter
	r      *frameReader
	nextID int
}

func (c *mcpClient) call(method string, params any) Response {
	c.t.Helper()
	c.nextID++
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			c.t.Fatalf("marshal params: %v", err)
		}
		rawParams = b
	}
	id, _ := json.Marshal(c.nextID)
	req := Request{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: rawParams}
	if err := c.w.Write(req); err != nil {
		c.t.Fatalf("client write %s: %v", method, err)
	}
	raw, err := c.r.Read()
	if err != nil {
		c.t.Fatalf("client read response to %s: %v", method, err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		c.t.Fatalf("client decode response to %s: %v", method, err)
	}
	return resp
}

// notify sends a notification (no id, no response expected).
func (c *mcpClient) notify(method string) {
	c.t.Helper()
	req := Request{JSONRPC: jsonrpcVersion, Method: method}
	if err := c.w.Write(req); err != nil {
		c.t.Fatalf("client notify %s: %v", method, err)
	}
}

// TestConformanceClientDrivesServer runs Server.Run in a goroutine against an
// io.Pipe pair and exercises the full protocol handshake, tool listing, dispatch
// for all six tools, and the two LLM-legible error paths, end to end.
func TestConformanceClientDrivesServer(t *testing.T) {
	fb := NewFakeBackend()
	fb.CreateID = "sbx-conf-1"
	fb.ExecResultV = ExecResult{ExitCode: 3, Stdout: "hello", Stderr: "warn"}
	fb.ReadContent = "the file body"
	fb.ForkIDs = []string{"sbx-a", "sbx-b", "sbx-c"}

	srv := New(fb, Options{})

	// clientToServer: client writes, server reads. serverToClient: server
	// writes, client reads.
	csr, csw := io.Pipe()
	scr, scw := io.Pipe()

	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = srv.Run(context.Background(), csr, scw)
	}()

	client := &mcpClient{
		t: t,
		w: newFrameWriter(csw),
		r: newFrameReader(scr),
	}

	// initialize
	initResp := client.call("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
	})
	if initResp.Error != nil {
		t.Fatalf("initialize returned error: %+v", initResp.Error)
	}
	var init initializeResult
	if err := json.Unmarshal(initResp.Result, &init); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if init.ProtocolVersion == "" {
		t.Error("initialize: protocolVersion missing")
	}
	if init.Capabilities.Tools == nil {
		t.Error("initialize: capabilities.tools not advertised")
	}
	if init.ServerInfo.Name == "" || init.ServerInfo.Version == "" {
		t.Errorf("initialize: serverInfo incomplete: %+v", init.ServerInfo)
	}

	client.notify("notifications/initialized")

	// tools/list
	listResp := client.call("tools/list", nil)
	if listResp.Error != nil {
		t.Fatalf("tools/list returned error: %+v", listResp.Error)
	}
	var list toolsListResult
	if err := json.Unmarshal(listResp.Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(list.Tools) != 6 {
		t.Fatalf("tools/list: expected 6 tools, got %d", len(list.Tools))
	}
	for _, tool := range list.Tools {
		if tool.Name == "" || tool.Description == "" {
			t.Errorf("tool with empty name/description: %+v", tool)
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %q schema type %q != object", tool.Name, tool.InputSchema.Type)
		}
	}

	// tools/call for each of the six, asserting result and recorded args.
	t.Run("create", func(t *testing.T) {
		tr := conformCall(t, client, ToolSandboxCreate, map[string]any{"pool": "default"})
		assertNoError(t, tr)
		if tr.Content[0].Text != "Created sandbox sbx-conf-1" {
			t.Errorf("unexpected text: %q", tr.Content[0].Text)
		}
	})

	t.Run("exec", func(t *testing.T) {
		tr := conformCall(t, client, ToolSandboxExec, map[string]any{
			"sandbox": "sbx-conf-1", "command": "echo hi", "timeout_seconds": 9,
		})
		assertNoError(t, tr)
		var res ExecResult
		if err := json.Unmarshal([]byte(tr.Content[0].Text), &res); err != nil {
			t.Fatalf("decode exec result: %v", err)
		}
		if res != fb.ExecResultV {
			t.Errorf("exec result %+v != canned %+v", res, fb.ExecResultV)
		}
	})

	t.Run("read_file", func(t *testing.T) {
		tr := conformCall(t, client, ToolSandboxReadFile, map[string]any{
			"sandbox": "sbx-conf-1", "path": "/etc/hosts",
		})
		assertNoError(t, tr)
		if tr.Content[0].Text != fb.ReadContent {
			t.Errorf("read result %q != canned %q", tr.Content[0].Text, fb.ReadContent)
		}
	})

	t.Run("write_file", func(t *testing.T) {
		tr := conformCall(t, client, ToolSandboxWriteFile, map[string]any{
			"sandbox": "sbx-conf-1", "path": "/tmp/x", "content": "data",
		})
		assertNoError(t, tr)
	})

	t.Run("fork", func(t *testing.T) {
		tr := conformCall(t, client, ToolSandboxFork, map[string]any{
			"sandbox": "sbx-conf-1", "replicas": 3,
		})
		assertNoError(t, tr)
		var ids []string
		if err := json.Unmarshal([]byte(tr.Content[0].Text), &ids); err != nil {
			t.Fatalf("decode fork ids: %v", err)
		}
		if len(ids) != len(fb.ForkIDs) {
			t.Errorf("fork ids %v != canned %v", ids, fb.ForkIDs)
		}
	})

	t.Run("terminate", func(t *testing.T) {
		tr := conformCall(t, client, ToolSandboxTerminate, map[string]any{"sandbox": "sbx-conf-1"})
		assertNoError(t, tr)
	})

	// Unknown tool: LLM-legible error.
	t.Run("unknown_tool", func(t *testing.T) {
		tr := conformCall(t, client, "bogus_tool", map[string]any{})
		assertLLMError(t, tr, "unknown_tool")
	})

	// Missing required argument: LLM-legible error.
	t.Run("missing_required_arg", func(t *testing.T) {
		tr := conformCall(t, client, ToolSandboxExec, map[string]any{"sandbox": "sbx-conf-1"})
		assertLLMError(t, tr, "missing_required_argument")
	})

	// Assert the backend recorded the right arguments for the dispatched tools.
	calls := fb.RecordedCalls()
	wantOrder := []struct {
		method string
		check  func(FakeCall) bool
	}{
		{"create", func(c FakeCall) bool { return c.Pool == "default" }},
		{"exec", func(c FakeCall) bool {
			return c.SandboxID == "sbx-conf-1" && c.Command == "echo hi" && c.TimeoutSec == 9
		}},
		{"read_file", func(c FakeCall) bool { return c.SandboxID == "sbx-conf-1" && c.Path == "/etc/hosts" }},
		{"write_file", func(c FakeCall) bool {
			return c.SandboxID == "sbx-conf-1" && c.Path == "/tmp/x" && c.Content == "data"
		}},
		{"fork", func(c FakeCall) bool { return c.SandboxID == "sbx-conf-1" && c.Replicas == 3 }},
		{"terminate", func(c FakeCall) bool { return c.SandboxID == "sbx-conf-1" }},
	}
	if len(calls) != len(wantOrder) {
		t.Fatalf("expected %d backend calls, got %d: %+v", len(wantOrder), len(calls), calls)
	}
	for i, w := range wantOrder {
		if calls[i].Method != w.method {
			t.Errorf("call %d method = %q, want %q", i, calls[i].Method, w.method)
		}
		if !w.check(calls[i]) {
			t.Errorf("call %d (%s) recorded wrong args: %+v", i, w.method, calls[i])
		}
	}

	// Close the client->server pipe so Run sees EOF and exits.
	if err := csw.Close(); err != nil {
		t.Fatalf("close client writer: %v", err)
	}
	wg.Wait()
	if runErr != nil {
		t.Errorf("Server.Run returned error: %v", runErr)
	}
	// Drain anything pending and close the server->client side.
	_ = scr.Close()
}

func conformCall(t *testing.T, c *mcpClient, name string, args map[string]any) toolResult {
	t.Helper()
	resp := c.call("tools/call", toolsCallParams{
		Name:      name,
		Arguments: mustMarshal(t, args),
	})
	if resp.Error != nil {
		t.Fatalf("tools/call %s jsonrpc error: %+v", name, resp.Error)
	}
	var tr toolResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		t.Fatalf("decode tool result for %s: %v", name, err)
	}
	return tr
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func assertNoError(t *testing.T, tr toolResult) {
	t.Helper()
	if tr.IsError {
		t.Fatalf("unexpected error result: %+v", tr)
	}
	if len(tr.Content) == 0 {
		t.Fatalf("result has no content")
	}
}

func assertLLMError(t *testing.T, tr toolResult, wantCode string) {
	t.Helper()
	if !tr.IsError {
		t.Fatalf("expected error result, got %+v", tr)
	}
	if len(tr.Content) == 0 {
		t.Fatalf("error result has no content")
	}
	var e llmError
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &e); err != nil {
		t.Fatalf("decode llmError: %v", err)
	}
	if e.Code != wantCode {
		t.Errorf("error code = %q, want %q", e.Code, wantCode)
	}
	if e.Cause == "" || e.Remediation == "" {
		t.Errorf("LLM-legible error missing cause/remediation: %+v", e)
	}
}
