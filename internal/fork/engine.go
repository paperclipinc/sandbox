package fork

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/cas"
	"github.com/paperclipinc/sandbox/internal/firecracker"
	"github.com/paperclipinc/sandbox/internal/netconf"
	"github.com/paperclipinc/sandbox/internal/network"
	"github.com/paperclipinc/sandbox/internal/vsock"
)

type Engine struct {
	mu             sync.RWMutex
	dataDir        string
	templateMgr    *firecracker.TemplateManager
	firecrackerBin string
	jailer         firecracker.JailerConfig
	sandboxes      map[string]*Sandbox
	nextPort       int32

	// casStore content-addresses every template snapshot for integrity
	// verification (issue #9) and incremental transfer (Task 4). Rooted at
	// <casDir>, defaulting to <dataDir>/cas.
	casStore *cas.Store
	// allowUnverified, when true, lets Fork proceed on a snapshot that failed
	// or skipped verification, after a loud one-time warning. Development
	// escape hatch only; default false refuses unverified forks.
	allowUnverified bool
	// vmmVersion is threaded into every snapshot manifest. Empty for now; the
	// version-compat contract is tracked separately (#32).
	vmmVersion string
	// unverifiedWarned records snapshot IDs that already emitted the loud
	// allow-unverified warning, so it fires once per template not per fork.
	unverifiedWarned map[string]struct{}
	// templateDigests caches the recorded manifest digest per template id so
	// GetCapacity can report it to the controller without re-reading disk.
	templateDigests map[string]cas.Digest

	// Networking is opt-in. When both netMgr and netAlloc are set, a fork
	// that carries NetworkOpts gets a distinct per-fork network identity:
	// the allocator hands out a unique tap/MAC/IP, the manager creates the
	// host tap and egress ruleset, the snapshot's baked NIC is remapped to
	// the new tap via network_overrides, and the guest is re-addressed via
	// NotifyForked. Both nil (the default) means networking is DISABLED and
	// the engine behaves exactly as before. resolverIP is the optional DNS
	// resolver guests may reach (nil omits the DNS allow rule).
	netMgr     network.Manager
	netAlloc   *netconf.Allocator
	resolverIP net.IP

	// agentBinPath and busyboxPath are injected into a rootfs built from an
	// OCI image (see EngineOpts). Unused for file-path rootfs templates.
	agentBinPath string
	busyboxPath  string

	// buildRootfsFromImage turns an OCI image ref into a bootable rootfs.ext4
	// at outPath. It is a seam so CreateTemplate can be tested without a
	// registry; the default pulls, extracts, injects the agent, and builds the
	// ext4 image.
	buildRootfsFromImage func(ctx context.Context, ref, outPath, agentBin, busyboxBin string) error

	// runTemplateBuild boots the VM, runs init in it, and snapshots it. It is a
	// seam so the init-failure safety property can be tested WITHOUT launching
	// Firecracker. The default delegates to the firecracker TemplateManager.
	runTemplateBuild func(id string, cfg firecracker.VMConfig, initCommands []string) error
}

// Placeholder network identity used only while building a template snapshot.
// The template VM is paused and snapshotted immediately, so the placeholder
// tap never carries live traffic; every fork remaps NetIfaceID to its OWN tap
// at load. The IPs are link-local-style addresses inside the default sandbox
// subnet and the MAC is locally administered unicast.
var (
	placeholderMAC     = "02:00:00:00:00:01"
	placeholderHostIP  = net.IPv4(10, 200, 255, 253).To4()
	placeholderGuestIP = net.IPv4(10, 200, 255, 254).To4()
)

// networkEnabled reports whether per-fork networking is wired. Both the
// manager and allocator must be present.
func (e *Engine) networkEnabled() bool {
	return e.netMgr != nil && e.netAlloc != nil
}

// forkNetwork is the result of preparing one fork's networking: the acquired
// identity, the snapshot/load NIC overrides that remap the baked placeholder
// NIC to this fork's tap, and the per-fork guest config delivered over vsock.
type forkNetwork struct {
	identity  netconf.Identity
	overrides []firecracker.NetworkOverride
	guestNet  *vsock.NotifyForkedNetwork
}

