package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paperclipinc/sandbox/internal/firecracker"
	"github.com/paperclipinc/sandbox/internal/vsock"
)

type Engine struct {
	mu             sync.RWMutex
	dataDir        string
	templateMgr    *firecracker.TemplateManager
	firecrackerBin string
	sandboxes      map[string]*Sandbox
	nextPort       int32
}

type Template struct {
	ID           string
	Image        string
	SnapshotDir  string
	MemFile      string
	VMStateFile  string
	CreatedAt    time.Time
	Ready        bool
	SnapshotSize int64
}

type Sandbox struct {
	ID           string
	TemplateID   string
	SnapshotID   string
	Endpoint     string
	Pid          int
	CreatedAt    time.Time
	MemoryUnique int64
	MemoryShared int64
	fcClient     *firecracker.Client
	agentClient  *vsock.Client
	VsockPath    string
}

type ForkResult struct {
	SandboxID    string
	Endpoint     string
	ForkTimeMs   float64
	MemoryUnique int64
	MemoryShared int64
	VsockPath    string
}

func NewEngine(dataDir, firecrackerBin, kernelPath string) (*Engine, error) {
	if err := validateKVM(); err != nil {
		return nil, fmt.Errorf("KVM not available: %w", err)
	}

	return &Engine{
		dataDir:        dataDir,
		firecrackerBin: firecrackerBin,
		templateMgr:    firecracker.NewTemplateManager(firecrackerBin, kernelPath, dataDir),
		sandboxes:      make(map[string]*Sandbox),
		nextPort:       10000,
	}, nil
}

func validateKVM() error {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return fmt.Errorf("/dev/kvm not found: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("/dev/kvm is not a character device")
	}
	return nil
}

// Fork creates a new sandbox from a snapshot.
//
// Firecracker loads the snapshot memory lazily via mmap; pages are faulted in
// on demand and shared (CoW) across all VMs restored from the same snapshot.
// This is the hot path; target is <10ms including FC process start.
func (e *Engine) Fork(snapshotID, sandboxID string, opts ForkOpts) (*ForkResult, error) {
	start := time.Now()

	snapshotDir := filepath.Join(e.dataDir, "templates", snapshotID, "snapshot")
	memFile := filepath.Join(snapshotDir, "mem")
	vmStateFile := filepath.Join(snapshotDir, "vmstate")

	if _, err := os.Stat(memFile); err != nil {
		return nil, fmt.Errorf("snapshot %s not found: %w", snapshotID, err)
	}

	// Create sandbox working directory
	sandboxDir := filepath.Join(e.dataDir, "sandboxes", sandboxID)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}

	// Firecracker loads the snapshot mem file via mmap(MAP_PRIVATE) internally.
	// We don't need to mmap it ourselves; just point Firecracker at the
	// original snapshot file. Each FC process gets its own CoW mapping.

	// Start a new Firecracker process
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")
	fcClient, err := firecracker.StartVM(firecracker.VMConfig{
		ID:             sandboxID,
		FirecrackerBin: e.firecrackerBin,
		WorkDir:        sandboxDir,
		SocketPath:     filepath.Join(sandboxDir, "firecracker.sock"),
	})
	if err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	// Load snapshot: Firecracker mmaps the mem file with MAP_PRIVATE
	if err := fcClient.LoadSnapshot(memFile, vmStateFile, true); err != nil {
		_ = fcClient.Kill()
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	elapsed := time.Since(start)

	// Determine endpoint
	e.mu.Lock()
	port := e.nextPort
	e.nextPort++
	e.mu.Unlock()
	endpoint := fmt.Sprintf("127.0.0.1:%d", port)

	sandbox := &Sandbox{
		ID:         sandboxID,
		SnapshotID: snapshotID,
		Endpoint:   endpoint,
		Pid:        fcClient.PID(),
		CreatedAt:  time.Now(),
		fcClient:   fcClient,
		VsockPath:  vsockPath,
	}

	sandbox.MemoryUnique, sandbox.MemoryShared = readMemoryStats(sandbox.Pid)

	e.mu.Lock()
	e.sandboxes[sandboxID] = sandbox
	e.mu.Unlock()

	return &ForkResult{
		SandboxID:    sandboxID,
		Endpoint:     endpoint,
		ForkTimeMs:   float64(elapsed.Microseconds()) / 1000.0,
		MemoryUnique: sandbox.MemoryUnique,
		MemoryShared: sandbox.MemoryShared,
		VsockPath:    vsockPath,
	}, nil
}

