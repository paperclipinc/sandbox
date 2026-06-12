package firecracker

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/paperclipinc/sandbox/internal/vsock"
)

// execFunc runs a single shell command in the booted template VM and returns
// its result. It is the seam between the template build and the guest-agent
// vsock transport: the production path connects over vsock, while tests inject
// a fake so the init-command safety logic can be exercised without Firecracker.
type execFunc func(command string) (*vsock.ExecResponse, error)

// initExecTimeoutSecs bounds each init command run in the template VM. Image
// init steps (pip install, apt-get) can be slow, so this is generous.
const initExecTimeoutSecs = 600

// initConnectAttempts and initConnectDelay bound the wait for the guest agent
// to come up inside the freshly booted template VM before init runs.
const (
	initConnectAttempts = 30
	initConnectDelay    = 1 * time.Second
)

// noAgentFallbackWait is the fixed sleep used when the guest agent never
// answers (a rootfs without the agent, an edge back-compat case). Boot
// readiness cannot be confirmed in that case, so we wait a short fixed time
// and log a warning before snapshotting rather than failing outright.
const noAgentFallbackWait = 5 * time.Second

// runInitCommands runs each init command in order through exec, failing the
// build on the first command that exits nonzero or errors. A failed init must
// NOT be snapshotted (a template whose `pip install` failed must never be
// served), so the returned error names the offending command and its stderr
// and execution stops immediately. An empty list is a no-op.
func runInitCommands(exec execFunc, commands []string) error {
	for _, cmd := range commands {
		res, err := exec(cmd)
		if err != nil {
			return fmt.Errorf("init command %q: %w", cmd, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("init command %q failed with exit code %d: %s", cmd, res.ExitCode, res.Stderr)
		}
	}
	return nil
}

// connectInitExec connects to the guest agent over the template VM's vsock UDS,
// confirms the guest has finished booting by Pinging the agent, and returns an
// execFunc that runs commands in /workspace. The connect+ping is retried while
// the agent finishes starting (mirrors cmd/test-agent's bounded retry). A
// successful ping is the boot-readiness signal: the agent only answers once it
// is up as PID 1, so the caller knows the VM is booted and is safe to snapshot.
// This runs regardless of whether there are init commands, so a half-booted VM
// is never snapshotted.
func connectInitExec(vsockPath string) (execFunc, func(), error) {
	var client *vsock.Client
	var err error
	for attempt := 0; attempt < initConnectAttempts; attempt++ {
		client, err = vsock.Connect(vsockPath, vsock.AgentPort)
		if err == nil {
			// Connected: confirm the agent actually answers before treating the
			// guest as booted. A bare connect can race the agent's listener
			// coming up; a successful Ping is the real readiness signal.
			if _, perr := client.Ping(); perr == nil {
				break
			} else {
				_ = client.Close()
				err = perr
			}
		}
		time.Sleep(initConnectDelay)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("connect to guest agent for init (%s): %w", vsockPath, err)
	}
	exec := func(command string) (*vsock.ExecResponse, error) {
		return client.Exec(command, "/workspace", nil, initExecTimeoutSecs)
	}
	return exec, func() { _ = client.Close() }, nil
}

// VsockRelPath is the vsock uds_path configured before snapshot and thus
// baked into every template snapshot. It is deliberately RELATIVE so that
// each restored Firecracker process binds it against its own working
// directory (raw mode) or chroot root (jailer mode); see SetVsock and the
// CreateTemplate comment for the per-cwd isolation invariant.
const VsockRelPath = "vsock.sock"

// NetIfaceID is the single guest NIC iface_id baked into every template
// snapshot. Forks remap this exact id to their own tap via the snapshot/load
// network_overrides (LoadSnapshotWithOverrides), so the value must be stable.
const NetIfaceID = "eth0"

// PlaceholderTapName is the host tap a template's placeholder NIC is bound to
// at snapshot time. It never carries live traffic: the template VM is paused
// and snapshotted, and every fork remaps NetIfaceID to its OWN tap at load.
// The template build creates this tap (host-side) before booting.
const PlaceholderTapName = "sbtap-template"

// TemplateManager handles the lifecycle of snapshot templates.
// A template is: boot a VM → run init commands → pause → snapshot → kill.
// The snapshot is then used by the fork engine for CoW forking.
type TemplateManager struct {
	firecrackerBin string
	kernelPath     string
	dataDir        string
	jailer         JailerConfig

	// connectInit resolves the booted template VM's vsock path to an execFunc
	// for running init commands. It is a seam: the default connects over vsock
	// to the guest agent; tests override it. It returns the exec, a cleanup to
	// close the connection, and an error.
	connectInit func(vsockPath string) (execFunc, func(), error)

	// fallbackWait is the fixed wait used when the guest agent never answers and
	// there are no init commands (boot readiness could not be confirmed). sleep
	// performs the wait. Both are fields so tests do not actually sleep.
	fallbackWait time.Duration
	sleep        func(time.Duration)
}

// NewTemplateManager builds a template manager. A zero jailer config
// keeps the direct-exec launch path.
func NewTemplateManager(firecrackerBin, kernelPath, dataDir string, jailer JailerConfig) *TemplateManager {
	return &TemplateManager{
		firecrackerBin: firecrackerBin,
		kernelPath:     kernelPath,
		dataDir:        dataDir,
		jailer:         jailer,
		connectInit:    connectInitExec,
		fallbackWait:   noAgentFallbackWait,
		sleep:          time.Sleep,
	}
}

// awaitReadyAndRunInit confirms the booted template VM is ready and runs its
// init commands before the caller snapshots. It ALWAYS connects to the guest
// agent first (via connectInit, which Pings as its readiness signal), even when
// there are no init commands, so a half-booted VM is never snapshotted. On a
// successful connect it runs each init command, failing the build on the first
// nonzero exit. If the agent never answers (a rootfs without the agent, an edge
// back-compat case) readiness cannot be confirmed: with init commands this is a
// hard error (there is no way to run them); with none it logs a warning and
// falls back to a short fixed wait. The fallback sleep is a field so tests do
// not actually sleep.
func (tm *TemplateManager) awaitReadyAndRunInit(id, vsockPath string, initCommands []string) error {
	exec, closeExec, connErr := tm.connectInit(vsockPath)
	if connErr != nil {
		if len(initCommands) > 0 {
			return fmt.Errorf("template %s: guest agent never answered, cannot run init commands: %w", id, connErr)
		}
		log.Printf("warning: template %s: guest agent did not respond (%v); boot readiness could not be confirmed, falling back to a %s fixed wait before snapshot", id, connErr, tm.fallbackWait)
		tm.sleep(tm.fallbackWait)
		return nil
	}
	defer closeExec()
	if err := runInitCommands(exec, initCommands); err != nil {
		return fmt.Errorf("template %s init failed: %w", id, err)
	}
	return nil
}

type TemplateResult struct {
	ID             string
	SnapshotDir    string
	MemFile        string
	VMStateFile    string
	CreationTimeMs float64
	SnapshotSize   int64
}

// CreateTemplate boots a VM, runs each init command IN the VM over the guest
// agent vsock, then snapshots it. If any init command exits nonzero the build
// fails and nothing is snapshotted, so a broken template (e.g. a failed
// `pip install`) is never served. With no init commands the VM is snapshotted
// as soon as it has booted. The VM is killed after snapshot.
func (tm *TemplateManager) CreateTemplate(id string, cfg VMConfig, initCommands []string) (*TemplateResult, error) {
	// Re-assert the allowlist barrier locally: the id is validated at the forkd
	// gRPC boundary (validateSandboxID), but a defense-in-depth check here keeps
	// every path that joins the id provably free of separators and traversal,
	// and is the CodeQL-recognized barrier for the go/path-injection flows below.
	if err := validateVMID(id); err != nil {
		return nil, err
	}
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
	// Placeholder volume backings are baked into the snapshot, so they must be
	// linked into the chroot in jailer mode too (same as the rootfs) for the
	// AddDrive path_on_host below to resolve inside it.
	for _, vd := range cfg.VolumeDrives {
		cfg.ChrootFiles = append(cfg.ChrootFiles, vd.PathOnHost)
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

	// Attach one placeholder volume drive per template volume BEFORE
	// InstanceStart so the snapshot bakes the block devices. Firecracker cannot
	// add a drive on restore, so each fork later rebinds these baked drives
	// (by drive id, which is the volume name) to its OWN backing via PatchDrive.
	// The guest does NOT mount them at build time. They are attached in slice
	// order, so the guest device order is deterministic: rootfs is vda and these
	// follow as vdb, vdc, ... in order.
	for _, vd := range cfg.VolumeDrives {
		if err := client.AddDrive(vd.DriveID, vd.PathOnHost, vd.ReadOnly, false); err != nil {
			return nil, fmt.Errorf("add placeholder volume drive %s: %w", vd.DriveID, err)
		}
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

	// Attach a placeholder NIC when networking is enabled so the snapshot
	// bakes a net device. Firecracker does NOT support hot-plugging a NIC
	// after boot or adding one on restore, so the device must exist at
	// snapshot time; each fork then remaps this iface_id to its own tap via
	// network_overrides on snapshot/load (LoadSnapshotWithOverrides). The
	// placeholder tap is host-created by the template build and carries no
	// live traffic (the VM is paused immediately and snapshotted).
	if cfg.Network != nil {
		if err := client.SetNetwork(cfg.Network.IfaceID, cfg.Network.GuestMAC, cfg.Network.HostDevName); err != nil {
			return nil, fmt.Errorf("attach placeholder NIC: %w", err)
		}
	}

	// Boot
	if err := client.Start(); err != nil {
		return nil, fmt.Errorf("start instance: %w", err)
	}

	// Wait for the guest to finish booting before snapshotting, ALWAYS,
	// regardless of whether there are init commands. We connect to the guest
	// agent over vsock and Ping (a bounded startup retry); a successful ping is
	// the boot-readiness signal, so a half-booted VM is never snapshotted. Then
	// we run each init command (if any), FAILING the build if any command exits
	// nonzero so a broken template (e.g. a failed `pip install`) is never
	// served. The vsock UDS is the baked relative path resolved against this
	// template VM's working directory (see SetVsock).
	//
	// If the agent never answers (a rootfs without the agent, an edge
	// back-compat case) we cannot confirm readiness, so we fall back to a short
	// fixed sleep and log a warning rather than aborting. Init commands cannot
	// run without an agent, so a fallback path with init commands is a hard
	// error: there is no way to execute them.
	vsockPath := client.VsockHostPath(VsockRelPath)
	if err := tm.awaitReadyAndRunInit(id, vsockPath, initCommands); err != nil {
		return nil, err
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
	// Defense-in-depth allowlist barrier before joining the id into a path that
	// is then recursively removed (see CreateTemplate).
	if err := validateVMID(id); err != nil {
		return err
	}
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
	// An id that fails the allowlist names no valid template; refuse before
	// joining it into a path (defense-in-depth, see CreateTemplate).
	if err := validateVMID(id); err != nil {
		return false
	}
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