// prepareForkNetwork acquires a distinct network identity for the sandbox,
// applies the host-side network (tap + egress ruleset) via the manager, and
// returns the NIC overrides and guest config the fork needs. It returns
// (nil, nil) when networking is disabled or the request carries no NetworkOpts,
// so the non-network path is untouched. On any failure it releases the
// just-acquired identity so a partial setup does not leak the allocation.
func (e *Engine) prepareForkNetwork(sandboxID string, opts ForkOpts) (*forkNetwork, error) {
	if !e.networkEnabled() || opts.Network == nil {
		return nil, nil
	}

	allow, skipped, err := netconf.SplitAllowList(opts.Network.AllowList)
	if err != nil {
		return nil, fmt.Errorf("parse egress allowlist for %s: %w", sandboxID, err)
	}
	// Name-based allow entries cannot be enforced without a controlled DNS
	// resolver (PR2); they are parsed but dropped from the ruleset. Warn loudly
	// so an operator is not misled into thinking a name rule is in effect. The
	// entries are config (host:port), safe to log.
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "forkd: WARNING egress allowlist for %s has %d name-based entries that are NOT enforced (no controlled resolver yet): %v\n", sandboxID, len(skipped), skipped)
	}
	policy := v1alpha1.EgressPolicy(opts.Network.EgressPolicy)

	id, err := e.netAlloc.Acquire(sandboxID)
	if err != nil {
		return nil, fmt.Errorf("acquire network identity for %s: %w", sandboxID, err)
	}

	if err := e.netMgr.Setup(context.Background(), id, policy, allow, e.resolverIP); err != nil {
		e.netAlloc.Release(sandboxID)
		return nil, fmt.Errorf("set up network for %s (tap %s): %w", sandboxID, id.TapName, err)
	}

	return &forkNetwork{
		identity: id,
		overrides: []firecracker.NetworkOverride{{
			IfaceID:     firecracker.NetIfaceID,
			HostDevName: id.TapName,
		}},
		guestNet: &vsock.NotifyForkedNetwork{
			GuestIP:   id.GuestIP.String(),
			GatewayIP: id.HostIP.String(),
			PrefixLen: 30,
		},
	}, nil
}

// teardownForkNetwork removes the host network for a sandbox and releases its
// identity. Best effort: a teardown error is logged (tap name only, no
// secrets) but the identity is always released so the slot is reusable.
func (e *Engine) teardownForkNetwork(sandboxID string, id netconf.Identity) {
	if !e.networkEnabled() {
		return
	}
	if err := e.netMgr.Teardown(context.Background(), id); err != nil {
		fmt.Fprintf(os.Stderr, "forkd: teardown network for %s (tap %s): %v\n", sandboxID, id.TapName, err)
	}
	e.netAlloc.Release(sandboxID)
}

// EngineOpts carries the optional, security-relevant engine configuration so
// the positional constructor stays short. The zero value is the safe default:
// CAS under <dataDir>/cas and unverified forks refused.
type EngineOpts struct {
	// CASDir roots the content-addressed store. Empty means <dataDir>/cas.
	CASDir string
	// AllowUnverified disables the verify-on-load refusal (development only).
	AllowUnverified bool
	// VMMVersion is recorded in every snapshot manifest. May be empty.
	VMMVersion string
	// NetManager and NetAllocator enable per-fork networking. Both must be
	// non-nil to enable it; either nil leaves networking DISABLED (default).
	NetManager   network.Manager
	NetAllocator *netconf.Allocator
	// ResolverIP is the optional DNS resolver guests may reach. Nil omits the
	// DNS allow rule from each fork's egress ruleset.
	ResolverIP net.IP
	// AgentBinPath is the host path of the guest agent binary injected as
	// /init when CreateTemplate builds a rootfs from an OCI image. It is
	// REQUIRED for image builds and unused for file-path rootfs templates.
	// For now forkd must be shipped/mounted with this binary present; a
	// follow-up will go:embed the agent into forkd so the flag is optional.
	AgentBinPath string
	// BusyboxPath is an optional static /bin/sh source injected when an image
	// lacks a shell. Empty means images without a shell cannot run init.
	BusyboxPath string
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
	// rootfsPath is the host path of the drive backing file embedded in
	// the snapshot this sandbox was restored from; live-forks inherit it
	// so the jailer chroot of every descendant can link it in.
	rootfsPath string
	// netID is this sandbox's per-fork network identity when networking is
	// enabled and the fork requested it; the zero value (empty TapName) means
	// no host network was set up and Terminate skips teardown.
	netID netconf.Identity
}

