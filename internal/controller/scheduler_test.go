package controller

import (
	"errors"
	"testing"
	"time"
)

const gib = int64(1024 * 1024 * 1024)

// warmNode builds a healthy node holding templateID with the given memory
// budget/usage and a per-template estimate (shared-once, avg unique, forks).
func warmNode(name string, total, used int64, templateID string, sharedOnce, avgUnique int64, forks int32) *NodeInfo {
	return &NodeInfo{
		Name:          name,
		Endpoint:      name + ":9090",
		MemoryTotal:   total,
		MemoryUsed:    used,
		TemplateIDs:   []string{templateID},
		LastHeartbeat: time.Now(),
		TemplateEstimates: map[string]TemplateCapacity{
			templateID: {
				TemplateID:         templateID,
				SharedOnceBytes:    sharedOnce,
				AvgForkUniqueBytes: avgUnique,
				ForkCount:          forks,
			},
		},
	}
}

// coldNode builds a healthy node with a budget but no knowledge of templateID.
func coldNode(name string, total, used int64) *NodeInfo {
	return &NodeInfo{
		Name:          name,
		Endpoint:      name + ":9090",
		MemoryTotal:   total,
		MemoryUsed:    used,
		LastHeartbeat: time.Now(),
	}
}

func TestSelectNodeAdmitsWhenForkFits(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(warmNode("n1", 4*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 3))

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "n1" {
		t.Fatalf("got %q want n1", node.Name)
	}
}

func TestSelectNodeNoCapacityWhenAllFull(t *testing.T) {
	r := NewNodeRegistry()
	// Both nodes are full: used == total, a cold or warm placement cannot fit.
	r.Register(warmNode("n1", 2*gib, 2*gib, "py", 256*1024*1024, 8*1024*1024, 1))
	r.Register(coldNode("n2", 2*gib, 2*gib))

	_, err := r.SelectNode("py", "")
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
}

func TestSelectNodePacksWarmHolderOverColdEmpty(t *testing.T) {
	r := NewNodeRegistry()
	// Cold node has the MOST free memory but does not hold the template. The
	// warm holder still fits, so packing prefers it (maximize CoW sharing).
	r.Register(coldNode("cold", 64*gib, 0))
	r.Register(warmNode("warm", 8*gib, 2*gib, "py", 256*1024*1024, 8*1024*1024, 5))

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "warm" {
		t.Fatalf("got %q want warm (packing prefers warm holder)", node.Name)
	}
}

func TestSelectNodePacksDenserWarmThenSpills(t *testing.T) {
	r := NewNodeRegistry()
	// Two warm holders. dense runs more forks and still fits -> pack it. Then
	// fill dense so it no longer admits and confirm we spill to sparse.
	dense := warmNode("dense", 8*gib, 4*gib, "py", 256*1024*1024, 8*1024*1024, 20)
	sparse := warmNode("sparse", 8*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 2)
	r.Register(dense)
	r.Register(sparse)

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "dense" {
		t.Fatalf("got %q want dense (pack the denser holder)", node.Name)
	}

	// Now fill dense to capacity: it no longer admits, spill to sparse.
	full := warmNode("dense", 8*gib, 8*gib, "py", 256*1024*1024, 8*1024*1024, 20)
	r.Register(full)
	node, err = r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode after fill: %v", err)
	}
	if node.Name != "sparse" {
		t.Fatalf("got %q want sparse (spill when dense is full)", node.Name)
	}
}

func TestSelectNodeOvercommitAdmitsMore(t *testing.T) {
	r := NewNodeRegistry()
	// Cold placement cost = 256 MiB shared + 8 MiB unique = 264 MiB. Node has
	// 200 MiB free at factor 1.0 (does not admit) but 200+1024 MiB at 2.0.
	const free = 200 * 1024 * 1024
	total := int64(2 * gib)
	used := total - free
	r.Register(coldNode("n1", total, used))
	// Seed a cross-node estimate for "py" via a separate warm node that is
	// permanently full (used far exceeds even 2x its budget) so it can never be
	// chosen, letting n1 compute a cold cost for py from the shared estimate.
	full := warmNode("full", gib, 4*gib, "py", 256*1024*1024, 8*1024*1024, 1)
	r.Register(full)

	if _, err := r.SelectNode("py", ""); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("at factor 1.0 expected ErrNoCapacity, got %v", err)
	}

	r.SetOvercommitFactor(2.0)
	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("at factor 2.0 SelectNode: %v", err)
	}
	if node.Name != "n1" {
		t.Fatalf("got %q want n1 (overcommit admits)", node.Name)
	}
}

func TestSelectNodeBypassesPreferredThatDoesNotFit(t *testing.T) {
	r := NewNodeRegistry()
	// Preferred node is full; another node fits. Preference is bypassed.
	r.Register(warmNode("pref", 2*gib, 2*gib, "py", 256*1024*1024, 8*1024*1024, 1))
	r.Register(warmNode("other", 8*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 1))

	node, err := r.SelectNode("py", "pref")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "other" {
		t.Fatalf("got %q want other (preferred full, bypassed)", node.Name)
	}
}

func TestSelectNodeHonorsPreferredThatFits(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(warmNode("pref", 8*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 1))
	r.Register(warmNode("denser", 8*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 50))

	node, err := r.SelectNode("py", "pref")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "pref" {
		t.Fatalf("got %q want pref (preferred fits, honored)", node.Name)
	}
}

func TestSelectNodeDeterministicTieBreak(t *testing.T) {
	r := NewNodeRegistry()
	// Two cold nodes with identical free memory: pick by name (alpha < beta).
	r.Register(coldNode("beta", 8*gib, 1*gib))
	r.Register(coldNode("alpha", 8*gib, 1*gib))

	for i := 0; i < 5; i++ {
		node, err := r.SelectNode("py", "")
		if err != nil {
			t.Fatalf("SelectNode: %v", err)
		}
		if node.Name != "alpha" {
			t.Fatalf("iteration %d: got %q want alpha (deterministic tie-break)", i, node.Name)
		}
	}
}

func TestSelectNodeNoNodesDistinctFromNoCapacity(t *testing.T) {
	r := NewNodeRegistry()
	_, err := r.SelectNode("py", "")
	if err == nil {
		t.Fatal("expected an error for empty registry")
	}
	if errors.Is(err, ErrNoCapacity) {
		t.Fatal("empty registry must NOT be ErrNoCapacity")
	}
}

func TestSelectNodeUnknownBudgetTreatedUnlimited(t *testing.T) {
	r := NewNodeRegistry()
	// A node reporting MemoryTotal 0 (meminfo unavailable, e.g. darwin/dev)
	// must still be selectable so dev and mock paths keep working.
	r.Register(&NodeInfo{Name: "dev", Endpoint: "dev:9090", LastHeartbeat: time.Now()})
	node, err := r.SelectNode("", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "dev" {
		t.Fatalf("got %q want dev", node.Name)
	}
}
