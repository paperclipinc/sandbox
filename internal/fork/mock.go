package fork

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paperclipinc/sandbox/internal/metering"
	"github.com/paperclipinc/sandbox/internal/volume"
	"github.com/paperclipinc/sandbox/internal/vsock"
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
	// terminated records every sandbox ID passed to Terminate, in call order,
	// so tests can assert a VM was reaped even after it leaves the live map.
	terminated []string
	// VsockDir overrides the root directory reported in ForkResult.VsockPath
	// (defaults to /tmp/agent-run-mock). Tests point it at a temp dir so a
	// fake agent can listen on the exact path the engine reports.
	VsockDir string
	// lastNetwork records the NetworkOpts of the most recent Fork call (nil if
	// the fork carried none). Tests assert the template's NetworkPolicy was
	// plumbed all the way through the Fork RPC to the engine.
	lastNetwork *NetworkOpts
	// lastVolumes records the volume specs of the most recent Fork call (nil if
	// the fork carried none). Tests assert the template's Volumes were plumbed
	// all the way through the Fork RPC to the engine.
	lastVolumes []volume.Spec
	// lastInitCommands records the init commands of the most recent
	// CreateTemplate call. Tests assert template.Spec.Init was plumbed all the
	// way through the CreateTemplate RPC to the engine.
	lastInitCommands []string
	// lastTemplateVolumes records the volume specs of the most recent
	// CreateTemplate call (nil if none). Tests assert the template's declared
	// volumes were plumbed through the CreateTemplate RPC to the engine.
	lastTemplateVolumes []volume.Spec
}

// LastInitCommands returns the init commands passed to the most recent
// CreateTemplate call, or nil if none has been recorded (or the last build
// carried none). It lets controller envtests assert template.Spec.Init reached
// the engine through the CreateTemplate RPC.
func (e *MockEngine) LastInitCommands() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastInitCommands
}

// LastForkNetwork returns the NetworkOpts passed to the most recent Fork call,
// or nil if none has been recorded (or the last fork carried no network). It
// lets controller envtests assert the egress policy and allowlist reached the
// engine through the Fork RPC.
func (e *MockEngine) LastForkNetwork() *NetworkOpts {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastNetwork
}

// LastForkVolumes returns the volume specs passed to the most recent Fork call,
// or nil if none has been recorded (or the last fork carried no volumes). It
// lets controller envtests assert the template's Volumes reached the engine
// through the Fork RPC.
func (e *MockEngine) LastForkVolumes() []volume.Spec {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastVolumes
}

// LastTemplateVolumes returns the volume specs passed to the most recent
// CreateTemplate call, or nil if none has been recorded. It lets controller
// envtests assert the template's declared volumes reached the engine through
// the CreateTemplate RPC.
func (e *MockEngine) LastTemplateVolumes() []volume.Spec {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastTemplateVolumes
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
		TemplateID:   snapshotID, // mock snapshot id IS the template id
		SnapshotID:   snapshotID,
		Endpoint:     endpoint,
		CreatedAt:    time.Now(),
		MemoryUnique: 265 * 1024,        // ~265KB per fork
		MemoryShared: 256 * 1024 * 1024, // ~256MB shared base
	}

	e.mu.Lock()
	e.sandboxes[sandboxID] = sandbox
	e.lastNetwork = opts.Network
	e.lastVolumes = opts.Volumes
	e.mu.Unlock()

	elapsed := time.Since(start)

	return &ForkResult{
		SandboxID:    sandboxID,
		Endpoint:     endpoint,
		ForkTimeMs:   float64(elapsed.Microseconds()) / 1000.0,
		MemoryUnique: sandbox.MemoryUnique,
		MemoryShared: sandbox.MemoryShared,
		VsockPath:    e.vsockPath(sandboxID),
		// Mirror the real engine's mount table so the daemon delivery path can be
		// exercised without KVM: device names follow attach order (vdb, vdc, ...)
		// and the read-only flag is the resolved drive policy (Share or explicit
		// readOnly). The mock does not prepare backings, so it derives the table
		// directly from the requested specs.
		VolumeMounts: mockMountTable(opts.Volumes),
	}, nil
}

