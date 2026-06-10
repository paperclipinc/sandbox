# Control-Plane Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the README's core claim true: a `SandboxClaim` actually produces a running sandbox via controller → forkd gRPC, with truthful status endpoints, working pool accounting, node discovery, and an SDK that can exec against what the controller reports.

**Architecture:** Generate the gRPC code from the existing `proto/forkd.proto`, implement the `ForkDaemon` service in `internal/daemon` on top of the existing `ForkEngine` interface, replace the three `not implemented` stubs in the controllers with real gRPC calls through `NodeRegistry`, add forkd pod discovery, and align the Python SDK's k8s mode with forkd's HTTP sandbox API. Everything is testable without KVM via the existing `MockEngine` (envtest + in-process gRPC servers).

**Tech Stack:** Go 1.26, controller-runtime + envtest, gRPC + protobuf, Python 3.12 + pytest + httpx.

**Context for the implementer (read first):**
- `internal/fork/engine.go`: real Firecracker engine. `internal/fork/mock.go`: `MockEngine` (no KVM). Both partially satisfy `internal/daemon/interface.go:ForkEngine`.
- `internal/daemon/server.go`: `Server` wraps a `ForkEngine` + `SandboxAPI`; `RegisterForkDaemonServer` is an empty TODO.
- `internal/controller/sandboxclaim_controller.go:forkOnNode` and `sandboxfork_controller.go:forkRunningOnNode` return `not implemented`.
- `internal/controller/sandboxpool_controller.go:createSnapshots/countReadySnapshots` are no-ops.
- `internal/controller/node_registry.go`: in-memory registry; `StartDiscovery` is a mock-only TODO.
- Envtest suite: `internal/controller/suite_test.go` (needs `make test-controller`, which downloads envtest binaries via setup-envtest).
- Run Go tests with `go test ./internal/<pkg>/... -count=1`. Controller tests: `make test-controller`. Python: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -v`.

---

### Task 1: Protobuf toolchain and generated gRPC code

**Files:**
- Modify: `proto/forkd.proto` (go_package option only)
- Modify: `Makefile` (proto target)
- Create: `proto/forkd/forkd.pb.go`, `proto/forkd/forkd_grpc.pb.go` (generated, committed)

- [x] **Step 1: Install toolchain**

```bash
brew install protobuf
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
export PATH="$PATH:$(go env GOPATH)/bin"
protoc --version   # expect: libprotoc 2x.x
```

- [x] **Step 2: Fix the go_package option so generated code lands in-repo with package name `forkdpb`**

In `proto/forkd.proto` replace:

```proto
option go_package = "github.com/paperclipinc/sandbox/proto/forkd";
```

with:

```proto
option go_package = "github.com/paperclipinc/sandbox/proto/forkd;forkdpb";
```

- [x] **Step 3: Update the Makefile proto target**

Replace the existing `proto:` target with:

```makefile
proto:
	protoc \
	  --go_out=. --go_opt=module=github.com/paperclipinc/sandbox \
	  --go-grpc_out=. --go-grpc_opt=module=github.com/paperclipinc/sandbox \
	  proto/forkd.proto
```

- [x] **Step 4: Generate and tidy**

```bash
make proto
go mod tidy
go build ./...
```

Expected: `proto/forkd/forkd.pb.go` and `proto/forkd/forkd_grpc.pb.go` exist; build passes.

- [x] **Step 5: Commit**

```bash
git add proto/ Makefile go.mod go.sum
git commit -m "feat: generate forkd gRPC code from proto"
```

---

### Task 2: NodeRegistry zero-value safety

`cmd/controller/main.go:54` and `internal/controller/suite_test.go` construct `&controller.NodeRegistry{}` whose `nodes` map is nil; the first `Register` call panics. Fix both construction sites AND make the zero value safe.

**Files:**
- Create: `internal/controller/node_registry_test.go`
- Modify: `internal/controller/node_registry.go:41-46`
- Modify: `cmd/controller/main.go:54`
- Modify: `internal/controller/suite_test.go` (the `nodeRegistry := &controller.NodeRegistry{}` line)

- [x] **Step 1: Write the failing test**

```go
package controller

import (
	"testing"
	"time"
)

