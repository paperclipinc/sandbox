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