// mockMountTable builds the guest mount table the mock reports, matching the
// real engine's device ordering and resolved read-only policy. Returns nil for
// no volumes.
func mockMountTable(specs []volume.Spec) []vsock.VolumeMountEntry {
	if len(specs) == 0 {
		return nil
	}
	prepared := make([]volume.Prepared, 0, len(specs))
	for _, s := range specs {
		prepared = append(prepared, volume.Prepared{
			Name:      s.Name,
			MountPath: s.MountPath,
			ReadOnly:  driveReadOnly(s),
		})
	}
	return volumeMountTable(prepared)
}

func (e *MockEngine) CreateTemplate(id string, image string, initCommands []string, volumes []volume.Spec) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastInitCommands = initCommands
	e.lastTemplateVolumes = volumes
	e.templates[id] = &Template{
		ID:          id,
		Image:       image,
		SnapshotDir: fmt.Sprintf("/tmp/agent-run-mock/templates/%s", id),
		CreatedAt:   time.Now(),
		Ready:       true,
	}
	return nil
}

// InjectSandbox seeds a live sandbox directly into the engine with a chosen
// created-at, bypassing Fork. Tests use it to plant an orphan VM (no backing
// claim) with a controlled uptime so the GC orphan sweep can be exercised.
func (e *MockEngine) InjectSandbox(id string, createdAt time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sandboxes[id] = &Sandbox{
		ID:        id,
		CreatedAt: createdAt,
	}
}

func (e *MockEngine) Terminate(sandboxID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.terminated = append(e.terminated, sandboxID)
	if _, ok := e.sandboxes[sandboxID]; !ok {
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}
	delete(e.sandboxes, sandboxID)
	return nil
}

// TerminatedIDs returns the sandbox IDs passed to Terminate, in call order.
// Tests use it to assert a VM was reaped.
func (e *MockEngine) TerminatedIDs() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, len(e.terminated))
	copy(out, e.terminated)
	return out
}

// ListSandboxes returns a record for every sandbox this mock engine
// currently holds. Order is unspecified.
func (e *MockEngine) ListSandboxes() []SandboxRecord {
	e.mu.RLock()
	defer e.mu.RUnlock()

	records := make([]SandboxRecord, 0, len(e.sandboxes))
	for _, s := range e.sandboxes {
		records = append(records, SandboxRecord{ID: s.ID, CreatedAt: s.CreatedAt})
	}
	return records
}

func (e *MockEngine) GetCapacity() Capacity {
	e.mu.RLock()
	templateIDs := make([]string, 0, len(e.templates))
	for id := range e.templates {
		templateIDs = append(templateIDs, id)
	}
	// Route through the same CoW-aware aggregation as the real engine so mock
	// tests see N forks of one template count the shared region once.
	samples := make([]metering.Sample, 0, len(e.sandboxes))
	for _, s := range e.sandboxes {
		samples = append(samples, metering.Sample{
			ID:           s.ID,
			Template:     s.TemplateID,
			MemoryUnique: s.MemoryUnique,
			MemoryShared: s.MemoryShared,
		})
	}
	active := int32(len(e.sandboxes))
	e.mu.RUnlock()

	report := metering.Aggregate(samples)

	return Capacity{
		ActiveSandboxes: active,
		MaxSandboxes:    1000,
		MemoryTotal:     8 * 1024 * 1024 * 1024,
		MemoryUsed:      report.UsedCoWAware,
		MemoryShared:    report.SharedOnceTotal(),
		TemplateIDs:     templateIDs,
		KVMAvailable:    false,
	}
}

// Metering returns the CoW-aware report for the mock's live sandboxes. The mock
// prepares no real backing files, so disk fields are zero; memory is aggregated
// exactly like the real engine (N forks of one template share the region once).
func (e *MockEngine) Metering() metering.Report {
	e.mu.RLock()
	samples := make([]metering.Sample, 0, len(e.sandboxes))
	for _, s := range e.sandboxes {
		samples = append(samples, metering.Sample{
			ID:           s.ID,
			Template:     s.TemplateID,
			MemoryUnique: s.MemoryUnique,
			MemoryShared: s.MemoryShared,
		})
	}
	e.mu.RUnlock()
	return metering.Aggregate(samples)
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
		TemplateID:   source.TemplateID,
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
