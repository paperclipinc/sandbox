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
	"github.com/paperclipinc/sandbox/internal/metering"
	"github.com/paperclipinc/sandbox/internal/netconf"
	"github.com/paperclipinc/sandbox/internal/network"
	"github.com/paperclipinc/sandbox/internal/snapcompat"
	"github.com/paperclipinc/sandbox/internal/volume"
	"github.com/paperclipinc/sandbox/internal/vsock"
)

type Engine struct {
	mu             sync.RWMutex
	dataDir        string
	templateMgr    *firecracker.TemplateManager
	firecrackerBin string
	kernelPath     string
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
	// env is the host environment detected once at engine start (Firecracker
	// version, CPU model, kernel, restorable format versions). It is stamped
	// into every template manifest at build and checked against every snapshot
	// at load (snapcompat). vmmVersion mirrors env.VMMVersion for the legacy
	// manifest field.
	env        snapcompat.Environment
	vmmVersion string
	// allowIncompatible disables the load-time compatibility refusal
	// (development only). Default false refuses a snapshot whose recorded
	// environment is incompatible with this host.
	allowIncompatible bool
	// unverifiedWarned records snapshot IDs that already emitted the loud
	// allow-unverified warning, so it fires once per template not per fork.
	unverifiedWarned map[string]struct{}
	// incompatibleWarned records snapshot IDs that already emitted the loud
	// allow-incompatible warning, so it fires once per template not per fork.
	incompatibleWarned map[string]struct{}
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

	// DNS-based name egress is opt-in on top of networking. When
	// enableDNSEgres is set and dnsRegistry is non-nil, a fork whose egress
	// allowlist carries DNS-name entries registers them (keyed by its unique
	// guest IP) with the node-wide DNS proxy, and every fork is pointed at the
	// resolver IP via its guest network config so name lookups go through the
	// controlled resolver. When off (the default) name entries stay
	// unenforced, nothing is registered, and the guest's resolv.conf is left
	// untouched: behavior is exactly as before.
	dnsRegistry    DNSRegistry
	enableDNSEgres bool

	// Volumes are opt-in. When enableVolumes is set and volBackend is non-nil,
	// the template build bakes one placeholder drive per template volume into
	// the snapshot, and every fork prepares its OWN backing per fork policy and
	// rebinds the drive to it (PATCH /drives) after the snapshot is resumed.
	// Both off (the default) means volumes are DISABLED and the engine behaves
	// exactly as before: no drives are attached and no backings are prepared.
	enableVolumes bool
	volBackend    *volume.Backend

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

// DNSRegistry is the subset of the dnsproxy registry the engine drives: it maps
// a sandbox's unique guest IP to the DNS names (and ports) it may resolve, and
// removes the mapping on teardown. The real *dnsproxy.Registry satisfies it;
// tests use an in-memory registry. It is a seam so the engine package does not
// depend on the proxy server, only on this narrow contract.
type DNSRegistry interface {
	Register(guestIP net.IP, names map[string][]int)
	Deregister(guestIP net.IP)
}

// networkEnabled reports whether per-fork networking is wired. Both the
// manager and allocator must be present.
func (e *Engine) networkEnabled() bool {
	return e.netMgr != nil && e.netAlloc != nil
}

// dnsEgressEnabled reports whether DNS-based name egress is wired: the flag is
// set, a registry is present, and a resolver IP exists for the guest to query.
// Networking must also be on (the registry keys on the per-fork guest IP).
func (e *Engine) dnsEgressEnabled() bool {
	return e.enableDNSEgres && e.dnsRegistry != nil && e.resolverIP != nil && e.networkEnabled()
}

// volumesEnabled reports whether per-fork volumes are wired. Both the flag and
// the backend must be present.
func (e *Engine) volumesEnabled() bool {
	return e.enableVolumes && e.volBackend != nil
}

// driveRebind maps one baked snapshot drive (by drive id, which equals the
// volume name) to the host backing file the fork prepared for it. After the
// snapshot is loaded and resumed, each rebind is applied with PATCH /drives so
// the fork's drive points at its OWN backing before the guest mounts it. The
// drive id and host path carry no secrets and are safe to log.
type driveRebind struct {
	DriveID    string
	PathOnHost string
}

// prepareForkVolumes prepares a per-fork backing file for each volume spec per
// its fork policy, returning the drive rebinds (drive id -> backing path) the
// caller applies after restore and the Prepared records. It returns (nil, nil,
// nil) when volumes are disabled or there are no specs, so the non-volume path
// is untouched. Each backing is sandbox-scoped, so two forks never share a
// Fresh backing and each Snapshot fork gets its own reflink copy. The Snapshot
// and Clone source is the template's seed backing at
// <dataDir>/templates/<id>/volumes/<name>.ext4, baked into the snapshot at
// build time. On any failure the already-prepared per-fork backings are removed
// so a partial preparation does not leak.
func (e *Engine) prepareForkVolumes(templateID, sandboxID string, specs []volume.Spec) ([]driveRebind, []volume.Prepared, error) {
	if !e.volumesEnabled() || len(specs) == 0 {
		return nil, nil, nil
	}
	rebinds := make([]driveRebind, 0, len(specs))
	prepared := make([]volume.Prepared, 0, len(specs))
	for _, spec := range specs {
		p, err := e.prepareForkVolume(templateID, sandboxID, spec)
		if err != nil {
			// Roll back the per-fork backings prepared so far; the template seed
			// (Share) is never removed here.
			_ = e.volBackend.Cleanup(sandboxID)
			return nil, nil, fmt.Errorf("prepare volume %s for %s: %w", spec.Name, sandboxID, err)
		}
		prepared = append(prepared, p)
		// The drive id is the volume name, matching the placeholder drive the
		// template build attached under the same id.
		rebinds = append(rebinds, driveRebind{DriveID: spec.Name, PathOnHost: p.HostPath})
	}
	return rebinds, prepared, nil
}

// volumeMountTable builds the guest mount table from the per-fork prepared
// volumes, in the SAME order they were attached. Firecracker enumerates block
// devices in attach order: the rootfs drive is /dev/vda and the i-th volume
// drive (0-based) is /dev/vd{b+i}. The guest agent mounts each entry's Device at
// MountPath after the host has rebound the drive to this fork's backing. Returns
// nil when there are no volumes so the guest mounts nothing.
func volumeMountTable(prepared []volume.Prepared) []vsock.VolumeMountEntry {
	if len(prepared) == 0 {
		return nil
	}
	entries := make([]vsock.VolumeMountEntry, 0, len(prepared))
	for i, p := range prepared {
		entries = append(entries, vsock.VolumeMountEntry{
			Device:    volumeDeviceName(i),
			MountPath: p.MountPath,
			ReadOnly:  p.ReadOnly,
		})
	}
	return entries
}

// volumeDeviceName returns the guest block device node for the i-th volume drive
// (0-based). The rootfs is /dev/vda, so volumes follow as /dev/vdb, /dev/vdc, ...
// in attach order. i is bounded by the number of template volumes (small), so a
// single trailing letter is sufficient.
func volumeDeviceName(i int) string {
	return fmt.Sprintf("/dev/vd%c", 'b'+i)
}

// driveReadOnly resolves whether a volume's baked Firecracker drive must be
// read-only at the BLOCK-DEVICE level. Firecracker bakes is_read_only into the
// snapshot at template build time and PATCH /drives cannot flip it on a fork's
// rebind, so the flag must be correct at build time. A volume is read-only at
// the drive iff it is declared spec.ReadOnly OR its resolved policy is Share:
// Share attaches the SAME seed backing to every fork, so a writable drive would
// let one fork corrupt or leak into the shared seed and every sibling fork.
// Fresh/Snapshot/Clone forks each get their own backing, so they stay writable
// unless explicitly marked read-only.
func driveReadOnly(spec volume.Spec) bool {
	return spec.ReadOnly || spec.Policy == volume.ForkPolicyShare
}

// prepareForkVolume dispatches one spec to the backend method for its policy.
// Fresh formats a new empty ext4; Snapshot reflink-copies the template seed;
// Share attaches the template seed read-only with no copy; Clone makes a full
// copy of the template seed.
func (e *Engine) prepareForkVolume(templateID, sandboxID string, spec volume.Spec) (volume.Prepared, error) {
	seed := e.volBackend.TemplateVolumePath(templateID, spec.Name)
	switch spec.Policy {
	case volume.ForkPolicyFresh:
		return e.volBackend.Fresh(spec, sandboxID)
	case volume.ForkPolicySnapshot:
		return e.volBackend.Snapshot(spec, sandboxID, seed)
	case volume.ForkPolicyShare:
		return e.volBackend.Share(spec, sandboxID, seed)
	case volume.ForkPolicyClone:
		return e.volBackend.Clone(spec, sandboxID, seed)
	default:
		// An unset or unknown policy defaults to Fresh: a per-fork empty volume
		// is the safe choice (no shared or copied template content leaks).
		return e.volBackend.Fresh(spec, sandboxID)
	}
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
	dnsOn := e.dnsEgressEnabled()
	// Name-based allow entries are enforced through the controlled DNS resolver
	// only when DNS egress is enabled; otherwise they are parsed but dropped
	// from the ruleset, and we warn loudly so an operator is not misled into
	// thinking a name rule is in effect. The entries are config (host:port),
	// safe to log.
	if len(skipped) > 0 && !dnsOn {
		fmt.Fprintf(os.Stderr, "forkd: WARNING egress allowlist for %s has %d name-based entries that are NOT enforced (DNS egress disabled): %v\n", sandboxID, len(skipped), skipped)
	}
	policy := v1alpha1.EgressPolicy(opts.Network.EgressPolicy)

	id, err := e.netAlloc.Acquire(sandboxID)
	if err != nil {
		return nil, fmt.Errorf("acquire network identity for %s: %w", sandboxID, err)
	}

	// The chain's udp/tcp 53 accept to resolverIP is wired whenever a resolver
	// IP is configured (the standalone --dns-resolver allow rule), independent
	// of DNS egress. The guest is only pointed at the resolver (resolv.conf)
	// when DNS egress is on; otherwise it keeps its existing resolv.conf,
	// preserving the prior behavior.
	var guestResolver string
	if dnsOn {
		guestResolver = e.resolverIP.String()
	}

	if err := e.netMgr.Setup(context.Background(), id, policy, allow, e.resolverIP); err != nil {
		e.netAlloc.Release(sandboxID)
		return nil, fmt.Errorf("set up network for %s (tap %s): %w", sandboxID, id.TapName, err)
	}

	// Register this sandbox's DNS-name allowlist with the proxy, keyed by its
	// unique guest IP (the proxy attributes queries by source IP). Only when
	// DNS egress is on AND the allowlist actually carries name entries; an
	// IP-only allowlist registers nothing.
	if dnsOn && len(skipped) > 0 {
		names, perr := netconf.ParseNameAllowList(opts.Network.AllowList)
		if perr != nil {
			e.netAlloc.Release(sandboxID)
			return nil, fmt.Errorf("parse name allowlist for %s: %w", sandboxID, perr)
		}
		if len(names) > 0 {
			e.dnsRegistry.Register(id.GuestIP, names)
		}
	}

	return &forkNetwork{
		identity: id,
		overrides: []firecracker.NetworkOverride{{
			IfaceID:     firecracker.NetIfaceID,
			HostDevName: id.TapName,
		}},
		guestNet: &vsock.NotifyForkedNetwork{
			GuestIP:    id.GuestIP.String(),
			GatewayIP:  id.HostIP.String(),
			PrefixLen:  30,
			ResolverIP: guestResolver,
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
	// Drop the sandbox's DNS-name registration so its guest IP can no longer
	// resolve allowlisted names (and a reused guest IP does not inherit a stale
	// allowlist). The per-tap nft timeout set is flushed by the manager's
	// Teardown. Safe to call for a guest that was never registered.
	if e.enableDNSEgres && e.dnsRegistry != nil {
		e.dnsRegistry.Deregister(id.GuestIP)
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
	// AllowIncompatible disables the load-time snapshot compatibility refusal
	// (development only). Default false refuses a snapshot built for a different
	// Firecracker, CPU, or snapshot format than this host.
	AllowIncompatible bool
	// Environment, when non-zero (FormatVersions set), overrides automatic
	// detection in NewEngine. Tests inject a known environment here; production
	// leaves it zero so NewEngine detects the real host.
	Environment snapcompat.Environment
	// NetManager and NetAllocator enable per-fork networking. Both must be
	// non-nil to enable it; either nil leaves networking DISABLED (default).
	NetManager   network.Manager
	NetAllocator *netconf.Allocator
	// ResolverIP is the optional DNS resolver guests may reach. Nil omits the
	// DNS allow rule from each fork's egress ruleset.
	ResolverIP net.IP
	// EnableDNSEgress turns on name-based egress: a fork's DNS-name allow
	// entries are registered with DNSRegistry (keyed by its guest IP) and each
	// fork is pointed at ResolverIP. Default false leaves name entries
	// unenforced and the guest's resolv.conf untouched (prior behavior).
	// DNSRegistry and ResolverIP and networking must all be set for it to take
	// effect.
	EnableDNSEgress bool
	// DNSRegistry is the dnsproxy registry the engine registers/deregisters
	// per-fork name allowlists with. Nil leaves DNS egress disabled.
	DNSRegistry DNSRegistry
	// AgentBinPath is the host path of the guest agent binary injected as
	// /init when CreateTemplate builds a rootfs from an OCI image. It is
	// REQUIRED for image builds and unused for file-path rootfs templates.
	// For now forkd must be shipped/mounted with this binary present; a
	// follow-up will go:embed the agent into forkd so the flag is optional.
	AgentBinPath string
	// BusyboxPath is an optional static /bin/sh source injected when an image
	// lacks a shell. Empty means images without a shell cannot run init.
	BusyboxPath string
	// EnableVolumes turns on per-fork volume drives: the template build bakes a
	// placeholder drive per template volume and each fork prepares its own
	// backing and rebinds the drive. Default false leaves volumes DISABLED (no
	// drives attached, existing behavior). VolumeBackend must be set when true.
	EnableVolumes bool
	// VolumeBackend prepares volume backing files under the data dir. Nil leaves
	// volumes disabled; when EnableVolumes is true NewEngine constructs a
	// default backend rooted at <dataDir> if this is nil.
	VolumeBackend *volume.Backend
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
	// hasVolumes records whether this sandbox had per-fork volume backings
	// prepared, so Terminate cleans them up. False means Terminate skips the
	// volume cleanup entirely (no backend call for sandboxes without volumes).
	hasVolumes bool
	// volumes are the resolved per-fork volume specs this sandbox was forked
	// with (nil when volumes are disabled or none were requested). The disk
	// metering path uses them to find each volume's backing and seed paths;
	// they are not on the hot fork path.
	volumes []volume.Spec
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
	// VolumeMounts, when non-empty, is the per-fork volume mount table the daemon
	// delivers to the guest in the NotifyForked message AFTER the drives are
	// rebound, so the guest mounts each device at its mount path. Nil when
	// volumes are disabled or the fork carried none. Device nodes and paths are
	// safe to log.
	VolumeMounts []vsock.VolumeMountEntry
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

	// Volumes are opt-in. When enabled, default the backend to one rooted at the
	// data dir so backing files live alongside snapshots and sandboxes.
	volBackend := opts.VolumeBackend
	if opts.EnableVolumes && volBackend == nil {
		volBackend = volume.New(dataDir)
	}

	// Detect the host environment once (Firecracker version, CPU, kernel) so
	// every template manifest is stamped with it and every snapshot is checked
	// against it on load. Tests may inject a known environment; production
	// detects the real host and surfaces a detection failure rather than
	// silently running with an unknown environment.
	env := opts.Environment
	if len(env.FormatVersions) == 0 {
		detected, derr := snapcompat.DetectEnvironment(firecrackerBin, snapcompat.ExecRunner, snapcompat.ProcCPUInfoReader)
		if derr != nil {
			return nil, fmt.Errorf("detect host environment for snapshot compatibility: %w", derr)
		}
		env = detected
	}

	tmplMgr := firecracker.NewTemplateManager(firecrackerBin, kernelPath, dataDir, jailer)
	e := &Engine{
		dataDir:              dataDir,
		firecrackerBin:       firecrackerBin,
		kernelPath:           kernelPath,
		jailer:               jailer,
		templateMgr:          tmplMgr,
		sandboxes:            make(map[string]*Sandbox),
		nextPort:             10000,
		casStore:             store,
		allowUnverified:      opts.AllowUnverified,
		allowIncompatible:    opts.AllowIncompatible,
		env:                  env,
		vmmVersion:           env.VMMVersion,
		unverifiedWarned:     make(map[string]struct{}),
		incompatibleWarned:   make(map[string]struct{}),
		templateDigests:      make(map[string]cas.Digest),
		netMgr:               opts.NetManager,
		netAlloc:             opts.NetAllocator,
		resolverIP:           opts.ResolverIP,
		dnsRegistry:          opts.DNSRegistry,
		enableDNSEgres:       opts.EnableDNSEgress,
		agentBinPath:         opts.AgentBinPath,
		busyboxPath:          opts.BusyboxPath,
		enableVolumes:        opts.EnableVolumes,
		volBackend:           volBackend,
		buildRootfsFromImage: buildRootfsFromImage,
	}
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		_, err := tmplMgr.CreateTemplate(id, cfg, initCommands)
		return err
	}
	return e, nil
}

// manifestMetadata builds the CAS manifest metadata stamped into a template's
// snapshot manifest: the current snapshot format version, the detected host
// environment (Firecracker version, CPU model, kernel), and the config hash.
// CreatedUnix is fixed at 0 so the digest is a pure content address (build time
// is not part of the snapshot identity).
//
// The config hash covers only stable inputs (vcpu count, memory, and the
// engine's kernel path), NOT mutable per-build fields like the temp rootfs path:
// the rootfs bytes are already chunked into the manifest's file entries, and a
// transient build path would make the digest irreproducible at verify time. This
// is what lets recordTemplateDigest and verifyTemplate (which passes the default
// config) derive the same digest from the same on-disk snapshot.
func (e *Engine) manifestMetadata(cfg firecracker.VMConfig) cas.Metadata {
	return cas.Metadata{
		SnapshotFormatVersion: cas.CurrentSnapshotFormatVersion,
		VMMVersion:            e.env.VMMVersion,
		CPUModel:              e.env.CPUModel,
		KernelVersion:         e.env.KernelVersion,
		ConfigHash:            snapcompat.ConfigHash(cfg.VcpuCount, cfg.MemSizeMib, e.kernelPath, ""),
	}
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
	// Compatibility gate (#32): the verified snapshot's recorded environment
	// (Firecracker version, CPU model, format) must be restorable on this host.
	// Runs AFTER the digest verify and BEFORE any Firecracker launch, so an
	// incompatible snapshot is refused without starting a VM.
	if err := e.ensureCompatible(snapshotID); err != nil {
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

	// Per-fork volumes (opt-in). Prepare a distinct backing per volume per the
	// fork policy BEFORE the snapshot is loaded so the files exist; the drive
	// rebinds are applied with PATCH after resume. Returns nil when volumes are
	// disabled or the fork carries none.
	rebinds, prepared, err := e.prepareForkVolumes(snapshotID, sandboxID, opts.Volumes)
	if err != nil {
		_ = fcClient.Kill()
		if fnet != nil {
			e.teardownForkNetwork(sandboxID, fnet.identity)
		}
		return nil, err
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
		if len(rebinds) > 0 {
			_ = e.volBackend.Cleanup(sandboxID)
		}
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	// Rebind each baked placeholder volume drive to THIS fork's backing now that
	// the snapshot is loaded and resumed. The guest mounts the volumes only
	// after it receives the post-restore vsock mount table (Task 4), so the
	// PATCH lands before the device is in use. Firecracker supports updating a
	// drive's path_on_host on a restored+resumed VM (v1.15).
	for _, rb := range rebinds {
		if err := fcClient.PatchDrive(rb.DriveID, rb.PathOnHost); err != nil {
			_ = fcClient.Kill()
			if fnet != nil {
				e.teardownForkNetwork(sandboxID, fnet.identity)
			}
			_ = e.volBackend.Cleanup(sandboxID)
			return nil, fmt.Errorf("rebind volume drive %s for %s: %w", rb.DriveID, sandboxID, err)
		}
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
		TemplateID: snapshotID,
		SnapshotID: snapshotID,
		Endpoint:   endpoint,
		Pid:        fcClient.PID(),
		CreatedAt:  time.Now(),
		fcClient:   fcClient,
		VsockPath:  vsockPath,
		rootfsPath: rootfsPath,
		volumes:    opts.Volumes,
	}
	var guestNet *vsock.NotifyForkedNetwork
	if fnet != nil {
		sandbox.netID = fnet.identity
		guestNet = fnet.guestNet
	}
	sandbox.hasVolumes = len(rebinds) > 0

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
		VolumeMounts: volumeMountTable(prepared),
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

	// Remove this fork's per-volume backing files. Skipped for sandboxes
	// without volumes so no backend call is made on the common path. The
	// sandbox dir RemoveAll below also covers the volumes, but Cleanup is the
	// backend's own contract and keeps the path explicit; both are best effort.
	if sandbox.hasVolumes && e.volBackend != nil {
		if err := e.volBackend.Cleanup(sandboxID); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: cleanup volumes for %s: %v\n", sandboxID, err)
		}
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

// GetCapacity returns the current node capacity. Memory is CoW-aware: forks of
// the same template map the SAME shared page set (MAP_PRIVATE restore of one
// snapshot), so the shared footprint is counted ONCE per template rather than
// once per fork. Capacity.MemoryUsed is the honest resident total
// (sum of per-fork unique + each template's shared set counted once) and
// Capacity.MemoryShared is the shared-once footprint across all templates.
// GetCapacity stays memory-only and lock-light on the hot heartbeat path; disk
// metering (which stats backing files) lives in Metering(), not here.
func (e *Engine) GetCapacity() Capacity {
	e.mu.RLock()
	samples := e.memorySamplesLocked()
	activeSandboxes := int32(len(e.sandboxes))
	digestsByID := make(map[string]string, len(e.templateDigests))
	for id, d := range e.templateDigests {
		digestsByID[id] = string(d)
	}
	e.mu.RUnlock()

	report := metering.Aggregate(samples)

	templateIDs, _ := e.templateMgr.ListTemplates()

	digests := make(map[string]string, len(templateIDs))
	for _, id := range templateIDs {
		if d, ok := digestsByID[id]; ok {
			digests[id] = d
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
		ActiveSandboxes: activeSandboxes,
		// MemoryUsed is the CoW-aware resident total (per-fork unique plus each
		// template's shared set counted once), NOT the naive per-fork sum.
		MemoryUsed: report.UsedCoWAware,
		// MemoryShared is the shared-once footprint summed over templates
		// (UsedCoWAware minus the unique total): the honest shared bytes.
		MemoryShared:    report.SharedOnceTotal(),
		TemplateIDs:     templateIDs,
		TemplateDigests: digests,
		KVMAvailable:    true,
	}
}

// memorySamplesLocked builds the per-sandbox memory metering samples from the
// live sandbox map. It assumes e.mu is held (read or write). Disk fields are
// left zero here: GetCapacity is the hot heartbeat path and must not stat
// backing files; disk accounting is gathered in Metering() instead.
func (e *Engine) memorySamplesLocked() []metering.Sample {
	samples := make([]metering.Sample, 0, len(e.sandboxes))
	for _, s := range e.sandboxes {
		samples = append(samples, metering.Sample{
			ID:           s.ID,
			Template:     s.TemplateID,
			MemoryUnique: s.MemoryUnique,
			MemoryShared: s.MemoryShared,
		})
	}
	return samples
}

// meteringSnapshot is the immutable per-sandbox view Metering() copies out under
// the lock so the (slower) disk stat work runs without holding e.mu.
type meteringSnapshot struct {
	id           string
	template     string
	memoryUnique int64
	memoryShared int64
	hasVolumes   bool
	volumes      []volume.Spec
}

// Metering returns the full CoW-aware node metering report (per-sandbox and
// per-template memory plus disk). Unlike GetCapacity (the hot heartbeat path,
// memory-only), Metering also stats each sandbox's volume backing files and
// template seeds, so it is meant for the operator/billing endpoint rather than
// the fork hot path.
//
// Disk rule (honest v1): for each sandbox volume,
//   - a Fresh volume's backing apparent size counts entirely as DiskUnique
//     (nothing shared with siblings);
//   - a Snapshot (reflink) volume shares the template seed, so the seed
//     apparent size counts once per (template, volume) as DiskShared and the
//     fork's divergence is approximated as max(0, forkBacking - seed) DiskUnique
//     using os.Stat APPARENT sizes;
//   - a Clone volume is a FULL byte-for-byte copy (no reflink), so its whole
//     fork backing counts as DiskUnique with nothing shared;
//   - a Share volume maps the seed directly, so the seed counts as DiskShared
//     with no per-fork divergence.
//
// The apparent-size divergence is an approximation: precise reflink block
// accounting (which blocks actually diverged) is a follow-up. We document it as
// approximate rather than claim exact CoW disk numbers.
func (e *Engine) Metering() metering.Report {
	e.mu.RLock()
	snaps := make([]meteringSnapshot, 0, len(e.sandboxes))
	for _, s := range e.sandboxes {
		var vols []volume.Spec
		if s.hasVolumes && len(s.volumes) > 0 {
			vols = append(vols, s.volumes...)
		}
		snaps = append(snaps, meteringSnapshot{
			id:           s.ID,
			template:     s.TemplateID,
			memoryUnique: s.MemoryUnique,
			memoryShared: s.MemoryShared,
			hasVolumes:   s.hasVolumes,
			volumes:      vols,
		})
	}
	e.mu.RUnlock()

	samples := make([]metering.Sample, 0, len(snaps))
	for _, sn := range snaps {
		du, ds := e.diskFootprint(sn)
		samples = append(samples, metering.Sample{
			ID:           sn.id,
			Template:     sn.template,
			MemoryUnique: sn.memoryUnique,
			MemoryShared: sn.memoryShared,
			DiskUnique:   du,
			DiskShared:   ds,
		})
	}
	return metering.Aggregate(samples)
}

// diskFootprint returns this sandbox's (unique, shared) backing-storage bytes
// using apparent file sizes. Returns zeros when volumes are disabled or no
// backend is configured.
func (e *Engine) diskFootprint(sn meteringSnapshot) (unique, shared int64) {
	if !sn.hasVolumes || e.volBackend == nil {
		return 0, 0
	}
	for _, spec := range sn.volumes {
		seed := e.volBackend.TemplateVolumePath(sn.template, spec.Name)
		seedSize := apparentSize(seed)
		switch spec.Policy {
		case volume.ForkPolicyFresh, volume.ForkPolicyClone:
			// Fresh: empty per-fork backing. Clone: full byte-for-byte copy.
			// Either way nothing is shared on disk with siblings.
			unique += apparentSize(e.volBackend.VolumePath(sn.id, spec.Name))
		case volume.ForkPolicyShare:
			// Maps the seed directly: shared, no per-fork backing.
			shared += seedSize
		case volume.ForkPolicySnapshot:
			shared += seedSize
			forkSize := apparentSize(e.volBackend.VolumePath(sn.id, spec.Name))
			if d := forkSize - seedSize; d > 0 {
				unique += d
			}
		default:
			// Unknown policy: count the fork backing as unique (conservative).
			unique += apparentSize(e.volBackend.VolumePath(sn.id, spec.Name))
		}
	}
	return unique, shared
}

// apparentSize is the os.Stat byte size of path, or 0 if it cannot be stat-ed.
// "Apparent" because it is the logical file length, not the allocated blocks;
// precise reflink-shared block accounting is a follow-up.
func apparentSize(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
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
func (e *Engine) CreateTemplate(id string, image string, initCommands []string, volumes []volume.Spec) error {
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

	// When volumes are enabled, create one seed backing per template volume and
	// bake a placeholder drive for it into the snapshot. The seed is an empty
	// ext4 of the spec size at <dataDir>/templates/<id>/volumes/<name>.ext4; it
	// is the source for Snapshot/Share/Clone forks and the placeholder backing
	// the snapshot's block device points at. Firecracker cannot add a drive on
	// restore, so the device must exist at snapshot time; each fork rebinds it.
	if e.volumesEnabled() && len(volumes) > 0 {
		drives := make([]firecracker.VolumeDrive, 0, len(volumes))
		for _, spec := range volumes {
			seed, err := e.volBackend.FreshTemplate(spec, id)
			if err != nil {
				return fmt.Errorf("seed template volume %s for %s: %w", spec.Name, id, err)
			}
			drives = append(drives, firecracker.VolumeDrive{
				DriveID:    spec.Name,
				PathOnHost: seed,
				ReadOnly:   driveReadOnly(spec),
			})
		}
		cfg.VolumeDrives = drives
	}

	if err := e.runTemplateBuild(id, cfg, initCommands); err != nil {
		return err
	}
	d, err := recordTemplateDigest(e.casStore, e.dataDir, id, e.manifestMetadata(cfg))
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
	d, err := verifyTemplate(e.dataDir, id, e.manifestMetadata(firecracker.DefaultVMConfig()))
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

// ensureCompatible loads the snapshot's recorded manifest from the CAS store
// (keyed by the on-disk recorded digest) and checks its stamped environment
// against this host. An incompatible snapshot is refused with the actionable
// snapcompat error UNLESS allowIncompatible is set, in which case it logs a loud
// one-time warning per template and proceeds. A snapshot with no recorded digest
// or manifest yet (e.g. a live-fork checkpoint that was never content-addressed)
// is treated as compatible: there is nothing to check, and ensureVerified
// already governs integrity.
func (e *Engine) ensureCompatible(snapshotID string) error {
	d, err := readDigestFile(e.dataDir, snapshotID)
	if err != nil {
		// No recorded digest: nothing to check (e.g. live-fork checkpoints).
		return nil
	}
	m, err := e.casStore.GetManifest(d)
	if err != nil {
		// No stored manifest: nothing to check here; integrity is governed by
		// ensureVerified, which would already have refused a tampered snapshot.
		return nil
	}
	if cerr := snapcompat.Check(m, e.env); cerr != nil {
		if e.allowIncompatible {
			e.warnIncompatibleOnce(snapshotID, cerr)
			return nil
		}
		return fmt.Errorf("refusing to fork incompatible snapshot %s: %w (set --allow-incompatible-snapshots to override in development)", snapshotID, cerr)
	}
	return nil
}

// warnIncompatibleOnce emits the loud allow-incompatible warning a single time
// per snapshot id. The id and the cause carry no secret values and are safe to
// log.
func (e *Engine) warnIncompatibleOnce(snapshotID string, cause error) {
	e.mu.Lock()
	_, seen := e.incompatibleWarned[snapshotID]
	if !seen {
		e.incompatibleWarned[snapshotID] = struct{}{}
	}
	e.mu.Unlock()
	if !seen {
		fmt.Fprintf(os.Stderr, "forkd: WARNING forking INCOMPATIBLE snapshot %s: %v; compatibility is NOT enforced because --allow-incompatible-snapshots is set (development only)\n", snapshotID, cause)
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
	// Volumes are the per-fork volume specs resolved from the template (with
	// VolumeOverrides applied) and threaded through the Fork RPC. The real
	// engine prepares and attaches their backing drives; until that lands
	// (Task 3) it carries them without acting on them.
	Volumes []volume.Spec
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
