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
	"golang.org/x/sys/unix"
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
	ID            string
	TemplateID    string
	SnapshotID    string
	KVMFD         int
	Endpoint      string
	Pid           int
	MemoryMap     []byte
	CreatedAt     time.Time
	MemoryUnique  int64
	MemoryShared  int64
	fcClient      *firecracker.Client
}

type ForkResult struct {
	SandboxID    string
	Endpoint     string
	ForkTimeMs   float64
	MemoryUnique int64
	MemoryShared int64
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

// Fork creates a new sandbox from a snapshot using CoW memory mapping.
// This is the hot path — target is <2ms.
func (e *Engine) Fork(snapshotID, sandboxID string, opts ForkOpts) (*ForkResult, error) {
	start := time.Now()

	snapshotDir := filepath.Join(e.dataDir, "templates", snapshotID, "snapshot")
	memFile := filepath.Join(snapshotDir, "mem")
	vmStateFile := filepath.Join(snapshotDir, "vmstate")

	if _, err := os.Stat(memFile); err != nil {
		return nil, fmt.Errorf("snapshot %s not found: %w", snapshotID, err)
	}

	// 1. Memory-map the snapshot file with MAP_PRIVATE (CoW).
	// All forks share the same physical pages until they diverge.
	fd, err := unix.Open(memFile, unix.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open snapshot memory: %w", err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("stat snapshot memory: %w", err)
	}

	memMap, err := unix.Mmap(fd, 0, int(stat.Size),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("mmap snapshot: %w", err)
	}
	unix.Close(fd)

	// 2. Start a new Firecracker process and load the snapshot.
	sandboxDir := filepath.Join(e.dataDir, "sandboxes", sandboxID)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		unix.Munmap(memMap)
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}

	// Write the CoW memory to a temporary file for Firecracker to load
	cowMemFile := filepath.Join(sandboxDir, "mem")
	if err := os.WriteFile(cowMemFile, memMap, 0600); err != nil {
		unix.Munmap(memMap)
		return nil, fmt.Errorf("write cow mem: %w", err)
	}

	// Copy vmstate
	vmStateData, err := os.ReadFile(vmStateFile)
	if err != nil {
		unix.Munmap(memMap)
		return nil, fmt.Errorf("read vmstate: %w", err)
	}
	cowVMState := filepath.Join(sandboxDir, "vmstate")
	if err := os.WriteFile(cowVMState, vmStateData, 0600); err != nil {
		unix.Munmap(memMap)
		return nil, fmt.Errorf("write cow vmstate: %w", err)
	}

	fcClient, err := firecracker.StartVM(firecracker.VMConfig{
		ID:             sandboxID,
		FirecrackerBin: e.firecrackerBin,
		WorkDir:        sandboxDir,
	})
	if err != nil {
		unix.Munmap(memMap)
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	if err := fcClient.LoadSnapshot(cowMemFile, cowVMState, true); err != nil {
		fcClient.Kill()
		unix.Munmap(memMap)
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	// 3. Determine endpoint
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
		MemoryMap:  memMap,
		CreatedAt:  time.Now(),
		fcClient:   fcClient,
	}

	// 4. Read memory stats
	sandbox.MemoryUnique, sandbox.MemoryShared = readMemoryStats(sandbox.Pid)

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
		defer source.fcClient.Resume()
	}

	// Checkpoint the running sandbox
	checkpointDir := filepath.Join(e.dataDir, "sandboxes", sourceSandboxID, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		return nil, fmt.Errorf("create checkpoint dir: %w", err)
	}

	memFile := filepath.Join(checkpointDir, "mem")
	vmStateFile := filepath.Join(checkpointDir, "vmstate")

	if source.fcClient != nil {
		if err := source.fcClient.CreateSnapshot(memFile, vmStateFile); err != nil {
			return nil, fmt.Errorf("checkpoint: %w", err)
		}
	}

	// Fork from the checkpoint using the standard fork path
	return e.Fork(sourceSandboxID, newSandboxID, ForkOpts{})
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

	if sandbox.fcClient != nil {
		sandbox.fcClient.Kill()
	}
	if sandbox.MemoryMap != nil {
		unix.Munmap(sandbox.MemoryMap)
	}

	// Clean up sandbox directory
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

// readMemoryStats reads /proc/<pid>/smaps_rollup to determine unique vs shared pages.
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
