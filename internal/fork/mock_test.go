package fork

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMockEngine_CreateTemplate(t *testing.T) {
	engine := NewMockEngine()

	if err := engine.CreateTemplate("python", "python:3.12-slim", nil, nil); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	cap := engine.GetCapacity()
	if len(cap.TemplateIDs) != 1 || cap.TemplateIDs[0] != "python" {
		t.Errorf("expected template 'python', got %v", cap.TemplateIDs)
	}
}

func TestMockEngine_CapacityReportsTotalAndEstimate(t *testing.T) {
	engine := NewMockEngine()
	engine.CreateTemplate("python", "python:3.12-slim", nil, nil)

	cap := engine.GetCapacity()
	if cap.MemoryTotal != 16*1024*1024*1024 {
		t.Errorf("MemoryTotal: got %d want 16 GiB", cap.MemoryTotal)
	}
	// Even with no forks yet, a known template carries a cold-start estimate so
	// envtest scheduling has a non-zero budget to bin-pack against.
	var found bool
	for _, est := range cap.TemplateEstimates {
		if est.TemplateID == "python" {
			found = true
			if est.SharedOnceBytes <= 0 || est.AvgForkUniqueBytes <= 0 {
				t.Errorf("python estimate has non-positive bytes: %+v", est)
			}
		}
	}
	if !found {
		t.Errorf("no estimate for template python: %+v", cap.TemplateEstimates)
	}

	engine.SetMemoryTotal(4 * 1024 * 1024 * 1024)
	if got := engine.GetCapacity().MemoryTotal; got != 4*1024*1024*1024 {
		t.Errorf("SetMemoryTotal: got %d want 4 GiB", got)
	}
}

func TestMockEngine_Fork(t *testing.T) {
	engine := NewMockEngine()
	engine.CreateTemplate("python", "python:3.12-slim", nil, nil)

	result, err := engine.Fork("python", "sandbox-1", ForkOpts{})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	if result.SandboxID != "sandbox-1" {
		t.Errorf("expected sandbox-1, got %s", result.SandboxID)
	}
	if result.Endpoint == "" {
		t.Error("expected non-empty endpoint")
	}
	if result.ForkTimeMs <= 0 {
		t.Error("expected positive fork time")
	}
	if result.MemoryUnique != 265*1024 {
		t.Errorf("expected ~265KB unique, got %d", result.MemoryUnique)
	}

	cap := engine.GetCapacity()
	if cap.ActiveSandboxes != 1 {
		t.Errorf("expected 1 active sandbox, got %d", cap.ActiveSandboxes)
	}
}

func TestMockEngine_ForkUnknownSnapshot(t *testing.T) {
	engine := NewMockEngine()

	_, err := engine.Fork("nonexistent", "sandbox-1", ForkOpts{})
	if err == nil {
		t.Fatal("expected error for unknown snapshot")
	}
}

