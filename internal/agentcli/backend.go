// Package agentcli implements the mitos command-line interface: a thin,
// dependency-free command tree over a Backend that drives the sandbox lifecycle
// (create, exec, file IO, fork, terminate, list).
//
// The CLI is built on the standard library flag package with a hand-written
// subcommand dispatcher rather than a third-party framework. spf13/cobra is only
// an indirect dependency in this repo; promoting it to a direct dependency would
// add maintenance surface and dependency risk on go 1.24 for no real gain, since
// the command surface here is small and fully covered by stdlib flag. This
// mirrors how internal/mcp implements its protocol surface directly to stay
// dependency free.
//
// Run is the testable entry point: it takes the Backend, output writers, and the
// raw argument slice, and returns a process exit code. cmd/mitos is a thin
// wrapper that builds a real cluster Backend and calls Run.
package agentcli

import (
	"context"
	"sync"
	"time"
)

// ExecResult is the outcome of running a command inside a sandbox. It mirrors
// mcp.ExecResult so the HTTP exec shapes can be reused.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// SandboxInfo is one row of the sandbox listing. Age is derived from the
// object's creation timestamp at list time.
type SandboxInfo struct {
	Name     string
	Pool     string
	Phase    string
	Node     string
	Endpoint string
	Age      time.Duration
}

// Backend is the sandbox lifecycle surface the CLI dispatches to. It is narrow
// on purpose so tests can supply a FakeBackend and the real implementation (the
// cluster backend) can wrap the controller-runtime client plus the sandbox HTTP
// API.
type Backend interface {
	// Create provisions a new sandbox from the named pool and returns its id.
	Create(ctx context.Context, pool string) (sandboxID string, err error)
	// Exec runs command inside the sandbox. A timeoutSec of 0 means the backend
	// default applies.
	Exec(ctx context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error)
	// ReadFile returns the contents of path inside the sandbox.
	ReadFile(ctx context.Context, sandboxID, path string) (string, error)
	// WriteFile writes content to path inside the sandbox.
	WriteFile(ctx context.Context, sandboxID, path, content string) error
	// Fork forks the sandbox into n copies and returns their ids.
	Fork(ctx context.Context, sandboxID string, n int) (forkIDs []string, err error)
	// Terminate destroys the sandbox.
	Terminate(ctx context.Context, sandboxID string) error
	// List returns the sandboxes in namespace. An empty namespace means the
	// backend default (all namespaces, or its configured default).
	List(ctx context.Context, namespace string) ([]SandboxInfo, error)
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
	Namespace  string
}

// FakeBackend is a test double that records every call and returns canned
// responses. An error can be injected per method via Errors to exercise the
// error path.
type FakeBackend struct {
	mu    sync.Mutex
	Calls []FakeCall

	// Canned responses.
	CreateID    string
	ExecResultV ExecResult
	ReadContent string
	ForkIDs     []string
	ListInfos   []SandboxInfo

	// Errors injects an error for the named method ("create", "exec",
	// "read_file", "write_file", "fork", "terminate", "list"). When set for a
	// method, that method returns the error instead of its canned response.
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

// Create implements Backend.
func (f *FakeBackend) Create(_ context.Context, pool string) (string, error) {
	f.record(FakeCall{Method: "create", Pool: pool})
	if e := f.err("create"); e != nil {
		return "", e
	}
	return f.CreateID, nil
}

// Exec implements Backend.
func (f *FakeBackend) Exec(_ context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error) {
	f.record(FakeCall{Method: "exec", SandboxID: sandboxID, Command: command, TimeoutSec: timeoutSec})
	if e := f.err("exec"); e != nil {
		return ExecResult{}, e
	}
	return f.ExecResultV, nil
}

// ReadFile implements Backend.
func (f *FakeBackend) ReadFile(_ context.Context, sandboxID, path string) (string, error) {
	f.record(FakeCall{Method: "read_file", SandboxID: sandboxID, Path: path})
	if e := f.err("read_file"); e != nil {
		return "", e
	}
	return f.ReadContent, nil
}

// WriteFile implements Backend.
func (f *FakeBackend) WriteFile(_ context.Context, sandboxID, path, content string) error {
	f.record(FakeCall{Method: "write_file", SandboxID: sandboxID, Path: path, Content: content})
	return f.err("write_file")
}

// Fork implements Backend.
func (f *FakeBackend) Fork(_ context.Context, sandboxID string, n int) ([]string, error) {
	f.record(FakeCall{Method: "fork", SandboxID: sandboxID, Replicas: n})
	if e := f.err("fork"); e != nil {
		return nil, e
	}
	return f.ForkIDs, nil
}

// Terminate implements Backend.
func (f *FakeBackend) Terminate(_ context.Context, sandboxID string) error {
	f.record(FakeCall{Method: "terminate", SandboxID: sandboxID})
	return f.err("terminate")
}

// List implements Backend.
func (f *FakeBackend) List(_ context.Context, namespace string) ([]SandboxInfo, error) {
	f.record(FakeCall{Method: "list", Namespace: namespace})
	if e := f.err("list"); e != nil {
		return nil, e
	}
	return f.ListInfos, nil
}
