package fork

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMockEngine_CreateTemplate(t *testing.T) {
	engine := NewMockEngine()

	if err := engine.CreateTemplate("python", "python:3.12-slim", 0); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	cap := engine.GetCapacity()
	if len(cap.TemplateIDs) != 1 || cap.TemplateIDs[0] != "python" {
		t.Errorf("expected template 'python', got %v", cap.TemplateIDs)
	}
}

func TestMockEngine_Fork(t *testing.T) {
	engine := NewMockEngine()
	engine.CreateTemplate("python", "python:3.12-slim", 0)

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
	engine.CreateTemplate("python", "python:3.12-slim", 0)
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
	engine.CreateTemplate("python", "python:3.12-slim", 0)

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
	engine.CreateTemplate("python", "python:3.12-slim", 0)

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
	engine.CreateTemplate("python", "python:3.12-slim", 0)

	for i := 0; i < 10; i++ {
		engine.Fork("python", "sandbox-"+string(rune('0'+i)), ForkOpts{})
	}

	cap := engine.GetCapacity()
	expectedUsed := int64(10) * 265 * 1024
	if cap.MemoryUsed != expectedUsed {
		t.Errorf("expected %d bytes used, got %d", expectedUsed, cap.MemoryUsed)
	}
}
