package fork

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paperclipinc/mitos/internal/metering"
	"github.com/paperclipinc/mitos/internal/volume"
	"github.com/paperclipinc/mitos/internal/vsock"
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
	// (defaults to /tmp/mitos-mock). Tests point it at a temp dir so a
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
	// memoryTotalBytes is the fixed node memory budget GetCapacity reports so
	// envtest scheduling has a non-zero capacity to bin-pack against. Defaults
	// to 16 GiB; SetMemoryTotal overrides it (Task 3 envtests shrink it to
	// force capacity exhaustion).
	memoryTotalBytes int64
	// pulls records every PullTemplate call the mock received, in call order, so
	// distribution tests can assert the controller issued a pull (source URL +
	// digest) instead of a second build. The pull token is NEVER recorded: only
	// its presence/length, so a test can confirm a token was carried without the
	// value touching test state.
	pulls []MockPullCall
	// templateDigests maps each template id to a fabricated stable content
	// address so the mock can stand in for a CAS-addressed holder; GetCapacity
	// surfaces it as the real engine does.
	templateDigests map[string]string
}

// MockPullCall is one recorded PullTemplate the mock engine received. TokenLen
// records the token length only; the token value is never stored.
type MockPullCall struct {
	TemplateID     string
	ManifestDigest string
	SourceURL      string
	TokenLen       int
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
		vsockDir = "/tmp/mitos-mock"
	}
	return fmt.Sprintf("%s/sandboxes/%s/vsock.sock", vsockDir, sandboxID)
}

func NewMockEngine() *MockEngine {
	return &MockEngine{
		templates:        make(map[string]*Template),
		sandboxes:        make(map[string]*Sandbox),
		ForkDelay:        800 * time.Microsecond,
		memoryTotalBytes: 16 * 1024 * 1024 * 1024,
	}
}

// SetMemoryTotal overrides the fixed node memory budget the mock's GetCapacity
// reports. Tests use it to shrink the budget and exercise capacity-exhaustion
// paths (the scheduler's ErrNoCapacity and the claim reconciler's bounded
// failure).
func (e *MockEngine) SetMemoryTotal(bytes int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.memoryTotalBytes = bytes
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
		SnapshotDir: fmt.Sprintf("/tmp/mitos-mock/templates/%s", id),
		CreatedAt:   time.Now(),
		Ready:       true,
	}
	// Report a deterministic content-address per template so the distribution
	// path has a digest to pull against (the real engine returns a true CAS
	// digest; the mock fabricates a stable one). 64 hex chars to match the
	// sha256 shape the registry and CRD status expect.
	if e.templateDigests == nil {
		e.templateDigests = make(map[string]string)
	}
	e.templateDigests[id] = mockTemplateDigest(id)
	return nil
}

// mockTemplateDigest fabricates a stable 64-char hex digest for a template id so
// the mock can stand in for a CAS-addressed holder in distribution tests.
func mockTemplateDigest(id string) string {
	sum := sha256.Sum256([]byte("mock-template:" + id))
	return hex.EncodeToString(sum[:])
}

// PullTemplate records the pull and registers the template as present, so a
// fake forkd node backed by the mock reports the template after a distribution
// pull. It records the source URL and digest (safe to log) and the token length
// only, never the token value. The mock does no real transfer.
func (e *MockEngine) PullTemplate(_ context.Context, templateID, manifestDigest, sourceURL, token string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pulls = append(e.pulls, MockPullCall{
		TemplateID:     templateID,
		ManifestDigest: manifestDigest,
		SourceURL:      sourceURL,
		TokenLen:       len(token),
	})
	e.templates[templateID] = &Template{
		ID:          templateID,
		SnapshotDir: fmt.Sprintf("/tmp/mitos-mock/templates/%s", templateID),
		CreatedAt:   time.Now(),
		Ready:       true,
	}
	// A pulled template carries the holder's digest, so the receiving node also
	// reports it as a content-addressed holder.
	if e.templateDigests == nil {
		e.templateDigests = make(map[string]string)
	}
	e.templateDigests[templateID] = manifestDigest
	return nil
}

// PullCalls returns the PullTemplate calls the mock received, in call order.
// Tests use it to assert the controller distributed by pull (source + digest)
// rather than issuing a second build.
func (e *MockEngine) PullCalls() []MockPullCall {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]MockPullCall, len(e.pulls))
	copy(out, e.pulls)
	return out
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
	memTotal := e.memoryTotalBytes
	digests := make(map[string]string, len(e.templateDigests))
	for id, d := range e.templateDigests {
		digests[id] = d
	}
	e.mu.RUnlock()

	report := metering.Aggregate(samples)

	// Per-template estimates: derive from live forks when present, else floor a
	// known template so envtest scheduling has a non-zero cold-start estimate
	// to bin-pack against even before any fork exists.
	estimates := templateEstimatesFromReport(report, nil)
	seen := make(map[string]struct{}, len(estimates))
	for _, est := range estimates {
		seen[est.TemplateID] = struct{}{}
	}
	for _, id := range templateIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		floor := templateEstimateFloor()
		floor.TemplateID = id
		// A representative cold shared set (256 MiB) so a cold placement has a
		// realistic non-zero marginal cost in scheduling tests.
		floor.SharedOnceBytes = 256 * 1024 * 1024
		estimates = append(estimates, floor)
	}

	return Capacity{
		ActiveSandboxes:   active,
		MaxSandboxes:      1000,
		MemoryTotal:       memTotal,
		MemoryUsed:        report.UsedCoWAware,
		MemoryShared:      report.SharedOnceTotal(),
		TemplateIDs:       templateIDs,
		TemplateDigests:   digests,
		TemplateEstimates: estimates,
		KVMAvailable:      false,
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