type ForkResult struct {
	SandboxID    string
	Endpoint     string
	ForkTimeMs   float64
	MemoryUnique int64
	MemoryShared int64
	VsockPath    string
	// GuestNetwork, when set, is the per-fork eth0 config the daemon delivers
	// to the guest in the NotifyForked message so the fork is re-addressed to
	// its distinct guest IP + gateway. Nil when networking is disabled or the
	// fork carried no NetworkOpts. Addresses are safe to log.
	GuestNetwork *vsock.NotifyForkedNetwork
}

// SandboxRecord is the minimal view of a live sandbox an engine reports
// through ListSandboxes. Last-activity is tracked by the SandboxAPI, not the
// engine (the engine never sees exec or file traffic), so it is absent here.
type SandboxRecord struct {
	ID        string
	CreatedAt time.Time
}

// NewEngine builds the real KVM-backed engine. A zero jailer config
// launches Firecracker directly (development only; flagged in the threat
// model); with JailerBin set every VM runs through the jailer with a
// dedicated uid/gid from the configured range and a per-VM chroot.
func NewEngine(dataDir, firecrackerBin, kernelPath string, jailer firecracker.JailerConfig, opts EngineOpts) (*Engine, error) {
	if err := validateKVM(); err != nil {
		return nil, fmt.Errorf("KVM not available: %w", err)
	}

	if jailer.Enabled() {
		jailer.DataDir = dataDir
		if jailer.UIDRange[0] == 0 || jailer.UIDRange[0] > jailer.UIDRange[1] {
			return nil, fmt.Errorf("jailer uid range %d-%d invalid: low must be nonzero and not above high", jailer.UIDRange[0], jailer.UIDRange[1])
		}
		if jailer.Allocator == nil {
			jailer.Allocator = firecracker.NewUIDAllocator(jailer.UIDRange[0], jailer.UIDRange[1])
		}
	}

	casDir := opts.CASDir
	if casDir == "" {
		casDir = filepath.Join(dataDir, "cas")
	}
	store, err := cas.New(casDir)
	if err != nil {
		return nil, fmt.Errorf("init CAS store: %w", err)
	}

	tmplMgr := firecracker.NewTemplateManager(firecrackerBin, kernelPath, dataDir, jailer)
	e := &Engine{
		dataDir:              dataDir,
		firecrackerBin:       firecrackerBin,
		jailer:               jailer,
		templateMgr:          tmplMgr,
		sandboxes:            make(map[string]*Sandbox),
		nextPort:             10000,
		casStore:             store,
		allowUnverified:      opts.AllowUnverified,
		vmmVersion:           opts.VMMVersion,
		unverifiedWarned:     make(map[string]struct{}),
		templateDigests:      make(map[string]cas.Digest),
		netMgr:               opts.NetManager,
		netAlloc:             opts.NetAllocator,
		resolverIP:           opts.ResolverIP,
		agentBinPath:         opts.AgentBinPath,
		busyboxPath:          opts.BusyboxPath,
		buildRootfsFromImage: buildRootfsFromImage,
	}
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		_, err := tmplMgr.CreateTemplate(id, cfg, initCommands)
		return err
	}
	return e, nil
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
	// Verify-on-load gate (issue #9): cheap marker check, with a one-time
	// lazy verify for templates this process did not build. Refuses on
	// mismatch unless the development escape hatch is set.
	if err := e.ensureVerified(snapshotID); err != nil {
		return nil, err
	}
	// The drive path embedded in the snapshot points at the template's
	// rootfs; in jailer mode that file must be linked into the new VM's
	// chroot at the same path for the restore to resolve it.
	rootfsPath := filepath.Join(e.dataDir, "templates", snapshotID, "rootfs.ext4")
	if _, err := os.Stat(rootfsPath); err != nil {
		rootfsPath = ""
	}
	return e.fork(snapshotID, sandboxID, rootfsPath, opts)
}

