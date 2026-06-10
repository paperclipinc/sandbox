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
