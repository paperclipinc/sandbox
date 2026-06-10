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
		if err := engine.CreateTemplate(tmpl, tmpl, 0); err != nil {
			t.Fatal(err)
		}
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