// fork is Fork with the backing rootfs path made explicit so ForkRunning
// can thread the original template rootfs through live-fork checkpoints.
func (e *Engine) fork(snapshotID, sandboxID, rootfsPath string, opts ForkOpts) (*ForkResult, error) {
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

	// In jailer mode the snapshot files and the backing rootfs are
	// hard-linked into the per-VM chroot before launch; the API paths
	// below then resolve inside it (mirror layout, see chrootPath).
	chrootFiles := []string{memFile, vmStateFile}
	if rootfsPath != "" {
		chrootFiles = append(chrootFiles, rootfsPath)
	}

	// Start a new Firecracker process
	fcClient, err := firecracker.StartVM(firecracker.VMConfig{
		ID:             sandboxID,
		FirecrackerBin: e.firecrackerBin,
		WorkDir:        sandboxDir,
		SocketPath:     filepath.Join(sandboxDir, "firecracker.sock"),
		Jailer:         e.jailer,
		ChrootFiles:    chrootFiles,
	})
	if err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	// The template snapshot bakes a RELATIVE vsock uds_path
	// (firecracker.VsockRelPath); every restored Firecracker process
	// rebinds that exact string against its own working directory, so two
	// forks of one snapshot never collide on a single host socket. In raw
	// direct-exec mode the working directory is this sandbox's WorkDir
	// (sandboxDir, set as cmd.Dir in StartVM); under the jailer it is the
	// per-VM chroot root. VsockHostPath resolves the baked relative path
	// to the absolute host location for whichever mode is active.
	vsockPath := fcClient.VsockHostPath(firecracker.VsockRelPath)

	// Per-fork networking (opt-in). Acquire a distinct identity, create the
	// host tap + egress ruleset, and build the snapshot/load overrides that
	// remap the snapshot's baked placeholder NIC to THIS fork's tap. Returns
	// nil when networking is disabled or the fork carries no NetworkOpts.
	fnet, err := e.prepareForkNetwork(sandboxID, opts)
	if err != nil {
		_ = fcClient.Kill()
		return nil, err
	}
	var overrides []firecracker.NetworkOverride
	if fnet != nil {
		overrides = fnet.overrides
	}

	// Load snapshot: Firecracker mmaps the mem file with MAP_PRIVATE. When
	// networking is on, network_overrides rebinds the baked NIC to the fork's
	// own tap (v1.15 supports this; it is the network analog of the relative
	// vsock uds_path). nil overrides restores exactly as before.
	if err := fcClient.LoadSnapshotWithOverrides(memFile, vmStateFile, true, overrides); err != nil {
		_ = fcClient.Kill()
		if fnet != nil {
			e.teardownForkNetwork(sandboxID, fnet.identity)
		}
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
		rootfsPath: rootfsPath,
	}
	var guestNet *vsock.NotifyForkedNetwork
	if fnet != nil {
		sandbox.netID = fnet.identity
		guestNet = fnet.guestNet
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
		GuestNetwork: guestNet,
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

	// Fail closed: a live fork restores the source's baked NIC, which would
	// collide on tap/MAC/IP with the source's live network. Until per-VM netns
	// (husk pods #18) isolates each fork's interface, live-forking a networked
	// sandbox is unsupported.
	if e.networkEnabled() {
		return nil, fmt.Errorf("live fork (ForkRunning) of a networked sandbox is not supported yet; tracked in #18")
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

	// Thread the original template rootfs through: the checkpoint's
	// embedded drive path still points at it, and the new VM's chroot
	// needs it linked in.
	return e.fork(sourceSandboxID+"-live", newSandboxID, source.rootfsPath, ForkOpts{})
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

	// Release the per-fork host network (tap + egress ruleset) and the
	// identity allocation. Skipped when this sandbox had no network set up
	// (empty TapName) or networking is disabled.
	if sandbox.netID.TapName != "" {
		e.teardownForkNetwork(sandboxID, sandbox.netID)
	}

	sandboxDir := filepath.Join(e.dataDir, "sandboxes", sandboxID)
	os.RemoveAll(sandboxDir)

	return nil
}

// ListSandboxes returns a record for every sandbox this engine currently
// holds. Order is unspecified.
func (e *Engine) ListSandboxes() []SandboxRecord {
	e.mu.RLock()
	defer e.mu.RUnlock()

	records := make([]SandboxRecord, 0, len(e.sandboxes))
	for _, s := range e.sandboxes {
		records = append(records, SandboxRecord{ID: s.ID, CreatedAt: s.CreatedAt})
	}
	return records
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

	digests := make(map[string]string, len(templateIDs))
	for _, id := range templateIDs {
		if d, ok := e.templateDigests[id]; ok {
			digests[id] = string(d)
			continue
		}
		// Template present on disk but not yet recorded by this process
		// (e.g. discovered after a restart). Surface the persisted digest so
		// the controller can still record it; verification happens lazily at
		// first fork.
		if d, err := readDigestFile(e.dataDir, id); err == nil {
			digests[id] = string(d)
		}
	}

	return Capacity{
		ActiveSandboxes: int32(len(e.sandboxes)),
		MemoryUsed:      totalUnique + totalShared,
		MemoryShared:    totalShared,
		TemplateIDs:     templateIDs,
		TemplateDigests: digests,
		KVMAvailable:    true,
	}
}

// CreateTemplate builds a template from an image ref or an existing rootfs file
// path, boots it, runs each init command IN the VM (failing the build if any
// command exits nonzero), snapshots it, then content-addresses the resulting
// snapshot into the CAS store, pins its manifest, records the digest, and
// writes the verified marker. The template is trusted at creation (this process
// just built it), so no re-hash gate is applied here.
//
// image is resolved by isImageRef: an existing file path takes the legacy copy
// path unchanged; an OCI reference is pulled, the agent is injected, and a
// rootfs.ext4 is built from it before boot. A nonzero init command aborts the
// build so a broken template is never snapshotted or served.
func (e *Engine) CreateTemplate(id string, image string, initCommands []string) error {
	cfg := firecracker.DefaultVMConfig()

	// The template rootfs runs the guest agent as PID 1 at /init: ociroot
	// injects the agent there for image builds, and the hand-built file-path
	// rootfs places it there too. A normal (non-initramfs) root filesystem does
	// NOT have /init in the kernel's default init search path, so the agent only
	// becomes PID 1 if we say so explicitly. Append init=/init unless the caller
	// already pinned an init=.
	if !strings.Contains(cfg.BootArgs, "init=") {
		cfg.BootArgs += " init=/init"
	}

	if isImageRef(image) {
		// Build the rootfs from the image into the template's own rootfs path.
		// The template manager copies cfg.RootfsPath into the template workdir,
		// so build into a temp file and remove it after the build.
		builtRootfs, err := os.CreateTemp("", "tmpl-rootfs-*.ext4")
		if err != nil {
			return fmt.Errorf("create temp rootfs for template %s: %w", id, err)
		}
		builtPath := builtRootfs.Name()
		_ = builtRootfs.Close()
		defer func() { _ = os.Remove(builtPath) }()

		if err := e.buildRootfsFromImage(context.Background(), image, builtPath, e.agentBinPath, e.busyboxPath); err != nil {
			return fmt.Errorf("build template %s from image %q: %w", id, image, err)
		}
		cfg.RootfsPath = builtPath
	} else {
		cfg.RootfsPath = image
	}

	// When networking is enabled, bake a placeholder NIC into the snapshot so
	// every fork has a net device to remap to its own tap via
	// network_overrides (Firecracker cannot add a NIC on restore). The
	// placeholder tap is created host-side just for the build and removed
	// after; it never carries live traffic. The placeholder MAC is also baked
	// in but is irrelevant: forks restore the same MAC, and each fork sits on
	// its own /30 tap, so the MAC need not be unique on the host bridge.
	if e.networkEnabled() {
		placeholderID := netconf.Identity{
			TapName:  firecracker.PlaceholderTapName,
			GuestMAC: placeholderMAC,
			HostIP:   placeholderHostIP,
			GuestIP:  placeholderGuestIP,
		}
		if err := e.netMgr.Setup(context.Background(), placeholderID, v1alpha1.EgressDeny, nil, nil); err != nil {
			return fmt.Errorf("create placeholder tap for template %s: %w", id, err)
		}
		defer func() {
			if err := e.netMgr.Teardown(context.Background(), placeholderID); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: remove placeholder tap for template %s: %v\n", id, err)
			}
		}()
		cfg.Network = &firecracker.NetworkIdentity{
			IfaceID:     firecracker.NetIfaceID,
			GuestMAC:    placeholderMAC,
			HostDevName: firecracker.PlaceholderTapName,
		}
	}

	if err := e.runTemplateBuild(id, cfg, initCommands); err != nil {
		return err
	}
	d, err := recordTemplateDigest(e.casStore, e.dataDir, id, e.vmmVersion)
	if err != nil {
		return fmt.Errorf("record template %s digest: %w", id, err)
	}
	e.mu.Lock()
	e.templateDigests[id] = d
	e.mu.Unlock()
	return nil
}

// VerifyTemplate re-derives the manifest digest of a template's on-disk
// snapshot from the CAS store and compares it to the recorded digest. On match
// it (re)writes the verified marker and caches the digest; on mismatch it
// returns an error and leaves the template unverified. This is the
// verify-on-load path for templates this process did not build, e.g. ones
// discovered on disk after a forkd restart. It is intentionally NOT called per
// fork: see verify.go for the verify-once-at-registration rationale.
func (e *Engine) VerifyTemplate(id string) error {
	d, err := verifyTemplate(e.dataDir, id, e.vmmVersion)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.templateDigests[id] = d
	e.mu.Unlock()
	return nil
}

// ensureVerified is the cheap Fork-time gate. It stats the verified marker
// (no hashing) and, only if it is absent, performs a one-time lazy
// verification. A failed verification refuses the fork unless allowUnverified
// is set, in which case it logs a loud one-time warning per template and
// proceeds. Steady state hits only the marker stat.
func (e *Engine) ensureVerified(snapshotID string) error {
	if isVerified(e.dataDir, snapshotID) {
		return nil
	}
	if err := e.VerifyTemplate(snapshotID); err != nil {
		if e.allowUnverified {
			e.warnUnverifiedOnce(snapshotID, err)
			return nil
		}
		return fmt.Errorf("refusing to fork unverified snapshot %s: %w (set --allow-unverified-snapshots to override in development)", snapshotID, err)
	}
	return nil
}

// warnUnverifiedOnce emits the loud allow-unverified warning a single time per
// snapshot id. The digest and id are safe to log; no secret values are touched.
func (e *Engine) warnUnverifiedOnce(snapshotID string, cause error) {
	e.mu.Lock()
	_, seen := e.unverifiedWarned[snapshotID]
	if !seen {
		e.unverifiedWarned[snapshotID] = struct{}{}
	}
	e.mu.Unlock()
	if !seen {
		fmt.Fprintf(os.Stderr, "forkd: WARNING forking UNVERIFIED snapshot %s: %v; integrity is NOT enforced because --allow-unverified-snapshots is set (development only)\n", snapshotID, cause)
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
	// TemplateDigests maps each template id to its recorded snapshot manifest
	// digest (a content address, safe to log). The controller records this in
	// the SandboxPool status so the snapshot identity is visible in the CRD.
	TemplateDigests map[string]string
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