// ForkRunning checkpoints a running sandbox and creates a new fork from it.
func (e *Engine) ForkRunning(sourceSandboxID, newSandboxID string, pauseSource bool) (*ForkResult, error) {
	e.mu.RLock()
	source, ok := e.sandboxes[sourceSandboxID]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", sourceSandboxID)
	}

	if pauseSource && source.fcClient != nil {
		if err := source.fcClient.Pause(); err != nil {
			return nil, fmt.Errorf("pause source: %w", err)
		}
		defer func() {
			if err := source.fcClient.Resume(); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: resume source %s after checkpoint: %v\n", sourceSandboxID, err)
			}
		}()
	}

	// Checkpoint the running sandbox
	checkpointDir := filepath.Join(e.dataDir, "sandboxes", sourceSandboxID, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		return nil, fmt.Errorf("create checkpoint dir: %w", err)
	}

	if source.fcClient != nil {
		memFile := filepath.Join(checkpointDir, "mem")
		vmStateFile := filepath.Join(checkpointDir, "vmstate")
		if err := source.fcClient.CreateSnapshot(memFile, vmStateFile); err != nil {
			return nil, fmt.Errorf("checkpoint: %w", err)
		}
	}

	// Create a temporary "template" from the checkpoint so Fork can find it
	tmpTemplateDir := filepath.Join(e.dataDir, "templates", sourceSandboxID+"-live")
	if err := os.MkdirAll(filepath.Join(tmpTemplateDir, "snapshot"), 0755); err != nil {
		return nil, fmt.Errorf("create live-template dir: %w", err)
	}
	defer os.RemoveAll(tmpTemplateDir)

	// Symlink the checkpoint files as a snapshot
	if err := os.Symlink(
		filepath.Join(checkpointDir, "mem"),
		filepath.Join(tmpTemplateDir, "snapshot", "mem"),
	); err != nil {
		return nil, fmt.Errorf("symlink checkpoint mem: %w", err)
	}
	if err := os.Symlink(
		filepath.Join(checkpointDir, "vmstate"),
		filepath.Join(tmpTemplateDir, "snapshot", "vmstate"),
	); err != nil {
		return nil, fmt.Errorf("symlink checkpoint vmstate: %w", err)
	}

	return e.Fork(sourceSandboxID+"-live", newSandboxID, ForkOpts{})
}

// Terminate kills a sandbox and releases its resources.
func (e *Engine) Terminate(sandboxID string) error {
	e.mu.Lock()
	sandbox, ok := e.sandboxes[sandboxID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}
	delete(e.sandboxes, sandboxID)
	e.mu.Unlock()

	if sandbox.agentClient != nil {
		sandbox.agentClient.Close()
	}
	if sandbox.fcClient != nil {
		_ = sandbox.fcClient.Kill()
	}

	sandboxDir := filepath.Join(e.dataDir, "sandboxes", sandboxID)
	os.RemoveAll(sandboxDir)

	return nil
}

// GetCapacity returns the current node capacity.
func (e *Engine) GetCapacity() Capacity {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var totalUnique, totalShared int64
	for _, s := range e.sandboxes {
		totalUnique += s.MemoryUnique
		totalShared += s.MemoryShared
	}

	templateIDs, _ := e.templateMgr.ListTemplates()

	return Capacity{
		ActiveSandboxes: int32(len(e.sandboxes)),
		MemoryUsed:      totalUnique + totalShared,
		MemoryShared:    totalShared,
		TemplateIDs:     templateIDs,
		KVMAvailable:    true,
	}
}

// CreateTemplate boots a VM, runs init, snapshots it.
func (e *Engine) CreateTemplate(id string, rootfsPath string, initWaitSecs int) error {
	cfg := firecracker.DefaultVMConfig()
	cfg.RootfsPath = rootfsPath
	_, err := e.templateMgr.CreateTemplate(id, cfg, initWaitSecs)
	return err
}

type Capacity struct {
	ActiveSandboxes int32
	MaxSandboxes    int32
	MemoryTotal     int64
	MemoryUsed      int64
	MemoryShared    int64
	TemplateIDs     []string
	SnapshotIDs     []string
	KVMAvailable    bool
}

type ForkOpts struct {
	Env     map[string]string
	Secrets map[string]string
	Network *NetworkOpts
}

type NetworkOpts struct {
	EgressPolicy string
	AllowList    []string
}

func readMemoryStats(pid int) (unique, shared int64) {
	if pid <= 0 {
		return 0, 0
	}

	path := fmt.Sprintf("/proc/%d/smaps_rollup", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		bytes := kb * 1024

		switch fields[0] {
		case "Private_Clean:", "Private_Dirty:":
			unique += bytes
		case "Shared_Clean:", "Shared_Dirty:":
			shared += bytes
		}
	}
	return unique, shared
}
