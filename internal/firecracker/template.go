package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// VsockRelPath is the vsock uds_path configured before snapshot and thus
// baked into every template snapshot. It is deliberately RELATIVE so that
// each restored Firecracker process binds it against its own working
// directory (raw mode) or chroot root (jailer mode); see SetVsock and the
// CreateTemplate comment for the per-cwd isolation invariant.
const VsockRelPath = "vsock.sock"

// TemplateManager handles the lifecycle of snapshot templates.
// A template is: boot a VM → run init commands → pause → snapshot → kill.
// The snapshot is then used by the fork engine for CoW forking.
type TemplateManager struct {
	firecrackerBin string
	kernelPath     string
	dataDir        string
	jailer         JailerConfig
}

// NewTemplateManager builds a template manager. A zero jailer config
// keeps the direct-exec launch path.
func NewTemplateManager(firecrackerBin, kernelPath, dataDir string, jailer JailerConfig) *TemplateManager {
	return &TemplateManager{
		firecrackerBin: firecrackerBin,
		kernelPath:     kernelPath,
		dataDir:        dataDir,
		jailer:         jailer,
	}
}

type TemplateResult struct {
	ID             string
	SnapshotDir    string
	MemFile        string
	VMStateFile    string
	CreationTimeMs float64
	SnapshotSize   int64
}

// CreateTemplate boots a VM, waits for it to be ready, optionally runs init
// commands, then snapshots it. The VM is killed after snapshot.
func (tm *TemplateManager) CreateTemplate(id string, cfg VMConfig, initWaitSeconds int) (*TemplateResult, error) {
	start := time.Now()

	snapshotDir := filepath.Join(tm.dataDir, "templates", id, "snapshot")
	workDir := filepath.Join(tm.dataDir, "templates", id)

	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	// Copy rootfs so the template has its own writable copy
	templateRootfs := filepath.Join(workDir, "rootfs.ext4")
	if cfg.RootfsPath != "" {
		if err := copyFile(cfg.RootfsPath, templateRootfs); err != nil {
			return nil, fmt.Errorf("copy rootfs: %w", err)
		}
	}

	cfg.FirecrackerBin = tm.firecrackerBin
	cfg.WorkDir = workDir
	cfg.ID = id
	cfg.Jailer = tm.jailer
	// Kernel and rootfs are hard-linked into the chroot in jailer mode so
	// the API paths below resolve inside it.
	cfg.ChrootFiles = []string{tm.kernelPath}
	if cfg.RootfsPath != "" {
		cfg.ChrootFiles = append(cfg.ChrootFiles, templateRootfs)
	}

	// Start the VM
	client, err := StartVM(cfg)
	if err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
	}
	defer func() { _ = client.Kill() }()

	// Configure the VM
	if err := client.SetBootSource(tm.kernelPath, cfg.BootArgs); err != nil {
		return nil, fmt.Errorf("set boot source: %w", err)
	}

	if err := client.SetMachineConfig(cfg.VcpuCount, cfg.MemSizeMib); err != nil {
		return nil, fmt.Errorf("set machine config: %w", err)
	}

	if err := client.AddDrive("rootfs", templateRootfs, false, true); err != nil {
		return nil, fmt.Errorf("add rootfs drive: %w", err)
	}

	// Set up vsock for guest communication.
	//
	// The uds_path MUST be relative ("vsock.sock"): Firecracker bakes the
	// exact uds_path string into the snapshot and rebinds it verbatim on
	// every restore. A relative path is resolved against each restored
	// Firecracker process's working directory, so identical baked path +
	// distinct per-VM cwd = distinct host socket, and forks never collide
	// on one UDS. Under the jailer the chroot already isolates each VM;
	// in raw direct-exec mode (which we keep) this relative path plus the
	// per-VM WorkDir set as cmd.Dir in StartVM is what keeps forks apart.
	if err := client.SetVsock(3, VsockRelPath); err != nil {
		return nil, fmt.Errorf("set vsock: %w", err)
	}

	// Boot
	if err := client.Start(); err != nil {
		return nil, fmt.Errorf("start instance: %w", err)
	}

	// Wait for init to complete.
	// In production, this would be replaced by guest agent signaling readiness.
	if initWaitSeconds > 0 {
		time.Sleep(time.Duration(initWaitSeconds) * time.Second)
	}

	// Pause the VM before snapshot
	if err := client.Pause(); err != nil {
		return nil, fmt.Errorf("pause VM: %w", err)
	}

	// Create snapshot
	memFile := filepath.Join(snapshotDir, "mem")
	vmStateFile := filepath.Join(snapshotDir, "vmstate")

	if err := client.CreateSnapshot(memFile, vmStateFile); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	// Get snapshot size
	memInfo, err := os.Stat(memFile)
	if err != nil {
		return nil, fmt.Errorf("stat mem file: %w", err)
	}

	elapsed := time.Since(start)

	return &TemplateResult{
		ID:             id,
		SnapshotDir:    snapshotDir,
		MemFile:        memFile,
		VMStateFile:    vmStateFile,
		CreationTimeMs: float64(elapsed.Milliseconds()),
		SnapshotSize:   memInfo.Size(),
	}, nil
}

// DeleteTemplate removes a template and its snapshot files.
func (tm *TemplateManager) DeleteTemplate(id string) error {
	dir := filepath.Join(tm.dataDir, "templates", id)
	return os.RemoveAll(dir)
}

// ListTemplates returns all template IDs on this node.
func (tm *TemplateManager) ListTemplates() ([]string, error) {
	templatesDir := filepath.Join(tm.dataDir, "templates")
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			snapshotDir := filepath.Join(templatesDir, e.Name(), "snapshot", "mem")
			if _, err := os.Stat(snapshotDir); err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	return ids, nil
}

// HasTemplate checks if a template snapshot exists.
func (tm *TemplateManager) HasTemplate(id string) bool {
	memFile := filepath.Join(tm.dataDir, "templates", id, "snapshot", "mem")
	_, err := os.Stat(memFile)
	return err == nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	return out.Sync()
}