func TestRegisterOnZeroValueRegistry(t *testing.T) {
	var r NodeRegistry // zero value, nil map
	r.Register(&NodeInfo{Name: "n1", Endpoint: "10.0.0.1:9090"})

	node, err := r.SelectNode("", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "n1" {
		t.Fatalf("got %q, want n1", node.Name)
	}
}

func TestSelectNodePrefersSnapshotHolder(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(&NodeInfo{Name: "empty", Endpoint: "10.0.0.1:9090", LastHeartbeat: time.Now()})
	r.Register(&NodeInfo{Name: "holder", Endpoint: "10.0.0.2:9090", TemplateIDs: []string{"py"}, LastHeartbeat: time.Now()})

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "holder" {
		t.Fatalf("got %q, want holder", node.Name)
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./internal/controller/ -run 'TestRegisterOnZeroValue|TestSelectNodePrefers' -count=1`
Expected: panic `assignment to entry in nil map` in the first test.

- [x] **Step 3: Make Register lazily initialize the map**

In `node_registry.go`, change `Register`:

```go
// Register adds or updates a node in the registry.
func (r *NodeRegistry) Register(info *NodeInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodes == nil {
		r.nodes = make(map[string]*NodeInfo)
	}
	info.LastHeartbeat = time.Now()
	r.nodes[info.Name] = info
}
```

- [x] **Step 4: Run tests to verify pass**

Run: `go test ./internal/controller/ -run 'TestRegisterOnZeroValue|TestSelectNodePrefers' -count=1`
Expected: PASS.

- [x] **Step 5: Use the constructor at both construction sites anyway**

`cmd/controller/main.go`: `nodeRegistry := controller.NewNodeRegistry()`
`internal/controller/suite_test.go`: `nodeRegistry := controller.NewNodeRegistry()`; and add a package-level `testRegistry *controller.NodeRegistry` var in the suite, assigned from it, so later e2e tests can register fake nodes:

```go
// suite_test.go, package-level:
var testRegistry *controller.NodeRegistry
// in TestMain, replacing the old line:
nodeRegistry := controller.NewNodeRegistry()
testRegistry = nodeRegistry
```

- [x] **Step 6: Build and commit**

```bash
go build ./... && go test ./internal/controller/ -run TestRegister -count=1
git add internal/controller/node_registry.go internal/controller/node_registry_test.go cmd/controller/main.go internal/controller/suite_test.go
git commit -m "fix: NodeRegistry zero-value safety; use constructor everywhere"
```

---

### Task 3: Add ForkRunning to the ForkEngine interface and MockEngine

The gRPC service must expose `ForkRunning`; the real engine has it (`internal/fork/engine.go:164`), the interface and mock do not.

**Files:**
- Modify: `internal/daemon/interface.go`
- Modify: `internal/fork/mock.go`
- Test: `internal/fork/mock_test.go` (append)

- [x] **Step 1: Write the failing test (append to `internal/fork/mock_test.go`)**

```go
func TestMockForkRunning(t *testing.T) {
	e := NewMockEngine()
	e.ForkDelay = 0
	if err := e.CreateTemplate("py", "python:3.12-slim", 0); err != nil {
		t.Fatal(err)
	}
	parent, err := e.Fork("py", "parent", ForkOpts{})
	if err != nil {
		t.Fatal(err)
	}

	child, err := e.ForkRunning(parent.SandboxID, "child", true)
	if err != nil {
		t.Fatalf("ForkRunning: %v", err)
	}
	if child.SandboxID != "child" {
		t.Fatalf("got %q, want child", child.SandboxID)
	}
	if got := e.GetCapacity().ActiveSandboxes; got != 2 {
		t.Fatalf("active sandboxes = %d, want 2", got)
	}

	if _, err := e.ForkRunning("nope", "child2", false); err == nil {
		t.Fatal("expected error for unknown source sandbox")
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./internal/fork/ -run TestMockForkRunning -count=1`
Expected: FAIL; `e.ForkRunning undefined`.

- [x] **Step 3: Implement on MockEngine (append to `internal/fork/mock.go`)**

```go
// ForkRunning simulates checkpoint-and-fork of a running sandbox.
func (e *MockEngine) ForkRunning(sourceSandboxID, newSandboxID string, pauseSource bool) (*ForkResult, error) {
	start := time.Now()

	e.mu.RLock()
	source, ok := e.sandboxes[sourceSandboxID]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", sourceSandboxID)
	}

	time.Sleep(e.ForkDelay)

	id := e.counter.Add(1)
	sandbox := &Sandbox{
		ID:           newSandboxID,
		SnapshotID:   source.SnapshotID,
		Endpoint:     fmt.Sprintf("127.0.0.1:%d", 10000+id),
		CreatedAt:    time.Now(),
		MemoryUnique: source.MemoryUnique,
		MemoryShared: source.MemoryShared,
	}

	e.mu.Lock()
	e.sandboxes[newSandboxID] = sandbox
	e.mu.Unlock()

	elapsed := time.Since(start)
	return &ForkResult{
		SandboxID:    newSandboxID,
		Endpoint:     sandbox.Endpoint,
		ForkTimeMs:   float64(elapsed.Microseconds()) / 1000.0,
		MemoryUnique: sandbox.MemoryUnique,
		MemoryShared: sandbox.MemoryShared,
		VsockPath:    fmt.Sprintf("/tmp/agent-run-mock/sandboxes/%s/vsock.sock", newSandboxID),
	}, nil
}
```

- [x] **Step 4: Add to the interface (`internal/daemon/interface.go`)**

```go
type ForkEngine interface {
	Fork(snapshotID, sandboxID string, opts fork.ForkOpts) (*fork.ForkResult, error)
	ForkRunning(sourceSandboxID, newSandboxID string, pauseSource bool) (*fork.ForkResult, error)
	Terminate(sandboxID string) error
	GetCapacity() fork.Capacity
	CreateTemplate(id string, rootfsPath string, initWaitSecs int) error
}
```

- [x] **Step 5: Run tests and build to verify both engines satisfy the interface**

Run: `go test ./internal/fork/ -count=1 && go build ./...`
Expected: PASS (the real engine already has the matching method).

- [x] **Step 6: Commit**

```bash
git add internal/daemon/interface.go internal/fork/mock.go internal/fork/mock_test.go
git commit -m "feat: add ForkRunning to ForkEngine interface and MockEngine"
```

---

### Task 4: forkd gRPC service implementation

Implement the `ForkDaemon` gRPC service over `Server`. RPCs whose backing engine support doesn't exist yet (`Exec`, `ExecStream`, file ops, `CreateSnapshot`, `DeleteSnapshot`, `DeleteTemplate`) return `codes.Unimplemented` with an honest message; they are served today by the HTTP sandbox API on :9091.

**Files:**
- Create: `internal/daemon/grpc_service.go`
- Create: `internal/daemon/grpc_service_test.go`
- Modify: `internal/daemon/server.go` (RegisterForkDaemonServer + vsock path fix)

- [x] **Step 1: Write the failing test**

`internal/daemon/grpc_service_test.go`:

```go
package daemon

import (
	"context"
	"net"
	"testing"

	"github.com/paperclipinc/sandbox/internal/fork"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func newTestClient(t *testing.T) (forkdpb.ForkDaemonClient, *fork.MockEngine) {
	t.Helper()
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return forkdpb.NewForkDaemonClient(conn), engine
}

func TestGRPCForkLifecycle(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()

	if _, err := client.CreateTemplate(ctx, &forkdpb.CreateTemplateRequest{
		TemplateId: "py", Image: "python:3.12-slim",
	}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	forkResp, err := client.Fork(ctx, &forkdpb.ForkRequest{
		SnapshotId: "py",
		SandboxId:  "sb-1",
		Env:        []*forkdpb.EnvVar{{Key: "SESSION", Value: "abc"}},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if forkResp.SandboxId != "sb-1" || forkResp.Endpoint == "" {
		t.Fatalf("bad fork response: %+v", forkResp)
	}

	runResp, err := client.ForkRunning(ctx, &forkdpb.ForkRunningRequest{
		SourceSandboxId: "sb-1", NewSandboxId: "sb-2", PauseSource: true,
	})
	if err != nil {
		t.Fatalf("ForkRunning: %v", err)
	}
	if runResp.SandboxId != "sb-2" {
		t.Fatalf("got %q, want sb-2", runResp.SandboxId)
	}

	capResp, err := client.GetCapacity(ctx, &forkdpb.GetCapacityRequest{})
	if err != nil {
		t.Fatalf("GetCapacity: %v", err)
	}
	if capResp.ActiveSandboxes != 2 {
		t.Fatalf("active = %d, want 2", capResp.ActiveSandboxes)
	}
	if len(capResp.TemplateIds) != 1 || capResp.TemplateIds[0] != "py" {
		t.Fatalf("templates = %v, want [py]", capResp.TemplateIds)
	}

	if _, err := client.Terminate(ctx, &forkdpb.TerminateRequest{SandboxId: "sb-1"}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

func TestGRPCForkUnknownSnapshot(t *testing.T) {
	client, _ := newTestClient(t)
	_, err := client.Fork(context.Background(), &forkdpb.ForkRequest{
		SnapshotId: "missing", SandboxId: "sb-x",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGRPCUnimplementedRPCsSayWhere(t *testing.T) {
	client, _ := newTestClient(t)
	_, err := client.Exec(context.Background(), &forkdpb.ExecRequest{SandboxId: "sb", Command: "true"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", status.Code(err))
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./internal/daemon/ -count=1`
Expected: compile FAIL (`RegisterForkDaemonServer` signature mismatch / service type missing).

- [x] **Step 3: Implement the service**

`internal/daemon/grpc_service.go`:

```go
package daemon

import (
	"context"
	"strings"

	"github.com/paperclipinc/sandbox/internal/fork"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcService implements forkdpb.ForkDaemonServer over Server.
// Exec and file RPCs are intentionally Unimplemented: that traffic is served
// by the HTTP sandbox API on the forkd HTTP port (default :9091).
type grpcService struct {
	forkdpb.UnimplementedForkDaemonServer
	srv *Server
}

func (g *grpcService) Fork(ctx context.Context, req *forkdpb.ForkRequest) (*forkdpb.ForkResponse, error) {
	result, err := g.srv.Fork(ctx, req.SnapshotId, req.SandboxId, envMap(req.Env), secretMap(req.Secrets))
	if err != nil {
		return nil, grpcError(err)
	}
	return &forkdpb.ForkResponse{
		SandboxId:         result.SandboxID,
		Endpoint:          result.Endpoint,
		ForkTimeMs:        result.ForkTimeMs,
		MemoryUniqueBytes: result.MemoryUnique,
		MemorySharedBytes: result.MemoryShared,
	}, nil
}

func (g *grpcService) ForkRunning(ctx context.Context, req *forkdpb.ForkRunningRequest) (*forkdpb.ForkRunningResponse, error) {
	result, err := g.srv.engine.ForkRunning(req.SourceSandboxId, req.NewSandboxId, req.PauseSource)
	if err != nil {
		return nil, grpcError(err)
	}
	g.srv.sandboxAPI.RegisterSandbox(result.SandboxID, result.VsockPath)
	return &forkdpb.ForkRunningResponse{
		SandboxId:  result.SandboxID,
		Endpoint:   result.Endpoint,
		ForkTimeMs: result.ForkTimeMs,
	}, nil
}

func (g *grpcService) Terminate(ctx context.Context, req *forkdpb.TerminateRequest) (*forkdpb.TerminateResponse, error) {
	if err := g.srv.Terminate(ctx, req.SandboxId); err != nil {
		return nil, grpcError(err)
	}
	return &forkdpb.TerminateResponse{}, nil
}

func (g *grpcService) GetCapacity(ctx context.Context, _ *forkdpb.GetCapacityRequest) (*forkdpb.GetCapacityResponse, error) {
	c := g.srv.engine.GetCapacity()
	return &forkdpb.GetCapacityResponse{
		ActiveSandboxes:   c.ActiveSandboxes,
		MaxSandboxes:      c.MaxSandboxes,
		MemoryTotalBytes:  c.MemoryTotal,
		MemoryUsedBytes:   c.MemoryUsed,
		MemorySharedBytes: c.MemoryShared,
		TemplateIds:       c.TemplateIDs,
		SnapshotIds:       c.SnapshotIDs,
		KvmAvailable:      c.KVMAvailable,
	}, nil
}

func (g *grpcService) CreateTemplate(ctx context.Context, req *forkdpb.CreateTemplateRequest) (*forkdpb.CreateTemplateResponse, error) {
	if err := g.srv.engine.CreateTemplate(req.TemplateId, req.Image, 0); err != nil {
		return nil, grpcError(err)
	}
	return &forkdpb.CreateTemplateResponse{TemplateId: req.TemplateId}, nil
}

func (g *grpcService) Exec(ctx context.Context, _ *forkdpb.ExecRequest) (*forkdpb.ExecResponse, error) {
	return nil, status.Error(codes.Unimplemented, "exec is served by the HTTP sandbox API on the forkd HTTP port")
}

func envMap(vars []*forkdpb.EnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Key] = v.Value
	}
	return m
}

func secretMap(vars []*forkdpb.SecretVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Key] = v.Value
	}
	return m
}

// grpcError maps engine errors to gRPC status codes.
func grpcError(err error) error {
	if strings.Contains(err.Error(), "not found") {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
```

In `internal/daemon/server.go` replace the TODO registration:

```go
// RegisterForkDaemonServer registers the gRPC service.
func RegisterForkDaemonServer(s *grpc.Server, srv *Server) {
	forkdpb.RegisterForkDaemonServer(s, &grpcService{srv: srv})
}
```

(add `forkdpb "github.com/paperclipinc/sandbox/proto/forkd"` to imports) and fix the hardcoded vsock path in `Server.Fork` to use the engine's answer:

```go
	// Connect to the guest agent so exec/files work
	s.sandboxAPI.RegisterSandbox(result.SandboxID, result.VsockPath)
```

- [x] **Step 4: Run tests to verify pass**

Run: `go test ./internal/daemon/ -count=1`
Expected: PASS (3 tests).

- [x] **Step 5: Commit**

```bash
git add internal/daemon/
git commit -m "feat: implement forkd gRPC service over ForkEngine"
```

---

### Task 5: NodeInfo.HTTPEndpoint and NodesWithTemplate

The claim status must report an endpoint the SDK can actually reach: forkd's HTTP sandbox API, not the engine's internal placeholder. The pool controller needs to count which nodes hold a template.

**Files:**
- Modify: `internal/controller/node_registry.go`
- Test: `internal/controller/node_registry_test.go` (append)

- [x] **Step 1: Write the failing tests (append)**

```go
func TestNodesWithTemplate(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(&NodeInfo{Name: "a", TemplateIDs: []string{"py"}})
	r.Register(&NodeInfo{Name: "b"})
	r.Register(&NodeInfo{Name: "c", TemplateIDs: []string{"py", "node"}})

	got := r.NodesWithTemplate("py")
	if len(got) != 2 {
		t.Fatalf("got %d nodes, want 2", len(got))
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./internal/controller/ -run TestNodesWithTemplate -count=1`
Expected: FAIL; `r.NodesWithTemplate undefined`.

- [x] **Step 3: Implement**

Add to `NodeInfo` struct in `node_registry.go`:

```go
	// HTTPEndpoint is the forkd HTTP sandbox API (exec/files), e.g. "10.0.3.7:9091".
	// This is what claim status endpoints point at.
	HTTPEndpoint string
```

Add method:

```go
// NodesWithTemplate returns healthy nodes that hold the given template snapshot.
func (r *NodeRegistry) NodesWithTemplate(templateID string) []*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*NodeInfo
	for _, n := range r.nodes {
		if n.isHealthy() && n.hasSnapshot(templateID) {
			out = append(out, n)
		}
	}
	return out
}
```

- [x] **Step 4: Run tests**

Run: `go test ./internal/controller/ -run 'TestNodesWithTemplate' -count=1`
Expected: PASS. (Healthy check: `Register` stamps `LastHeartbeat`, so freshly registered nodes are healthy.)

- [x] **Step 5: Commit**

```bash
git add internal/controller/node_registry.go internal/controller/node_registry_test.go
git commit -m "feat: NodeInfo.HTTPEndpoint and NodesWithTemplate"
```

---### Task 6: Controller-side forkd client (forkOnNode / forkRunningOnNode)

**Files:**
- Create: `internal/controller/forkd_client.go`
- Create: `internal/controller/forkd_client_test.go`
- Modify: `internal/controller/sandboxclaim_controller.go` (delete the stub `forkOnNode`, lines 215-218)
- Modify: `internal/controller/sandboxfork_controller.go` (delete the stub `forkRunningOnNode`, lines 155-158)

- [x] **Step 1: Write the failing test**

`internal/controller/forkd_client_test.go` (in-package). It spins a real gRPC server with the MockEngine on a random localhost port; no bufconn needed because `NodeRegistry.GetConnection` dials a TCP endpoint.

```go
package controller

import (
	"context"
	"net"
	"testing"

	"github.com/paperclipinc/sandbox/internal/daemon"
	"github.com/paperclipinc/sandbox/internal/fork"
	"google.golang.org/grpc"
)

// startFakeForkd runs a real forkd gRPC server with a MockEngine on 127.0.0.1:0
// and returns its address and engine.
func startFakeForkd(t *testing.T, templates ...string) (string, *fork.MockEngine) {
	t.Helper()
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	for _, tmpl := range templates {
		engine.CreateTemplate(tmpl, tmpl, 0)
	}
	srv := daemon.NewServer(engine, daemon.NewSandboxAPI(t.TempDir()))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	daemon.RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	return lis.Addr().String(), engine
}

func TestForkOnNode(t *testing.T) {
	addr, _ := startFakeForkd(t, "py")

	registry := NewNodeRegistry()
	registry.Register(&NodeInfo{Name: "n1", Endpoint: addr, HTTPEndpoint: "10.0.0.1:9091", TemplateIDs: []string{"py"}})
	node, err := registry.SelectNode("py", "")
	if err != nil {
		t.Fatal(err)
	}

	r := &SandboxClaimReconciler{NodeRegistry: registry}
	result, err := r.forkOnNode(context.Background(), node, "py", "sb-1", map[string]string{"A": "1"})
	if err != nil {
		t.Fatalf("forkOnNode: %v", err)
	}
	if result.SandboxID != "sb-1" {
		t.Fatalf("sandboxID = %q, want sb-1", result.SandboxID)
	}
	// The reported endpoint must be the reachable forkd HTTP API, not the
	// engine-internal placeholder.
	if result.Endpoint != "10.0.0.1:9091" {
		t.Fatalf("endpoint = %q, want 10.0.0.1:9091", result.Endpoint)
	}
}

func TestForkRunningOnNode(t *testing.T) {
	addr, _ := startFakeForkd(t, "py")
	registry := NewNodeRegistry()
	registry.Register(&NodeInfo{Name: "n1", Endpoint: addr, HTTPEndpoint: "10.0.0.1:9091", TemplateIDs: []string{"py"}})
	node, _ := registry.SelectNode("py", "")

	claimRec := &SandboxClaimReconciler{NodeRegistry: registry}
	if _, err := claimRec.forkOnNode(context.Background(), node, "py", "parent", nil); err != nil {
		t.Fatal(err)
	}

	forkRec := &SandboxForkReconciler{NodeRegistry: registry}
	result, err := forkRec.forkRunningOnNode(context.Background(), node, "parent", "child", true)
	if err != nil {
		t.Fatalf("forkRunningOnNode: %v", err)
	}
	if result.SandboxID != "child" || result.Endpoint != "10.0.0.1:9091" {
		t.Fatalf("bad result: %+v", result)
	}
}

func TestForkOnNodeUnknownSnapshot(t *testing.T) {
	addr, _ := startFakeForkd(t) // no templates
	registry := NewNodeRegistry()
	registry.Register(&NodeInfo{Name: "n1", Endpoint: addr})
	node, _ := registry.SelectNode("", "")

	r := &SandboxClaimReconciler{NodeRegistry: registry}
	if _, err := r.forkOnNode(context.Background(), node, "missing", "sb", nil); err == nil {
		t.Fatal("expected error")
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./internal/controller/ -run 'TestForkOnNode|TestForkRunningOnNode' -count=1`
Expected: FAIL; `not implemented` errors from the stubs.

- [x] **Step 3: Implement `internal/controller/forkd_client.go` and delete both stubs**

```go
package controller

import (
	"context"
	"fmt"

	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
)

// forkOnNode asks the forkd on the given node to fork a sandbox from a snapshot.
// The returned endpoint is the node's HTTP sandbox API, which is what clients
// (SDKs) actually talk to.
func (r *SandboxClaimReconciler) forkOnNode(ctx context.Context, node *NodeInfo, snapshotID, sandboxID string, env map[string]string) (*forkResult, error) {
	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		return nil, err
	}
	resp, err := forkdpb.NewForkDaemonClient(conn).Fork(ctx, &forkdpb.ForkRequest{
		SnapshotId: snapshotID,
		SandboxId:  sandboxID,
		Env:        toEnvVars(env),
	})
	if err != nil {
		return nil, fmt.Errorf("forkd fork on %s: %w", node.Name, err)
	}
	return &forkResult{
		SandboxID:  resp.SandboxId,
		Endpoint:   clientEndpoint(node, resp.Endpoint),
		ForkTimeMs: resp.ForkTimeMs,
	}, nil
}

// forkRunningOnNode asks forkd to checkpoint a running sandbox and fork it.
func (r *SandboxForkReconciler) forkRunningOnNode(ctx context.Context, node *NodeInfo, sourceSandboxID, newSandboxID string, pauseSource bool) (*forkRunningResult, error) {
	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		return nil, err
	}
	resp, err := forkdpb.NewForkDaemonClient(conn).ForkRunning(ctx, &forkdpb.ForkRunningRequest{
		SourceSandboxId: sourceSandboxID,
		NewSandboxId:    newSandboxID,
		PauseSource:     pauseSource,
	})
	if err != nil {
		return nil, fmt.Errorf("forkd fork-running on %s: %w", node.Name, err)
	}
	return &forkRunningResult{
		SandboxID:    resp.SandboxId,
		Endpoint:     clientEndpoint(node, resp.Endpoint),
		ForkTimeMs:   resp.ForkTimeMs,
		CheckpointMs: resp.CheckpointTimeMs,
	}, nil
}

// clientEndpoint prefers the node's HTTP sandbox API; the engine-reported
// endpoint is an internal placeholder until guest networking exists.
func clientEndpoint(node *NodeInfo, engineEndpoint string) string {
	if node.HTTPEndpoint != "" {
		return node.HTTPEndpoint
	}
	return engineEndpoint
}

func toEnvVars(m map[string]string) []*forkdpb.EnvVar {
	vars := make([]*forkdpb.EnvVar, 0, len(m))
	for k, v := range m {
		vars = append(vars, &forkdpb.EnvVar{Key: k, Value: v})
	}
	return vars
}
```

Delete the stub `forkOnNode` from `sandboxclaim_controller.go` and the stub `forkRunningOnNode` from `sandboxfork_controller.go` (keep the `forkResult` / `forkRunningResult` structs where they are).

- [x] **Step 4: Run tests**

Run: `go test ./internal/controller/ -run 'TestForkOnNode|TestForkRunningOnNode' -count=1`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "feat: controller calls forkd over gRPC for Fork and ForkRunning"
```

---

### Task 7: Pool controller: real snapshot accounting and creation

**Files:**
- Modify: `internal/controller/sandboxpool_controller.go:73-87`
- Test: `internal/controller/forkd_client_test.go` (append; reuses `startFakeForkd`)

Semantics (document in code): one template snapshot per node; `readySnapshots` = number of healthy nodes holding the pool's template; `createSnapshots` asks nodes lacking the template to build it, up to the deficit. `spec.replicas` is therefore capped by node count for now; pool conditions say so honestly.

- [x] **Step 1: Write the failing test (append to forkd_client_test.go)**

```go
func TestPoolSnapshotAccounting(t *testing.T) {
	addrWith, _ := startFakeForkd(t, "py-tmpl")
	addrWithout, engineWithout := startFakeForkd(t)

	registry := NewNodeRegistry()
	registry.Register(&NodeInfo{Name: "has", Endpoint: addrWith, TemplateIDs: []string{"py-tmpl"}})
	registry.Register(&NodeInfo{Name: "lacks", Endpoint: addrWithout})

	r := &SandboxPoolReconciler{NodeRegistry: registry}

	if got := r.readySnapshotCount("py-tmpl"); got != 1 {
		t.Fatalf("ready = %d, want 1", got)
	}

	created, err := r.createSnapshotsOnNodes(context.Background(), "py-tmpl", "python:3.12-slim", 5)
	if err != nil {
		t.Fatalf("createSnapshotsOnNodes: %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1 (only one node lacks the template)", created)
	}
	caps := engineWithout.GetCapacity()
	if len(caps.TemplateIDs) != 1 || caps.TemplateIDs[0] != "py-tmpl" {
		t.Fatalf("template not created on lacking node: %v", caps.TemplateIDs)
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./internal/controller/ -run TestPoolSnapshotAccounting -count=1`
Expected: FAIL; methods undefined.

- [x] **Step 3: Implement in `sandboxpool_controller.go`**

Replace the three stub methods with:

```go
// readySnapshotCount counts healthy nodes that hold the pool's template snapshot.
// One snapshot per node per template, so replicas are capped by node count.
func (r *SandboxPoolReconciler) readySnapshotCount(templateID string) int32 {
	return int32(len(r.NodeRegistry.NodesWithTemplate(templateID)))
}

// createSnapshotsOnNodes asks up to `deficit` healthy nodes that lack the
// template to build it. Returns how many builds were started.
func (r *SandboxPoolReconciler) createSnapshotsOnNodes(ctx context.Context, templateID, image string, deficit int32) (int32, error) {
	var created int32
	var errs []error
	for _, node := range r.NodeRegistry.ListNodes() {
		if created >= deficit {
			break
		}
		if !node.isHealthy() || node.hasSnapshot(templateID) {
			continue
		}
		conn, err := r.NodeRegistry.GetConnection(node.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := forkdpb.NewForkDaemonClient(conn).CreateTemplate(ctx, &forkdpb.CreateTemplateRequest{
			TemplateId: templateID,
			Image:      image,
		}); err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}
		node.TemplateIDs = append(node.TemplateIDs, templateID)
		created++
	}
	if created == 0 && len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	return created, nil
}

func (r *SandboxPoolReconciler) nodeDistribution(templateID string) map[string]int32 {
	dist := make(map[string]int32)
	for _, n := range r.NodeRegistry.NodesWithTemplate(templateID) {
		dist[n.Name] = 1
	}
	return dist
}
```

(imports: add `errors` and `forkdpb "github.com/paperclipinc/sandbox/proto/forkd"`.)

Update `Reconcile` to use them: replace the deficit block and status lines:

```go
	templateID := pool.Spec.TemplateRef.Name
	readySnapshots := r.readySnapshotCount(templateID)
	desired := pool.Spec.Replicas

	if readySnapshots < desired {
		deficit := desired - readySnapshots
		logger.Info("snapshot deficit", "ready", readySnapshots, "desired", desired, "creating", deficit)
		created, err := r.createSnapshotsOnNodes(ctx, templateID, template.Spec.Image, deficit)
		if err != nil {
			logger.Error(err, "failed to create snapshots")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		readySnapshots += created
	}

	// Update status
	pool.Status.ReadySnapshots = readySnapshots
	pool.Status.TotalSnapshots = readySnapshots
	pool.Status.NodeDistribution = r.nodeDistribution(templateID)
```

(Note: return `nil` error with requeue on snapshot-creation failure; re-erroring causes double requeue.)

- [x] **Step 4: Run tests**

Run: `go test ./internal/controller/ -run TestPoolSnapshotAccounting -count=1 && go build ./...`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/controller/sandboxpool_controller.go internal/controller/forkd_client_test.go
git commit -m "feat: pool controller tracks and creates snapshots via forkd"
```

---

### Task 8: Envtest end-to-end: claim and fork reach Ready

**Files:**
- Create: `internal/controller/e2e_envtest_test.go` (package `controller_test`)
- Modify: `internal/controller/suite_test.go` (export the registry to tests, done in Task 2; also export a helper to start a fake forkd, see below)
- Possibly modify: `internal/controller/sandboxpool_controller_test.go` / claim test expectations that assumed stub behavior

The fake-forkd helper from Task 6 lives in package `controller` (internal test). For `controller_test` we need an exported equivalent; put it in a shared file:

- [x] **Step 1: Add an exported test helper**

Create `internal/controller/testsupport.go`:

```go
package controller

// Test support: used by envtest suites. Kept in the main package so external
// test packages (controller_test) can start fake forkd nodes.

import (
	"net"

	"github.com/paperclipinc/sandbox/internal/daemon"
	"github.com/paperclipinc/sandbox/internal/fork"
	"google.golang.org/grpc"
)

// StartFakeForkdNode runs an in-process forkd gRPC server backed by a
// MockEngine with the given templates, registers it in the registry, and
// returns a stop function.
func StartFakeForkdNode(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), err error) {
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	for _, tmpl := range templates {
		if err := engine.CreateTemplate(tmpl, tmpl, 0); err != nil {
			return nil, err
		}
	}
	srv := daemon.NewServer(engine, daemon.NewSandboxAPI("/tmp"))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	gs := grpc.NewServer()
	daemon.RegisterForkDaemonServer(gs, srv)

	go gs.Serve(lis)
	registry.Register(&NodeInfo{
		Name:         nodeName,
		Endpoint:     lis.Addr().String(),
		HTTPEndpoint: lis.Addr().String(), // tests only need a non-empty reachable-shaped value
		TemplateIDs:  templates,
		MaxSandboxes: 100,
	})
	return gs.Stop, nil
}
```

Then simplify Task 6's `startFakeForkd` test helper to reuse this if convenient (optional; duplication in tests is acceptable; do NOT block on it).

- [x] **Step 2: Write the failing e2e test**

`internal/controller/e2e_envtest_test.go`:

```go
package controller_test

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestClaimReachesReadyEndToEnd(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "e2e-node-1", "e2e-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "e2e-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "e2e-pool"},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "e2e-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxReady {
				if got.Status.Endpoint == "" {
					t.Fatal("ready claim has empty endpoint")
				}
				if got.Status.SandboxID == "" {
					t.Fatal("ready claim has empty sandboxID")
				}
				if got.Status.Node != "e2e-node-1" {
					t.Fatalf("node = %q, want e2e-node-1", got.Status.Node)
				}
				return
			}
			if got.Status.Phase == v1alpha1.SandboxFailed {
				t.Fatalf("claim failed: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("claim did not become Ready within 15s")
}
```

- [x] **Step 3: Run the controller suite**

Run: `make test-controller`
Expected: the new test passes. **If pre-existing tests (`TestSandboxClaim_CreateAndReconcile`, etc.) fail because they asserted the old stub behavior (claims failing or pending forever), read them and update the assertions to the new behavior: claims with no registered node should requeue as Pending, claims with a fake node should go Ready.** Do not delete assertions; update them to the now-correct semantics.

- [x] **Step 4: Commit**

```bash
git add internal/controller/
git commit -m "test: envtest e2e; SandboxClaim reaches Ready through fake forkd"
```

---

### Task 9: Forkd node discovery

The controller currently never learns about real forkd pods. Implement discovery: list pods labeled `app.kubernetes.io/component=forkd` in the controller's namespace, register `NodeInfo` from pod IPs, refresh capacity via gRPC `GetCapacity` heartbeats, prune stale nodes.

**Files:**
- Create: `internal/controller/forkd_discovery.go`
- Create: `internal/controller/forkd_discovery_test.go`
- Modify: `cmd/controller/main.go` (wire it into the manager)
- Modify: `deploy/controller/deployment.yaml` (RBAC: pods get/list/watch)
- Delete: the `StartDiscovery` TODO method in `node_registry.go` (superseded)

- [x] **Step 1: Write the failing test for the pure mapping**

`internal/controller/forkd_discovery_test.go`:

```go
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeInfoFromPod(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "forkd-abc12",
			Labels: map[string]string{"app.kubernetes.io/component": "forkd"},
		},
		Spec:   corev1.PodSpec{NodeName: "worker-1"},
		Status: corev1.PodStatus{PodIP: "10.0.3.7", Phase: corev1.PodRunning},
	}

	info, ok := NodeInfoFromPod(pod, 9090, 9091)
	if !ok {
		t.Fatal("expected ok")
	}
	if info.Name != "worker-1" {
		t.Fatalf("name = %q, want worker-1", info.Name)
	}
	if info.Endpoint != "10.0.3.7:9090" {
		t.Fatalf("endpoint = %q", info.Endpoint)
	}
	if info.HTTPEndpoint != "10.0.3.7:9091" {
		t.Fatalf("httpEndpoint = %q", info.HTTPEndpoint)
	}
}

func TestNodeInfoFromPodSkipsNotReady(t *testing.T) {
	for _, pod := range []corev1.Pod{
		{Status: corev1.PodStatus{PodIP: "", Phase: corev1.PodRunning}, Spec: corev1.PodSpec{NodeName: "w"}},
		{Status: corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodPending}, Spec: corev1.PodSpec{NodeName: "w"}},
		{Status: corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning}, Spec: corev1.PodSpec{NodeName: ""}},
	} {
		if _, ok := NodeInfoFromPod(pod, 9090, 9091); ok {
			t.Fatalf("expected not ok for pod %+v", pod.Status)
		}
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./internal/controller/ -run TestNodeInfoFromPod -count=1`
Expected: FAIL; `NodeInfoFromPod` undefined.

- [x] **Step 3: Implement `internal/controller/forkd_discovery.go`**

```go
package controller

import (
	"context"
	"fmt"
	"time"

	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const forkdComponentLabel = "app.kubernetes.io/component"

// ForkdDiscovery keeps the NodeRegistry in sync with running forkd pods.
// It lists labeled pods periodically, registers them, refreshes capacity via
// GetCapacity, and prunes nodes that stop heartbeating.
type ForkdDiscovery struct {
	Client    client.Client
	Registry  *NodeRegistry
	Namespace string        // namespace forkd runs in, e.g. "agent-run"
	Interval  time.Duration // default 15s
	GRPCPort  int           // default 9090
	HTTPPort  int           // default 9091
}

func (d *ForkdDiscovery) Start(ctx context.Context) error {
	if d.Interval == 0 {
		d.Interval = 15 * time.Second
	}
	if d.GRPCPort == 0 {
		d.GRPCPort = 9090
	}
	if d.HTTPPort == 0 {
		d.HTTPPort = 9091
	}
	ticker := time.NewTicker(d.Interval)
	defer ticker.Stop()
	for {
		d.sync(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (d *ForkdDiscovery) sync(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("forkd-discovery")

	var pods corev1.PodList
	if err := d.Client.List(ctx, &pods,
		client.InNamespace(d.Namespace),
		client.MatchingLabels{forkdComponentLabel: "forkd"},
	); err != nil {
		logger.Error(err, "list forkd pods")
		return
	}

	for _, pod := range pods.Items {
		info, ok := NodeInfoFromPod(pod, d.GRPCPort, d.HTTPPort)
		if !ok {
			continue
		}
		d.refreshCapacity(ctx, info)
		d.Registry.Register(info)
	}

	d.Registry.PruneStale(2 * time.Minute)
}

// refreshCapacity fills template/capacity fields via forkd's GetCapacity.
// Registration still happens if the call fails; SelectNode's health window
// and the next sync handle flapping pods.
func (d *ForkdDiscovery) refreshCapacity(ctx context.Context, info *NodeInfo) {
	conn, err := d.Registry.GetConnection(info.Name)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := forkdpb.NewForkDaemonClient(conn).GetCapacity(cctx, &forkdpb.GetCapacityRequest{})
	if err != nil {
		return
	}
	info.ActiveSandboxes = resp.ActiveSandboxes
	info.MaxSandboxes = resp.MaxSandboxes
	info.MemoryTotal = resp.MemoryTotalBytes
	info.MemoryUsed = resp.MemoryUsedBytes
	info.TemplateIDs = resp.TemplateIds
	info.SnapshotIDs = resp.SnapshotIds
}

// NodeInfoFromPod maps a forkd pod to a NodeInfo. Returns false when the pod
// is not running, has no IP, or has no node assignment yet.
func NodeInfoFromPod(pod corev1.Pod, grpcPort, httpPort int) (*NodeInfo, bool) {
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || pod.Spec.NodeName == "" {
		return nil, false
	}
	return &NodeInfo{
		Name:         pod.Spec.NodeName,
		Endpoint:     fmt.Sprintf("%s:%d", pod.Status.PodIP, grpcPort),
		HTTPEndpoint: fmt.Sprintf("%s:%d", pod.Status.PodIP, httpPort),
	}, true
}
```

Note: `refreshCapacity` calls `GetConnection(info.Name)` which requires the node registered first: register-then-refresh on first sight. Reorder in `sync`: `d.Registry.Register(info)` first, then `d.refreshCapacity(ctx, info)`, then `d.Registry.Register(info)` again to store refreshed fields (Register is an upsert). Implement it that way:

```go
	for _, pod := range pods.Items {
		info, ok := NodeInfoFromPod(pod, d.GRPCPort, d.HTTPPort)
		if !ok {
			continue
		}
		d.Registry.Register(info)
		d.refreshCapacity(ctx, info)
		d.Registry.Register(info)
	}
```

Delete `StartDiscovery` from `node_registry.go` (and its mock-node block; `cmd/controller` no longer calls it; verify with `grep -rn StartDiscovery`).

- [x] **Step 4: Wire into the manager (`cmd/controller/main.go`, after reconciler setup)**

```go
	discoveryNamespace := os.Getenv("FORKD_NAMESPACE")
	if discoveryNamespace == "" {
		discoveryNamespace = "agent-run"
	}
	if err := mgr.Add(&controller.ForkdDiscovery{
		Client:    mgr.GetClient(),
		Registry:  nodeRegistry,
		Namespace: discoveryNamespace,
	}); err != nil {
		logger.Error(err, "unable to add forkd discovery")
		os.Exit(1)
	}
```

- [x] **Step 5: RBAC: add pods to the ClusterRole in `deploy/controller/deployment.yaml`**

```yaml
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
```

- [x] **Step 6: Run tests and build**

Run: `go test ./internal/controller/ -run TestNodeInfoFromPod -count=1 && go build ./... && grep -rn StartDiscovery --include='*.go' .`
Expected: PASS, build OK, no remaining StartDiscovery references.

- [x] **Step 7: Commit**

```bash
git add internal/controller/ cmd/controller/main.go deploy/controller/deployment.yaml
git commit -m "feat: forkd pod discovery with capacity heartbeats"
```

---

### Task 10: Python SDK: align k8s mode with the forkd sandbox API

`Sandbox.exec`/`files.*` post to `{endpoint}/exec` and `{endpoint}/files/read` with no sandbox ID; forkd serves `POST /v1/exec` etc. and routes by the `sandbox` field. Fix the SDK to (a) hit `/v1/...`, (b) send `"sandbox": <sandboxID from claim status>`.

**Files:**
- Modify: `sdk/python/agent_run/sandbox.py`
- Test: `sdk/python/tests/test_sandbox.py` (append; follow the existing test style in that file; read it first)

- [x] **Step 1: Read the existing tests to match their fixtures**

Run: `sed -n 1,60p sdk/python/tests/test_sandbox.py`
Match whatever mocking approach is already used (the suite passes today without a cluster, so the k8s client is already fake-able).

- [x] **Step 2: Write the failing test (append, adapting fixture names to what Step 1 showed)**

```python
import httpx

from agent_run.sandbox import Sandbox
from agent_run.types import SandboxPhase


def _ready_sandbox(transport: httpx.MockTransport) -> Sandbox:
    sandbox = Sandbox(
        name="claim-1",
        namespace="default",
        pool="pool-1",
        api=None,  # not used when endpoint/phase pre-seeded
        _endpoint="10.0.3.7:9091",
        _phase=SandboxPhase.READY,
    )
    sandbox._sandbox_id = "sb-claim-1"
    sandbox._http = httpx.Client(transport=transport)
    return sandbox


def test_exec_targets_v1_and_sends_sandbox_id():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["json"] = __import__("json").loads(request.content)
        return httpx.Response(200, json={"exit_code": 0, "stdout": "hi\n", "stderr": "", "exec_time_ms": 1.0})

    result = _ready_sandbox(httpx.MockTransport(handler)).exec("echo hi")

    assert result.stdout == "hi\n"
    assert seen["url"] == "http://10.0.3.7:9091/v1/exec"
    assert seen["json"]["sandbox"] == "sb-claim-1"


def test_files_read_targets_v1_and_sends_sandbox_id():
    def handler(request: httpx.Request) -> httpx.Response:
        body = __import__("json").loads(request.content)
        assert body["sandbox"] == "sb-claim-1"
        assert str(request.url).endswith("/v1/files/read")
        return httpx.Response(200, json={"content": "data", "size": 4})

    content = _ready_sandbox(httpx.MockTransport(handler)).files.read("/workspace/x")
    assert content == "data"
```

- [x] **Step 3: Run to verify failure**

Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/test_sandbox.py -v -k 'targets_v1'`
Expected: FAIL; URLs missing `/v1`, no `sandbox` field, no `_sandbox_id` attribute.

- [x] **Step 4: Implement in `sandbox.py`**

1. In `Sandbox.__init__`, add `self._sandbox_id: Optional[str] = None`.
2. In `_wait_ready`, after reading status, add `self._sandbox_id = status.get("sandboxID")`.
3. Change `_base_url` to include the API prefix:

```python
    @property
    def _base_url(self) -> str:
        if not self._endpoint:
            self._wait_ready()
        return f"http://{self._endpoint}/v1"
```

4. Add a helper and use it in every request body:

```python
    @property
    def _sandbox_ref(self) -> str:
        if self._sandbox_id is None:
            self._wait_ready()
        return self._sandbox_id or self.name
```

5. `exec` payload gains `"sandbox": self._sandbox_ref`. Every `SandboxFiles` method body gains `"sandbox": self._sandbox._sandbox_ref` (read, read_bytes, write, list, remove, mkdir).
6. In `_wait_forks`, forks get `_endpoint` from fork info; also set `sandbox._sandbox_id = f.get("sandboxID")` on each constructed fork Sandbox.

- [x] **Step 5: Run the full Python suite**

Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -v`
Expected: PASS, including pre-existing tests. If pre-existing tests pinned the old URLs, update them to `/v1/...` + sandbox field; the old shape never worked against any server in this repo.

- [x] **Step 6: Commit**

```bash
git add sdk/python/
git commit -m "fix: Python SDK k8s mode speaks the forkd /v1 sandbox API"
```

---

### Task 11: Full-suite verification and docs truth pass

**Files:**
- Modify (if needed): `README.md`, `ROADMAP.md` status lines
- Modify: `.github/workflows/ci.yaml` (no change expected; verify generated proto code builds in CI)

- [x] **Step 1: Run everything**

```bash
go build ./... && go vet ./...
go test ./internal/fork/... ./internal/workspace/... ./internal/vsock/... ./internal/daemon/... -count=1
go test ./internal/controller/ -run 'TestRegister|TestSelectNode|TestNodesWith|TestFork|TestPool|TestNodeInfo' -count=1
make test-controller
cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -v && cd ../..
```

Expected: all PASS.

- [x] **Step 2: Docs truth pass**

Open `README.md` and `ROADMAP.md`. For each item this plan implemented, flip its status from "not implemented" to "implemented" ONLY if its test passed in Step 1:
- controller ↔ forkd gRPC (claim → Ready)
- pool snapshot accounting/creation
- node discovery
- SDK exec path alignment

Leave everything else (volume policies, secrets delivery into guest, networking, TS SDK, benchmarks) marked not implemented; this plan deliberately did not touch them.

- [x] **Step 3: Commit**

```bash
git add README.md ROADMAP.md
git commit -m "docs: update status to match wired control plane"
```

---

## Out of scope (separate plans, in ROADMAP priority order)

1. **Fork correctness on KVM** (RNG reseed, clock resync, secret-fork policy, network identity): needs guest agent changes + KVM CI; see `docs/fork-correctness.md`.
2. **forkd security hardening**: jailer, mTLS on the gRPC channel, authn on the HTTP sandbox API; see `docs/threat-model.md` §1/§3.
3. **Failure/GC semantics**: orphan sweeps, NodeLost, claim TTLs, controller-restart reconciliation.
4. **bench/ harness + honest comparison table.**
5. **Snapshot distribution** (content-addressed store, P2P).
6. **Talos/Hetzner reference platform.**
7. **Ergonomics:** MCP server interface, streaming exec + PTY, code-interpreter API shim, kubectl plugin, TypeScript SDK.
