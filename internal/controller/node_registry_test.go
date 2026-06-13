package controller

import (
	"testing"
	"time"
)

func TestRegisterOnZeroValueRegistry(t *testing.T) {
	var r NodeRegistry // zero value, nil map
	r.Register(&NodeInfo{Name: "n1", Endpoint: "10.0.0.1:9090", LastHeartbeat: time.Now()})

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

func TestRegisterCarriesConnectionForward(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(&NodeInfo{Name: "n1", Endpoint: "127.0.0.1:1"})
	conn, err := r.GetConnection("n1")
	if err != nil {
		t.Fatal(err)
	}

	// Re-register same endpoint: connection must be carried forward.
	r.Register(&NodeInfo{Name: "n1", Endpoint: "127.0.0.1:1"})
	conn2, err := r.GetConnection("n1")
	if err != nil {
		t.Fatal(err)
	}
	if conn2 != conn {
		t.Fatal("connection was not carried forward on same-endpoint re-register")
	}

	// Re-register with a NEW endpoint: old conn closed, fresh dial happens.
	r.Register(&NodeInfo{Name: "n1", Endpoint: "127.0.0.1:2"})
	conn3, err := r.GetConnection("n1")
	if err != nil {
		t.Fatal(err)
	}
	if conn3 == conn {
		t.Fatal("stale connection survived endpoint change")
	}
}

// TestNodeUnhealthyAfterProbeFailureThreshold asserts a node whose liveness
// probe (forkd GetCapacity) has failed at least probeFailureThreshold times in a
// row is treated as unhealthy and dropped from SelectNode, even though its
// heartbeat is fresh (its pod is still Running, but forkd is hung or the host
// died). A single failure must NOT flap the node out.
func TestNodeUnhealthyAfterProbeFailureThreshold(t *testing.T) {
	r := NewNodeRegistry()
	node := &NodeInfo{Name: "n1", Endpoint: "10.0.0.1:9090", LastHeartbeat: time.Now()}

	// One failure short of the threshold: still healthy (no single-blip flap).
	node.probeFailures = probeFailureThreshold - 1
	r.Register(node)
	if !r.NodeHealthy("n1") {
		t.Fatalf("node with %d/%d probe failures must still be healthy", node.probeFailures, probeFailureThreshold)
	}
	if _, err := r.SelectNode("", ""); err != nil {
		t.Fatalf("SelectNode below threshold: %v", err)
	}

	// At the threshold: unhealthy, dropped from scheduling.
	node = &NodeInfo{Name: "n1", Endpoint: "10.0.0.1:9090", LastHeartbeat: time.Now(), probeFailures: probeFailureThreshold}
	r.Register(node)
	if r.NodeHealthy("n1") {
		t.Fatal("node at the probe-failure threshold must be unhealthy")
	}
	if _, err := r.SelectNode("", ""); err == nil {
		t.Fatal("SelectNode must not place onto a node failing its liveness probe")
	}
}

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

func TestTemplateDigestRecordedAndReported(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(&NodeInfo{Name: "a", Endpoint: "10.0.0.1:9090", LastHeartbeat: time.Now()})

	r.AddTemplateWithDigest("a", "py", "abc123")

	if d, ok := r.TemplateDigest("py"); !ok || d != "abc123" {
		t.Fatalf("TemplateDigest = %q, %v; want abc123, true", d, ok)
	}

	// A template with no recorded digest reports not-found.
	r.AddTemplate("a", "node")
	if d, ok := r.TemplateDigest("node"); ok || d != "" {
		t.Fatalf("TemplateDigest(node) = %q, %v; want empty, false", d, ok)
	}
}
