package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// callServer invokes a single request against a Server via handle and returns
// the decoded response. It is the in-process path used by unit tests; the full
// stdio loop is exercised by the conformance test.
func callServer(t *testing.T, s *Server, method string, params any) Response {
	t.Helper()
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		rawParams = b
	}
	req := Request{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(`1`),
		Method:  method,
		Params:  rawParams,
	}
	resp, ok := s.handle(context.Background(), &req)
	if !ok {
		t.Fatalf("method %q produced no response", method)
	}
	return resp
}

func decodeToolResult(t *testing.T, resp Response) toolResult {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected jsonrpc error: %+v", resp.Error)
	}
	var tr toolResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	return tr
}

func decodeLLMError(t *testing.T, tr toolResult) llmError {
	t.Helper()
	if !tr.IsError {
		t.Fatalf("expected isError tool result, got %+v", tr)
	}
	if len(tr.Content) == 0 {
		t.Fatalf("error tool result has no content")
	}
	var e llmError
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &e); err != nil {
		t.Fatalf("decode llmError from %q: %v", tr.Content[0].Text, err)
	}
	if e.Code == "" || e.Cause == "" || e.Remediation == "" {
		t.Fatalf("llmError missing fields: %+v", e)
	}
	return e
}

func TestInitialize(t *testing.T) {
	s := New(NewFakeBackend(), Options{})
	resp := callServer(t, s, "initialize", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	var res initializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if res.ProtocolVersion == "" {
		t.Error("protocolVersion empty")
	}
	if res.Capabilities.Tools == nil {
		t.Error("capabilities.tools not advertised")
	}
	if res.ServerInfo.Name == "" || res.ServerInfo.Version == "" {
		t.Errorf("serverInfo incomplete: %+v", res.ServerInfo)
	}
}

func TestToolsListCoreOnly(t *testing.T) {
	s := New(NewFakeBackend(), Options{})
	resp := callServer(t, s, "tools/list", nil)
	var res toolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(res.Tools) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(res.Tools))
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		ToolSandboxCreate, ToolSandboxExec, ToolSandboxReadFile,
		ToolSandboxWriteFile, ToolSandboxFork, ToolSandboxTerminate,
	} {
		if !names[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
	for _, ws := range workspaceToolNames {
		if names[ws] {
			t.Errorf("workspace tool %q present when disabled", ws)
		}
	}
}

func TestToolsListWithWorkspaceEnabled(t *testing.T) {
	s := New(NewFakeBackend(), Options{EnableWorkspaceTools: true})
	resp := callServer(t, s, "tools/list", nil)
	var res toolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, ws := range workspaceToolNames {
		if !names[ws] {
			t.Errorf("workspace tool %q missing when enabled", ws)
		}
	}
	if len(res.Tools) != 6+len(workspaceToolNames) {
		t.Errorf("expected %d tools, got %d", 6+len(workspaceToolNames), len(res.Tools))
	}
}

func TestToolsCallDispatchesCreate(t *testing.T) {
	fb := NewFakeBackend()
	fb.CreateID = "sbx-77"
	s := New(fb, Options{})

	resp := callServer(t, s, "tools/call", toolsCallParams{
		Name:      ToolSandboxCreate,
		Arguments: json.RawMessage(`{"pool":"default"}`),
	})
	tr := decodeToolResult(t, resp)
	if tr.IsError {
		t.Fatalf("unexpected error result: %+v", tr)
	}
	if got := tr.Content[0].Text; got != "Created sandbox sbx-77" {
		t.Errorf("unexpected text %q", got)
	}

	calls := fb.RecordedCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 backend call, got %d", len(calls))
	}
	if calls[0].Method != "create" || calls[0].Pool != "default" {
		t.Errorf("backend recorded %+v", calls[0])
	}
}

func TestToolsCallDispatchesExecWithArgs(t *testing.T) {
	fb := NewFakeBackend()
	fb.ExecResultV = ExecResult{ExitCode: 2, Stdout: "out", Stderr: "err"}
	s := New(fb, Options{})

	resp := callServer(t, s, "tools/call", toolsCallParams{
		Name:      ToolSandboxExec,
		Arguments: json.RawMessage(`{"sandbox":"sbx-1","command":"ls","timeout_seconds":5}`),
	})
	tr := decodeToolResult(t, resp)
	if tr.IsError {
		t.Fatalf("unexpected error result: %+v", tr)
	}
	var res ExecResult
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &res); err != nil {
		t.Fatalf("decode exec result: %v", err)
	}
	if res != fb.ExecResultV {
		t.Errorf("exec result %+v != canned %+v", res, fb.ExecResultV)
	}

	calls := fb.RecordedCalls()
	if calls[0].SandboxID != "sbx-1" || calls[0].Command != "ls" || calls[0].TimeoutSec != 5 {
		t.Errorf("backend recorded %+v", calls[0])
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	s := New(NewFakeBackend(), Options{})
	resp := callServer(t, s, "tools/call", toolsCallParams{
		Name:      "no_such_tool",
		Arguments: json.RawMessage(`{}`),
	})
	tr := decodeToolResult(t, resp)
	e := decodeLLMError(t, tr)
	if e.Code != "unknown_tool" {
		t.Errorf("code = %q, want unknown_tool", e.Code)
	}
}

func TestToolsCallMissingRequiredArg(t *testing.T) {
	fb := NewFakeBackend()
	s := New(fb, Options{})
	resp := callServer(t, s, "tools/call", toolsCallParams{
		Name:      ToolSandboxExec,
		Arguments: json.RawMessage(`{"sandbox":"sbx-1"}`), // missing command
	})
	tr := decodeToolResult(t, resp)
	e := decodeLLMError(t, tr)
	if e.Code != "missing_required_argument" {
		t.Errorf("code = %q, want missing_required_argument", e.Code)
	}
	if len(fb.RecordedCalls()) != 0 {
		t.Errorf("backend should not be called on validation failure")
	}
}

func TestToolsCallBackendErrorInjection(t *testing.T) {
	fb := NewFakeBackend()
	fb.Errors["create"] = errors.New("pool exhausted")
	s := New(fb, Options{})
	resp := callServer(t, s, "tools/call", toolsCallParams{
		Name:      ToolSandboxCreate,
		Arguments: json.RawMessage(`{"pool":"default"}`),
	})
	tr := decodeToolResult(t, resp)
	e := decodeLLMError(t, tr)
	if e.Code != "backend_error" {
		t.Errorf("code = %q, want backend_error", e.Code)
	}
	if e.Remediation == "" {
		t.Error("backend error must carry remediation")
	}
}

func TestUnknownMethodReturnsJSONRPCError(t *testing.T) {
	s := New(NewFakeBackend(), Options{})
	resp := callServer(t, s, "does/not/exist", nil)
	if resp.Error == nil {
		t.Fatal("expected jsonrpc error for unknown method")
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Errorf("code = %d, want %d", resp.Error.Code, codeMethodNotFound)
	}
}