func TestMockEngine_Terminate(t *testing.T) {
	engine := NewMockEngine()
	engine.CreateTemplate("python", "python:3.12-slim", nil, nil)
	engine.Fork("python", "sandbox-1", ForkOpts{})

	if err := engine.Terminate("sandbox-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	cap := engine.GetCapacity()
	if cap.ActiveSandboxes != 0 {
		t.Errorf("expected 0 active sandboxes, got %d", cap.ActiveSandboxes)
	}
}

func TestMockEngine_TerminateUnknown(t *testing.T) {
	engine := NewMockEngine()

	if err := engine.Terminate("nonexistent"); err == nil {
		t.Fatal("expected error for unknown sandbox")
	}
}

func TestMockEngine_ConcurrentForks(t *testing.T) {
	engine := NewMockEngine()
	engine.ForkDelay = 0
	engine.CreateTemplate("python", "python:3.12-slim", nil, nil)

	const n = 100
	var wg sync.WaitGroup
	errors := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sandbox-concurrent-%d", i)
			_, err := engine.Fork("python", id, ForkOpts{})
			if err != nil {
				errors <- err
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent fork error: %v", err)
	}

	cap := engine.GetCapacity()
	if cap.ActiveSandboxes != n {
		t.Errorf("expected %d active sandboxes, got %d", n, cap.ActiveSandboxes)
	}
}

func TestMockEngine_ForkLatency(t *testing.T) {
	engine := NewMockEngine()
	engine.ForkDelay = 500 * time.Microsecond
	engine.CreateTemplate("python", "python:3.12-slim", nil, nil)

	result, err := engine.Fork("python", "sandbox-1", ForkOpts{})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	if result.ForkTimeMs < 0.3 {
		t.Errorf("fork too fast (%.3fms), expected >= 0.5ms", result.ForkTimeMs)
	}
}

func TestMockEngine_MemoryAccounting(t *testing.T) {
	engine := NewMockEngine()
	engine.ForkDelay = 0
	engine.CreateTemplate("python", "python:3.12-slim", nil, nil)

	for i := 0; i < 10; i++ {
		engine.Fork("python", "sandbox-"+string(rune('0'+i)), ForkOpts{})
	}

	cap := engine.GetCapacity()
	// CoW-aware: ten forks of one template count the 256MiB shared region ONCE,
	// plus 265KiB unique per fork. The naive sum would be 10*(265KiB+256MiB).
	const (
		uniquePerFork = int64(265 * 1024)
		sharedOnce    = int64(256 * 1024 * 1024)
	)
	expectedUsed := 10*uniquePerFork + sharedOnce
	if cap.MemoryUsed != expectedUsed {
		t.Errorf("expected %d bytes used (CoW-aware), got %d", expectedUsed, cap.MemoryUsed)
	}
	if cap.MemoryShared != sharedOnce {
		t.Errorf("expected shared %d (counted once), got %d", sharedOnce, cap.MemoryShared)
	}
}

func TestMockEngine_ListSandboxes(t *testing.T) {
	engine := NewMockEngine()
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "python:3.12-slim", nil, nil); err != nil {
		t.Fatal(err)
	}

	if recs := engine.ListSandboxes(); len(recs) != 0 {
		t.Fatalf("ListSandboxes on empty engine = %d records, want 0", len(recs))
	}

	if _, err := engine.Fork("py", "sb-a", ForkOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Fork("py", "sb-b", ForkOpts{}); err != nil {
		t.Fatal(err)
	}

	recs := engine.ListSandboxes()
	if len(recs) != 2 {
		t.Fatalf("ListSandboxes = %d records, want 2", len(recs))
	}
	got := map[string]time.Time{}
	for _, r := range recs {
		got[r.ID] = r.CreatedAt
	}
	for _, id := range []string{"sb-a", "sb-b"} {
		ts, ok := got[id]
		if !ok {
			t.Fatalf("ListSandboxes missing %s", id)
		}
		if ts.IsZero() {
			t.Fatalf("ListSandboxes %s has zero CreatedAt", id)
		}
	}

	// Live forks and terminations are reflected.
	if _, err := engine.ForkRunning("sb-a", "sb-c", false); err != nil {
		t.Fatal(err)
	}
	if err := engine.Terminate("sb-b"); err != nil {
		t.Fatal(err)
	}

	recs = engine.ListSandboxes()
	live := map[string]bool{}
	for _, r := range recs {
		live[r.ID] = true
	}
	if !live["sb-a"] || !live["sb-c"] || live["sb-b"] || len(recs) != 2 {
		t.Fatalf("ListSandboxes after fork/terminate = %v, want sb-a and sb-c only", live)
	}
}

func TestMockForkRunning(t *testing.T) {
	e := NewMockEngine()
	e.ForkDelay = 0
	if err := e.CreateTemplate("py", "python:3.12-slim", nil, nil); err != nil {
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
	if len(e.PausedSources) != 1 || e.PausedSources[0] != parent.SandboxID {
		t.Fatalf("PausedSources = %v, want [%s]", e.PausedSources, parent.SandboxID)
	}

	if _, err := e.ForkRunning("nope", "child2", false); err == nil {
		t.Fatal("expected error for unknown source sandbox")
	}
}
