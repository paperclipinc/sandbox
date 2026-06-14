package agentcli

import (
	"context"
	"sync"
	"time"
)

// RevisionInfo is one row of a workspace revision log.
type RevisionInfo struct {
	Name      string
	Phase     string
	Lineage   string // "fromClaim:<n>" or "fromWorkspaceRevision:<n>" or "root"
	Resumable bool
	Age       time.Duration
}

// DiffInfo is the path-level content-hash diff of a revision against its parent.
type DiffInfo struct {
	Parent   string
	Added    []string
	Removed  []string
	Modified []string
}

// WorkspaceInfo is one row of a workspace listing.
type WorkspaceInfo struct {
	Name      string
	Head      string
	Revisions int
	Resumable bool
	Age       time.Duration
}

// WorkspaceBackend is the workspace lifecycle surface the `mitos ws` commands
// dispatch to. It is narrow on purpose, mirroring the sandbox Backend, so tests
// supply a FakeWorkspaceBackend and the cluster backend wraps the client.
type WorkspaceBackend interface {
	CreateWorkspace(ctx context.Context, name string) error
	ListWorkspaces(ctx context.Context, namespace string) ([]WorkspaceInfo, error)
	Log(ctx context.Context, workspace string) ([]RevisionInfo, error)
	Diff(ctx context.Context, workspace, revision string) (DiffInfo, error)
	Fork(ctx context.Context, srcWorkspace, srcRevision, dstWorkspace string) (newRevision string, err error)
	Revert(ctx context.Context, workspace, revision string) (newRevision string, err error)
	RemoveWorkspace(ctx context.Context, name string) error
	Bind(ctx context.Context, sandboxID, workspace string) error
}

// FakeWorkspaceBackend records calls and returns canned data for CLI tests.
type FakeWorkspaceBackend struct {
	mu         sync.Mutex
	Calls      []FakeCall
	Revisions  []RevisionInfo
	DiffV      DiffInfo
	Workspaces []WorkspaceInfo
	NewRev     string
	Errors     map[string]error
}

// NewFakeWorkspaceBackend returns a FakeWorkspaceBackend with a default canned
// new-revision name and an empty error map.
func NewFakeWorkspaceBackend() *FakeWorkspaceBackend {
	return &FakeWorkspaceBackend{NewRev: "branch-abc123", Errors: map[string]error{}}
}

func (f *FakeWorkspaceBackend) record(c FakeCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, c)
}

func (f *FakeWorkspaceBackend) err(m string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Errors[m]
}

// RecordedCalls returns a copy of the recorded calls.
func (f *FakeWorkspaceBackend) RecordedCalls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeCall, len(f.Calls))
	copy(out, f.Calls)
	return out
}

// CreateWorkspace implements WorkspaceBackend.
func (f *FakeWorkspaceBackend) CreateWorkspace(_ context.Context, name string) error {
	f.record(FakeCall{Method: "ws_create", SandboxID: name})
	return f.err("ws_create")
}

// ListWorkspaces implements WorkspaceBackend.
func (f *FakeWorkspaceBackend) ListWorkspaces(_ context.Context, ns string) ([]WorkspaceInfo, error) {
	f.record(FakeCall{Method: "ws_list", Namespace: ns})
	if e := f.err("ws_list"); e != nil {
		return nil, e
	}
	return f.Workspaces, nil
}

// Log implements WorkspaceBackend.
func (f *FakeWorkspaceBackend) Log(_ context.Context, ws string) ([]RevisionInfo, error) {
	f.record(FakeCall{Method: "ws_log", SandboxID: ws})
	if e := f.err("ws_log"); e != nil {
		return nil, e
	}
	return f.Revisions, nil
}

// Diff implements WorkspaceBackend.
func (f *FakeWorkspaceBackend) Diff(_ context.Context, ws, rev string) (DiffInfo, error) {
	f.record(FakeCall{Method: "ws_diff", SandboxID: ws, Path: rev})
	if e := f.err("ws_diff"); e != nil {
		return DiffInfo{}, e
	}
	return f.DiffV, nil
}

// Fork implements WorkspaceBackend.
func (f *FakeWorkspaceBackend) Fork(_ context.Context, src, rev, dst string) (string, error) {
	f.record(FakeCall{Method: "ws_fork", SandboxID: src, Path: rev, Content: dst})
	if e := f.err("ws_fork"); e != nil {
		return "", e
	}
	return f.NewRev, nil
}

// Revert implements WorkspaceBackend.
func (f *FakeWorkspaceBackend) Revert(_ context.Context, ws, rev string) (string, error) {
	f.record(FakeCall{Method: "ws_revert", SandboxID: ws, Path: rev})
	if e := f.err("ws_revert"); e != nil {
		return "", e
	}
	return f.NewRev, nil
}

// RemoveWorkspace implements WorkspaceBackend.
func (f *FakeWorkspaceBackend) RemoveWorkspace(_ context.Context, name string) error {
	f.record(FakeCall{Method: "ws_rm", SandboxID: name})
	return f.err("ws_rm")
}

// Bind implements WorkspaceBackend.
func (f *FakeWorkspaceBackend) Bind(_ context.Context, sb, ws string) error {
	f.record(FakeCall{Method: "ws_bind", SandboxID: sb, Content: ws})
	return f.err("ws_bind")
}
