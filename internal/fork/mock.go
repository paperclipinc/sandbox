package fork

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MockEngine simulates fork operations without KVM.
// Used for local development on macOS and in CI (kind clusters).
type MockEngine struct {
	mu        sync.RWMutex
	templates map[string]*Template
	sandboxes map[string]*Sandbox
	counter   atomic.Int64
	// Simulated fork latency
	ForkDelay time.Duration
	// PausedSources records source sandbox IDs that were "paused" during ForkRunning.
	PausedSources []string
	// VsockDir overrides the root directory reported in ForkResult.VsockPath
	// (defaults to /tmp/agent-run-mock). Tests point it at a temp dir so a
	// fake agent can listen on the exact path the engine reports.
	VsockDir string
}

// vsockPath reports the vsock UDS path for a sandbox, rooted at VsockDir.
func (e *MockEngine) vsockPath(sandboxID string) string {
	vsockDir := e.VsockDir
	if vsockDir == "" {
		vsockDir = "/tmp/agent-run-mock"
	}
	return fmt.Sprintf("%s/sandboxes/%s/vsock.sock", vsockDir, sandboxID)
}

func NewMockEngine() *MockEngine {
	return &MockEngine{
		templates: make(map[string]*Template),
		sandboxes: make(map[string]*Sandbox),
		ForkDelay: 800 * time.Microsecond,
	}
}

func (e *MockEngine) Fork(snapshotID, sandboxID string, opts ForkOpts) (*ForkResult, error) {
	start := time.Now()

	e.mu.RLock()
	_, ok := e.findTemplateBySnapshot(snapshotID)
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("snapshot %s not found", snapshotID)
	}

	// Simulate fork latency
	time.Sleep(e.ForkDelay)

	id := e.counter.Add(1)
	endpoint := fmt.Sprintf("127.0.0.1:%d", 10000+id)

	sandbox := &Sandbox{
		ID:           sandboxID,
		SnapshotID:   snapshotID,
		Endpoint:     endpoint,
		CreatedAt:    time.Now(),
		MemoryUnique: 265 * 1024,        // ~265KB per fork
		MemoryShared: 256 * 1024 * 1024, // ~256MB shared base
	}

	e.mu.Lock()
	e.sandboxes[sandboxID] = sandbox
	e.mu.Unlock()

	elapsed := time.Since(start)

	return &ForkResult{
		SandboxID:    sandboxID,
		Endpoint:     endpoint,
		ForkTimeMs:   float64(elapsed.Microseconds()) / 1000.0,
		MemoryUnique: sandbox.MemoryUnique,
		MemoryShared: sandbox.MemoryShared,
		VsockPath:    e.vsockPath(sandboxID),
	}, nil
}

func (e *MockEngine) CreateTemplate(id string, rootfsPath string, initWaitSecs int) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.templates[id] = &Template{
		ID:          id,
		Image:       rootfsPath,
		SnapshotDir: fmt.Sprintf("/tmp/agent-run-mock/templates/%s", id),
		CreatedAt:   time.Now(),
		Ready:       true,
	}
	return nil
}

func (e *MockEngine) Terminate(sandboxID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := e.sandboxes[sandboxID]; !ok {
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}
	delete(e.sandboxes, sandboxID)
	return nil
}

func (e *MockEngine) GetCapacity() Capacity {
	e.mu.RLock()
	defer e.mu.RUnlock()

	templateIDs := make([]string, 0, len(e.templates))
	for id := range e.templates {
		templateIDs = append(templateIDs, id)
	}

	return Capacity{
		ActiveSandboxes: int32(len(e.sandboxes)),
		MaxSandboxes:    1000,
		MemoryTotal:     8 * 1024 * 1024 * 1024,
		MemoryUsed:      int64(len(e.sandboxes)) * 265 * 1024,
		MemoryShared:    256 * 1024 * 1024,
		TemplateIDs:     templateIDs,
		KVMAvailable:    false,
	}
}

func (e *MockEngine) findTemplateBySnapshot(snapshotID string) (*Template, bool) {
	for _, t := range e.templates {
		if t.ID == snapshotID {
			return t, true
		}
	}
	return nil, false
}

// ForkRunning simulates checkpoint-and-fork of a running sandbox.
func (e *MockEngine) ForkRunning(sourceSandboxID, newSandboxID string, pauseSource bool) (*ForkResult, error) {
	start := time.Now()

	e.mu.RLock()
	source, ok := e.sandboxes[sourceSandboxID]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", sourceSandboxID)
	}

	if pauseSource {
		e.mu.Lock()
		e.PausedSources = append(e.PausedSources, sourceSandboxID)
		e.mu.Unlock()
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
		VsockPath:    e.vsockPath(newSandboxID),
	}, nil
}
