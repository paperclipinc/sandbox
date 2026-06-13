package controller

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/daemon"
	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/volume"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// startFakeForkd runs a real forkd gRPC server with a MockEngine on 127.0.0.1:0
// and returns its address and engine.
func startFakeForkd(t *testing.T, templates ...string) (string, *fork.MockEngine) {
	t.Helper()
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	for _, tmpl := range templates {
		if err := engine.CreateTemplate(tmpl, tmpl, nil, nil); err != nil {
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
	result, err := r.forkOnNode(context.Background(), node, "py", "sb-1", map[string]string{"A": "1"}, map[string]string{"S": "x"}, nil, nil, "tok-sb-1", nil)
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

// TestForkOnNodePlumbsNetworkPolicy asserts the template NetworkPolicy reaches
// the engine through the Fork RPC: the egress policy and allowlist are recorded
// by the MockEngine. A name-based allow entry is passed through unchanged (the
// daemon splits it; forkd does not reject it here).
func TestForkOnNodePlumbsNetworkPolicy(t *testing.T) {
	addr, engine := startFakeForkd(t, "py")

	registry := NewNodeRegistry()
	registry.Register(&NodeInfo{Name: "n1", Endpoint: addr, HTTPEndpoint: "10.0.0.1:9091", TemplateIDs: []string{"py"}})
	node, err := registry.SelectNode("py", "")
	if err != nil {
		t.Fatal(err)
	}

	policy := &v1alpha1.NetworkPolicy{
		Egress: v1alpha1.EgressDeny,
		Allow:  []string{"10.0.0.5:443", "api.example.com:443"},
	}
	r := &SandboxClaimReconciler{NodeRegistry: registry}
	if _, err := r.forkOnNode(context.Background(), node, "py", "sb-net", nil, nil, policy, nil, "tok", nil); err != nil {
		t.Fatalf("forkOnNode: %v", err)
	}

	got := engine.LastForkNetwork()
	if got == nil {
		t.Fatal("engine recorded no NetworkOpts; policy was not plumbed through")
	}
	if got.EgressPolicy != string(v1alpha1.EgressDeny) {
		t.Fatalf("egress policy = %q, want deny", got.EgressPolicy)
	}
	want := []string{"10.0.0.5:443", "api.example.com:443"}
	if len(got.AllowList) != len(want) {
		t.Fatalf("allow list = %v, want %v", got.AllowList, want)
	}
	for i, e := range want {
		if got.AllowList[i] != e {
			t.Fatalf("allow[%d] = %q, want %q", i, got.AllowList[i], e)
		}
	}
}

// TestForkOnNodeNilNetworkPolicy confirms a template without a NetworkPolicy
// sends no NetworkConfig and the engine records no NetworkOpts.
func TestForkOnNodeNilNetworkPolicy(t *testing.T) {
	addr, engine := startFakeForkd(t, "py")

	registry := NewNodeRegistry()
	registry.Register(&NodeInfo{Name: "n1", Endpoint: addr, HTTPEndpoint: "10.0.0.1:9091", TemplateIDs: []string{"py"}})
	node, err := registry.SelectNode("py", "")
	if err != nil {
		t.Fatal(err)
	}

	r := &SandboxClaimReconciler{NodeRegistry: registry}
	if _, err := r.forkOnNode(context.Background(), node, "py", "sb-nonet", nil, nil, nil, nil, "tok", nil); err != nil {
		t.Fatalf("forkOnNode: %v", err)
	}
	if got := engine.LastForkNetwork(); got != nil {
		t.Fatalf("engine recorded NetworkOpts %+v for a template with no policy", got)
	}
}

func TestForkRunningOnNode(t *testing.T) {
	addr, _ := startFakeForkd(t, "py")
	registry := NewNodeRegistry()
	registry.Register(&NodeInfo{Name: "n1", Endpoint: addr, HTTPEndpoint: "10.0.0.1:9091", TemplateIDs: []string{"py"}})
	node, err := registry.SelectNode("py", "")
	if err != nil {
		t.Fatal(err)
	}

	claimRec := &SandboxClaimReconciler{NodeRegistry: registry}
	if _, err := claimRec.forkOnNode(context.Background(), node, "py", "parent", nil, nil, nil, nil, "tok-parent", nil); err != nil {
		t.Fatal(err)
	}

	forkRec := &SandboxForkReconciler{NodeRegistry: registry}
	result, err := forkRec.forkRunningOnNode(context.Background(), node, "parent", "child", true, nil, "tok-child")
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
	node, err := registry.SelectNode("", "")
	if err != nil {
		t.Fatal(err)
	}

	r := &SandboxClaimReconciler{NodeRegistry: registry}
	if _, err := r.forkOnNode(context.Background(), node, "missing", "sb", nil, nil, nil, nil, "tok-sb", nil); err == nil {
		t.Fatal("expected error")
	} else if !isNotFound(err) {
		t.Fatalf("expected NotFound through the wrap, got: %v", err)
	}
}

func TestIsNotFound(t *testing.T) {
	wrapped := fmt.Errorf("forkd fork on n1: %w", status.Error(codes.NotFound, "snapshot missing"))
	if !isNotFound(wrapped) {
		t.Fatal("wrapped NotFound should be detected")
	}
	if isNotFound(fmt.Errorf("plain error")) {
		t.Fatal("plain error is not NotFound")
	}
	if isNotFound(fmt.Errorf("wrap: %w", status.Error(codes.Internal, "boom"))) {
		t.Fatal("Internal is not NotFound")
	}
}

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

	initCommands := []string{"echo ready"}
	templateVols := []v1alpha1.SandboxVolume{
		{Name: "data", Size: "64Mi", MountPath: "/data", ForkPolicy: v1alpha1.ForkPolicyFresh},
	}
	created, err := r.createSnapshotsOnNodes(context.Background(), "py-tmpl", "python:3.12-slim", initCommands, templateVols, nil, 5)
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
	// The init commands must flow from template.Spec.Init through the
	// CreateTemplate RPC to the engine; without that plumbing template.Spec.Init
	// silently never reaches the VM build. Assert the engine that actually built
	// the template received them.
	if got := engineWithout.LastInitCommands(); !reflect.DeepEqual(got, initCommands) {
		t.Fatalf("engine init commands = %v, want %v", got, initCommands)
	}
	// The template's declared volumes must flow through the CreateTemplate RPC
	// to the engine so the snapshot bakes a placeholder drive per volume.
	gotVols := engineWithout.LastTemplateVolumes()
	if len(gotVols) != 1 || gotVols[0].Name != "data" || gotVols[0].Policy != volume.ForkPolicyFresh {
		t.Fatalf("engine template volumes = %+v, want one Fresh volume named data", gotVols)
	}
	if got := r.readySnapshotCount("py-tmpl"); got != 2 {
		t.Fatalf("ready after create = %d, want 2", got)
	}
}
