package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// scanResult carries one line from the scan goroutine or the terminal error.
type scanResult struct {
	line []byte
	err  error
}

// Protocol and server identity constants.
const (
	// protocolVersion is the MCP protocol revision this server speaks.
	protocolVersion = "2025-06-18"
	serverName      = "mitos-mcp"
	serverVersion   = "0.1.0"
)

// Options configures a Server.
type Options struct {
	// ServerVersion overrides the advertised server version when set.
	ServerVersion string
	// EnableWorkspaceTools advertises the workspace tools in tools/list. Their
	// dispatch is deferred (#21); enabling only affects discovery.
	EnableWorkspaceTools bool
}

// Server is an MCP server that dispatches tool calls to a SandboxBackend.
type Server struct {
	backend SandboxBackend
	opts    Options
	tools   []Tool
	byName  map[string]Tool
}

// New builds a Server over the given backend.
func New(backend SandboxBackend, opts Options) *Server {
	tools := coreTools()
	if opts.EnableWorkspaceTools {
		tools = append(tools, workspaceTools()...)
	}
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	return &Server{backend: backend, opts: opts, tools: tools, byName: byName}
}

// version returns the advertised server version.
func (s *Server) version() string {
	if s.opts.ServerVersion != "" {
		return s.opts.ServerVersion
	}
	return serverVersion
}

// ---- wire types ----

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type capabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
}

