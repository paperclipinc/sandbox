// Package mcp implements a Model Context Protocol (MCP) server that exposes
// the sandbox lifecycle (create, exec, file IO, fork, terminate) as MCP tools
// over a JSON-RPC 2.0 stdio transport.
//
// The MCP protocol surface is implemented directly (see jsonrpc.go) rather than
// via the official Go SDK: that SDK requires go 1.25 and pulls in oauth2, jwt,
// and x/tools, while this repo is pinned to go 1.24. The subset we need
// (initialize, tools/list, tools/call over newline-delimited JSON-RPC) is small,
// fully specified, and dependency free.
package mcp

import (
	"context"
	"sync"
)

// ExecResult is the outcome of running a command inside a sandbox.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// SandboxBackend is the lifecycle surface the MCP server dispatches tool calls
// to. It is intentionally narrow so tests can supply a fake and the real
// implementation (Task 3) can wrap forkd or sandbox-server.
type SandboxBackend interface {
	// Create provisions a new sandbox from the named pool and returns its id.
	Create(ctx context.Context, pool string) (sandboxID string, err error)
	// Exec runs command inside the sandbox. A timeoutSec of 0 means the
	// backend default applies.
	Exec(ctx context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error)
	// ReadFile returns the contents of path inside the sandbox.
	ReadFile(ctx context.Context, sandboxID, path string) (string, error)
	// WriteFile writes content to path inside the sandbox.
	WriteFile(ctx context.Context, sandboxID, path, content string) error
	// Fork forks the sandbox into replicas copies and returns their ids.
	Fork(ctx context.Context, sandboxID string, replicas int) (forkIDs []string, err error)
	// Terminate destroys the sandbox.
	Terminate(ctx context.Context, sandboxID string) error
}

// FakeCall records a single backend invocation for assertions in tests.
type FakeCall struct {
	Method     string
	Pool       string
	SandboxID  string
	Command    string
	TimeoutSec int
	Path       string
	Content    string
	Replicas   int
}

// FakeBackend is a test double that records every call and returns canned
// responses. Responses are settable; an error can be injected per method via
// Errors so the LLM-legible error path can be exercised.
type FakeBackend struct {
	mu    sync.Mutex
	Calls []FakeCall

	// Canned responses.
	CreateID    string
	ExecResultV ExecResult
	ReadContent string
	ForkIDs     []string

	// Errors injects an error for the named method ("create", "exec",
	// "read_file", "write_file", "fork", "terminate"). When set for a method,
	// that method returns the error instead of its canned response.
	Errors map[string]error
}

// NewFakeBackend returns a FakeBackend with sensible default canned responses.
func NewFakeBackend() *FakeBackend {
	return &FakeBackend{
		CreateID:    "sbx-fake-1",
		ExecResultV: ExecResult{ExitCode: 0, Stdout: "ok", Stderr: ""},
		ReadContent: "file-contents",
		ForkIDs:     []string{"sbx-fork-1", "sbx-fork-2"},
		Errors:      map[string]error{},
	}
}

func (f *FakeBackend) record(c FakeCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, c)
}

func (f *FakeBackend) err(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Errors == nil {
		return nil
	}
	return f.Errors[method]
}

// RecordedCalls returns a copy of the recorded calls.
func (f *FakeBackend) RecordedCalls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeCall, len(f.Calls))
	copy(out, f.Calls)
	return out
}

// Create implements SandboxBackend.
func (f *FakeBackend) Create(_ context.Context, pool string) (string, error) {
	f.record(FakeCall{Method: "create", Pool: pool})
	if e := f.err("create"); e != nil {
		return "", e
	}
	return f.CreateID, nil
}

// Exec implements SandboxBackend.
func (f *FakeBackend) Exec(_ context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error) {
	f.record(FakeCall{Method: "exec", SandboxID: sandboxID, Command: command, TimeoutSec: timeoutSec})
	if e := f.err("exec"); e != nil {
		return ExecResult{}, e
	}
	return f.ExecResultV, nil
}

// ReadFile implements SandboxBackend.
func (f *FakeBackend) ReadFile(_ context.Context, sandboxID, path string) (string, error) {
	f.record(FakeCall{Method: "read_file", SandboxID: sandboxID, Path: path})
	if e := f.err("read_file"); e != nil {
		return "", e
	}
	return f.ReadContent, nil
}

// WriteFile implements SandboxBackend.
func (f *FakeBackend) WriteFile(_ context.Context, sandboxID, path, content string) error {
	f.record(FakeCall{Method: "write_file", SandboxID: sandboxID, Path: path, Content: content})
	return f.err("write_file")
}

// Fork implements SandboxBackend.
func (f *FakeBackend) Fork(_ context.Context, sandboxID string, replicas int) ([]string, error) {
	f.record(FakeCall{Method: "fork", SandboxID: sandboxID, Replicas: replicas})
	if e := f.err("fork"); e != nil {
		return nil, e
	}
	return f.ForkIDs, nil
}

// Terminate implements SandboxBackend.
func (f *FakeBackend) Terminate(_ context.Context, sandboxID string) error {
	f.record(FakeCall{Method: "terminate", SandboxID: sandboxID})
	return f.err("terminate")
}