type toolsListResult struct {
	Tools []Tool `json:"tools"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// textContent is a single MCP content block of type text.
type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is the result of a tools/call. isError marks an LLM-legible tool
// failure (as opposed to a transport-level JSON-RPC error).
type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// llmError is the LLM-legible error payload carried inside a failed tool
// result. It is never a bare code: cause is a one-sentence human explanation and
// remediation is an actionable next step (API v2 rule, issue #28).
type llmError struct {
	Code        string `json:"code"`
	Cause       string `json:"cause"`
	Remediation string `json:"remediation"`
}

// Run drives the stdio JSON-RPC loop, reading newline-delimited requests from
// in and writing responses to out until in reaches EOF or ctx is cancelled.
//
// The scan loop runs in a separate goroutine so that a cancelled ctx unblocks
// promptly without waiting for the next line on the reader. When ctx is
// cancelled, Run returns ctx.Err() (or nil if EOF and ctx are both done). The
// scan goroutine will remain blocked on its next Read until the caller closes
// in (the normal behavior for a stdio process receiving SIGTERM), which is
// acceptable; the goroutine does not leak any other resources.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := newFrameReader(in)
	writer := newFrameWriter(out)

	lines := make(chan scanResult, 1)
	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			raw, err := reader.Read()
			select {
			case lines <- scanResult{line: raw, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sr := <-lines:
			if errors.Is(sr.err, io.EOF) {
				return nil
			}
			if sr.err != nil {
				return fmt.Errorf("read request: %w", sr.err)
			}

			var req Request
			if uerr := json.Unmarshal(sr.line, &req); uerr != nil {
				// Cannot recover an id from an unparseable request.
				if werr := writer.Write(errorResponse(nil, codeParseError, "parse error")); werr != nil {
					return werr
				}
				continue
			}

			resp, hasResp := s.handle(ctx, &req)
			if !hasResp {
				continue // notification: no response
			}
			if werr := writer.Write(resp); werr != nil {
				return werr
			}
		}
	}
}

// handle routes a single request. The second return is false for notifications,
// which must not produce a response.
func (s *Server) handle(ctx context.Context, req *Request) (Response, bool) {
	if req.JSONRPC != jsonrpcVersion {
		if req.IsNotification() {
			return Response{}, false
		}
		return errorResponse(req.ID, codeInvalidRequest, "invalid jsonrpc version"), true
	}

	switch req.Method {
	case "initialize":
		return s.okResponse(req.ID, s.handleInitialize()), true
	case "notifications/initialized":
		return Response{}, false
	case "tools/list":
		return s.okResponse(req.ID, toolsListResult{Tools: s.tools}), true
	case "tools/call":
		return s.handleToolsCall(ctx, req), true
	default:
		if req.IsNotification() {
			return Response{}, false
		}
		return errorResponse(req.ID, codeMethodNotFound, fmt.Sprintf("unknown method %q", req.Method)), true
	}
}

func (s *Server) handleInitialize() initializeResult {
	return initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    capabilities{Tools: &toolsCapability{ListChanged: false}},
		ServerInfo:      serverInfo{Name: serverName, Version: s.version()},
	}
}

func (s *Server) handleToolsCall(ctx context.Context, req *Request) Response {
	var params toolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid tools/call params")
	}

	tool, ok := s.byName[params.Name]
	if !ok {
		return s.okResponse(req.ID, toolErrorResult(llmError{
			Code:        "unknown_tool",
			Cause:       fmt.Sprintf("No tool named %q is registered on this server.", params.Name),
			Remediation: "Call tools/list to see the available tool names and retry with one of them.",
		}))
	}

	args, verr := parseArgs(tool, params.Arguments)
	if verr != nil {
		return s.okResponse(req.ID, toolErrorResult(*verr))
	}

	result := s.dispatch(ctx, tool.Name, args)
	return s.okResponse(req.ID, result)
}

// parsedArgs holds the validated, typed arguments for a tool call.
type parsedArgs struct {
	pool       string
	sandbox    string
	command    string
	path       string
	content    string
	timeoutSec int
	replicas   int
}

// parseArgs validates required fields against the tool schema and extracts typed
// values. On a validation failure it returns an LLM-legible error.
func parseArgs(tool Tool, raw json.RawMessage) (parsedArgs, *llmError) {
	var m map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return parsedArgs{}, &llmError{
				Code:        "invalid_arguments",
				Cause:       fmt.Sprintf("The arguments for tool %q were not a JSON object.", tool.Name),
				Remediation: "Pass arguments as a JSON object matching the tool inputSchema.",
			}
		}
	}
	if m == nil {
		m = map[string]any{}
	}

	for _, reqField := range tool.InputSchema.Required {
		v, present := m[reqField]
		if !present || v == nil {
			return parsedArgs{}, &llmError{
				Code:        "missing_required_argument",
				Cause:       fmt.Sprintf("Tool %q requires the %q argument, which was missing or null.", tool.Name, reqField),
				Remediation: fmt.Sprintf("Retry the call including a %q value; see the tool inputSchema for all required fields.", reqField),
			}
		}
	}

	var a parsedArgs
	a.pool = stringArg(m, "pool")
	a.sandbox = stringArg(m, "sandbox")
	a.command = stringArg(m, "command")
	a.path = stringArg(m, "path")
	a.content = stringArg(m, "content")
	a.timeoutSec = intArg(m, "timeout_seconds")
	a.replicas = intArg(m, "replicas")
	return a, nil
}

func stringArg(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func intArg(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

// dispatch routes a validated tool call to the backend and shapes the result.
func (s *Server) dispatch(ctx context.Context, name string, a parsedArgs) toolResult {
	switch name {
	case ToolSandboxCreate:
		id, err := s.backend.Create(ctx, a.pool)
		if err != nil {
			return backendErrorResult(name, err)
		}
		return textResult(fmt.Sprintf("Created sandbox %s", id))

	case ToolSandboxExec:
		res, err := s.backend.Exec(ctx, a.sandbox, a.command, a.timeoutSec)
		if err != nil {
			return backendErrorResult(name, err)
		}
		payload, _ := json.Marshal(res)
		return textResult(string(payload))

	case ToolSandboxReadFile:
		content, err := s.backend.ReadFile(ctx, a.sandbox, a.path)
		if err != nil {
			return backendErrorResult(name, err)
		}
		return textResult(content)

	case ToolSandboxWriteFile:
		if err := s.backend.WriteFile(ctx, a.sandbox, a.path, a.content); err != nil {
			return backendErrorResult(name, err)
		}
		return textResult(fmt.Sprintf("Wrote %d bytes to %s in sandbox %s", len(a.content), a.path, a.sandbox))

	case ToolSandboxFork:
		ids, err := s.backend.Fork(ctx, a.sandbox, a.replicas)
		if err != nil {
			return backendForkErrorResult(err)
		}
		payload, _ := json.Marshal(ids)
		return textResult(string(payload))

	case ToolSandboxTerminate:
		if err := s.backend.Terminate(ctx, a.sandbox); err != nil {
			return backendErrorResult(name, err)
		}
		return textResult(fmt.Sprintf("Terminated sandbox %s", a.sandbox))

	default:
		// Reachable only if a tool is advertised but not dispatched (the
		// workspace stubs); report it LLM-legibly rather than panicking.
		return toolErrorResult(llmError{
			Code:        "tool_not_dispatchable",
			Cause:       fmt.Sprintf("Tool %q is advertised but not yet dispatchable on this server.", name),
			Remediation: "Use one of the sandbox_* tools; workspace tools are not yet implemented (issue #21).",
		})
	}
}

// ---- result helpers ----

func textResult(text string) toolResult {
	return toolResult{Content: []textContent{{Type: "text", Text: text}}}
}

func toolErrorResult(e llmError) toolResult {
	payload, _ := json.Marshal(e)
	return toolResult{
		Content: []textContent{{Type: "text", Text: string(payload)}},
		IsError: true,
	}
}

// backendErrorResult maps a backend error to an LLM-legible tool error. Secret
// values are never logged or echoed; only the error string the backend chose to
// surface is included, and backends are responsible for keeping it clean.
func backendErrorResult(tool string, err error) toolResult {
	return toolErrorResult(llmError{
		Code:        "backend_error",
		Cause:       fmt.Sprintf("The %s operation failed: %s", tool, err.Error()),
		Remediation: "Verify the sandbox id and arguments, check sandbox status, and retry; if it persists the backend may be unavailable.",
	})
}

// backendForkErrorResult maps a fork backend error to an LLM-legible tool
// error. The error string is expected to name any partially-created sandbox ids
// so the LLM can terminate them.
func backendForkErrorResult(err error) toolResult {
	return toolErrorResult(llmError{
		Code:        "backend_error",
		Cause:       fmt.Sprintf("The %s operation failed: %s", ToolSandboxFork, err.Error()),
		Remediation: "The error message names any sandbox ids that were created before the failure; terminate them to avoid resource leaks, then retry.",
	})
}

func (s *Server) okResponse(id json.RawMessage, result any) Response {
	b, err := json.Marshal(result)
	if err != nil {
		return errorResponse(id, codeInternalError, "failed to marshal result")
	}
	return Response{JSONRPC: jsonrpcVersion, ID: id, Result: b}
}

func errorResponse(id json.RawMessage, code int, msg string) Response {
	return Response{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg},
	}
}
