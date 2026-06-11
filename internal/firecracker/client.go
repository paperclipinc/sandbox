package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Client talks to a single Firecracker process via its Unix socket API.
type Client struct {
	socketPath string
	http       *http.Client
	process    *os.Process
	// workDir is the working directory the Firecracker process was
	// launched with (cmd.Dir). A relative vsock uds_path is bound by
	// Firecracker against this directory, so the host path of the vsock
	// socket is workDir/<uds_path>. Empty for ConnectVM clients.
	workDir string
	// wait reaps the launched process (exec.Cmd.Wait for StartVM
	// clients). Kill calls it after the kill signal so the process is
	// gone before its uid is released; nil for clients without a child
	// process (ConnectVM).
	wait func() error

	// Jailer state; zero values for direct exec.
	id          string // validated VM id (passed validateVMID), "" for ConnectVM
	chrootDir   string // host path of the chroot root, "" when not jailed
	jailerVMDir string // per-VM jailer workspace, removed on Kill
	dataDir     string // forkd data dir; bounds export-from-jail host paths
	jailedUID   uint32
	jailedGID   uint32
	allocator   *UIDAllocator
}

// StartVM launches a Firecracker process and returns a client connected
// to it. With cfg.Jailer enabled the process is launched through the
// jailer binary inside a per-VM chroot under a dedicated uid/gid; with
// the zero JailerConfig the firecracker binary is exec'd directly,
// exactly as before.
func StartVM(cfg VMConfig) (*Client, error) {
	if cfg.Jailer.Enabled() {
		return startJailedVM(cfg)
	}

	// Validate the id before it is used anywhere, so the same allowlist
	// barrier that protects the jailed path builders also guards direct
	// exec. An empty id is allowed here (direct exec does not require one);
	// a non-empty id must pass the allowlist.
	if cfg.ID != "" {
		if err := validateVMID(cfg.ID); err != nil {
			return nil, err
		}
	}

	socketPath := cfg.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(cfg.WorkDir, "firecracker.sock")
	}

	os.Remove(socketPath)

	args := []string{
		"--api-sock", socketPath,
	}
	if cfg.ID != "" {
		args = append(args, "--id", cfg.ID)
	}

	cmd := exec.Command(cfg.FirecrackerBin, args...)
	cmd.Dir = cfg.WorkDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	client := &Client{
		socketPath: socketPath,
		process:    cmd.Process,
		wait:       cmd.Wait,
		workDir:    cfg.WorkDir,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}

	if err := client.waitReady(5 * time.Second); err != nil {
		_ = cmd.Process.Kill()
		// Reap the failed launch so it does not linger as a zombie (I2).
		_ = cmd.Wait()
		return nil, fmt.Errorf("firecracker not ready: %w", err)
	}

	return client, nil
}

// startJailedVM launches Firecracker through the jailer: it allocates a
// per-VM uid/gid, hard-links the configured files into the per-VM
// chroot, and waits for the API socket at its jailed location.
func startJailedVM(cfg VMConfig) (*Client, error) {
	if cfg.ID == "" {
		return nil, fmt.Errorf("jailer launch requires a VM id (jailer --id)")
	}
	// Allowlist barrier: the id is joined into every per-VM path below, so
	// validate it before ANY path is built from it (CodeQL go/path-injection
	// sanitizer). All downstream path builders consume this validated id.
	if err := validateVMID(cfg.ID); err != nil {
		return nil, err
	}
	id := cfg.ID
	if filepath.Base(cfg.FirecrackerBin) != jailerExecFileName {
		return nil, fmt.Errorf("jailer launch requires the firecracker binary to be named %q (the jailer derives the chroot layout from the --exec-file basename); got %q", jailerExecFileName, cfg.FirecrackerBin)
	}
	if cfg.Jailer.Allocator == nil {
		return nil, fmt.Errorf("jailer launch requires a uid allocator; construct the engine with a uid range")
	}
	// C1 defense in depth: refuse ids whose `..` segments would move the
	// per-VM directories outside the chroot base, before any allocation
	// or filesystem operation derived from the id.
	if err := guardJailerLayout(cfg); err != nil {
		return nil, err
	}

	uid, gid, err := cfg.Jailer.Allocator.Acquire()
	if err != nil {
		return nil, fmt.Errorf("allocate jailer uid for %s: %w", cfg.ID, err)
	}
	launched := false
	defer func() {
		if !launched {
			cfg.Jailer.Allocator.Release(uid)
		}
	}()

	chrootDir := jailerChrootDir(cfg.Jailer.ChrootBaseDir, id)
	if err := os.MkdirAll(filepath.Join(chrootDir, "run"), 0o755); err != nil {
		return nil, fmt.Errorf("create chroot run dir: %w", err)
	}
	if _, err := prepareChroot(cfg, id, cfg.ChrootFiles); err != nil {
		return nil, fmt.Errorf("prepare chroot for %s: %w", id, err)
	}
	chownIntoJail(chrootDir, cfg, id, uid, gid)

	socketPath := jailedAPISocketPath(cfg.Jailer.ChrootBaseDir, id)
	os.Remove(socketPath)

	cmd := exec.Command(cfg.Jailer.JailerBin, jailerArgs(cfg, id, uid, gid)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start jailer: %w", err)
	}

	client := &Client{
		socketPath:  socketPath,
		process:     cmd.Process,
		wait:        cmd.Wait,
		id:          id,
		chrootDir:   chrootDir,
		jailerVMDir: jailerVMDir(cfg.Jailer.ChrootBaseDir, id),
		dataDir:     cfg.Jailer.DataDir,
		jailedUID:   uid,
		jailedGID:   gid,
		allocator:   cfg.Jailer.Allocator,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}

	if err := client.waitReady(10 * time.Second); err != nil {
		_ = cmd.Process.Kill()
		// Reap before the deferred uid Release (I2): the uid must not be
		// reusable while the killed jailer can still run under it.
		_ = cmd.Wait()
		return nil, fmt.Errorf("jailed firecracker not ready: %w", err)
	}

	launched = true
	return client, nil
}

// chownIntoJail hands the prepared chroot files and the API socket dir
// to the jailed uid/gid so the deprivileged Firecracker can open them.
// Failures are logged (path only, never contents) and not fatal: on a
// correctly deployed root forkd they do not happen, and the VM fails
// later with a clear permission error if one slipped through.
func chownIntoJail(chrootDir string, cfg VMConfig, id string, uid, gid uint32) {
	targets := []string{filepath.Join(chrootDir, "run")}
	for _, f := range cfg.ChrootFiles {
		targets = append(targets, chrootPath(cfg.Jailer.ChrootBaseDir, id, f))
	}
	for _, t := range targets {
		if err := os.Chown(t, int(uid), int(gid)); err != nil {
			fmt.Fprintf(os.Stderr, "firecracker: chown %s to jailed uid %d failed: %v\n", t, uid, err)
		}
	}
}

// HostPath maps a path as Firecracker sees it over its API to the host
// location of the same file. For a jailed VM that is the mirrored path
// inside the chroot; for direct exec it is the path itself.
func (c *Client) HostPath(p string) string {
	if c.chrootDir == "" {
		return p
	}
	return filepath.Join(c.chrootDir, filepath.Clean(p))
}

// VsockHostPath returns the host path at which Firecracker binds the vsock
// UDS for a (relative) uds_path. Firecracker resolves a relative uds_path
// against its own working directory: in direct-exec mode that is the
// per-VM WorkDir (cmd.Dir), in jailer mode it is the chroot root after the
// jailer chdir's into it. Either way distinct VMs get distinct sockets,
// which is the whole point of baking a relative path into the snapshot.
func (c *Client) VsockHostPath(relUDSPath string) string {
	base := c.workDir
	if c.chrootDir != "" {
		base = c.chrootDir
	}
	return filepath.Join(base, relUDSPath)
}

// ConnectVM connects to an already-running Firecracker instance.
func ConnectVM(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(c.socketPath); err == nil {
			if _, err := c.get("/"); err == nil {
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for socket %s", c.socketPath)
}

// --- VM Configuration ---

func (c *Client) SetBootSource(kernel string, bootArgs string) error {
	return c.put("/boot-source", BootSource{
		KernelImagePath: kernel,
		BootArgs:        bootArgs,
	})
}

func (c *Client) SetMachineConfig(vcpus int, memMB int) error {
	return c.put("/machine-config", MachineConfig{
		VcpuCount:  vcpus,
		MemSizeMib: memMB,
	})
}

func (c *Client) AddDrive(driveID string, path string, readOnly bool, rootDevice bool) error {
	return c.put("/drives/"+driveID, Drive{
		DriveID:      driveID,
		PathOnHost:   path,
		IsReadOnly:   readOnly,
		IsRootDevice: rootDevice,
	})
}

// PatchDrive rebinds an existing drive's backing file to pathOnHost via
// PATCH /drives/{drive_id}. Firecracker has long supported updating a drive's
// path_on_host on a configured VM, including one restored from a snapshot. It
// is how each fork gives its baked placeholder volume drive its OWN backing:
// the snapshot bakes the block device by driveID, and every fork PATCHes that
// driveID to the fork's prepared backing after the snapshot is loaded and
// resumed but before the guest mounts it. The drive id and host path carry no
// secrets and are safe to log.
func (c *Client) PatchDrive(driveID, pathOnHost string) error {
	return c.patch("/drives/"+driveID, DrivePatch{
		DriveID:    driveID,
		PathOnHost: pathOnHost,
	})
}

// SetNetwork attaches a guest NIC bound to a host tap device via
// PUT /network-interfaces/{ifaceID}. It must be called before InstanceStart
// (Firecracker does not support hot-plugging a NIC after boot). For a
// fresh-boot sandbox this gives the guest its egress device; for template
// creation it bakes a placeholder NIC into the snapshot that forks later
// remap with LoadSnapshotWithOverrides. The MAC and tap name are safe to log.
func (c *Client) SetNetwork(ifaceID, guestMAC, hostDevName string) error {
	return c.put("/network-interfaces/"+ifaceID, NetworkInterface{
		IfaceID:     ifaceID,
		GuestMAC:    guestMAC,
		HostDevName: hostDevName,
	})
}

func (c *Client) SetVsock(guestCID int, udsPath string) error {
	// For a jailed VM Firecracker binds the UDS inside its chroot; the
	// mirrored parent directory must exist and be writable by the jailed
	// uid before the API call.
	if err := c.ensureJailedDir(filepath.Dir(udsPath)); err != nil {
		return fmt.Errorf("prepare vsock dir in chroot: %w", err)
	}
	return c.put("/vsock", Vsock{
		GuestCID: guestCID,
		UdsPath:  udsPath,
	})
}

// ensureJailedDir creates the in-chroot mirror of a host directory and
// hands it to the jailed uid. No-op for direct-exec clients.
func (c *Client) ensureJailedDir(hostDir string) error {
	if c.chrootDir == "" {
		return nil
	}
	dir := c.HostPath(hostDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(dir, int(c.jailedUID), int(c.jailedGID)); err != nil {
		fmt.Fprintf(os.Stderr, "firecracker: chown %s to jailed uid %d failed: %v\n", dir, c.jailedUID, err)
	}
	return nil
}

// --- VM Lifecycle ---

func (c *Client) Start() error {
	return c.put("/actions", Action{ActionType: "InstanceStart"})
}

func (c *Client) Pause() error {
	return c.patch("/vm", VMState{State: "Paused"})
}

func (c *Client) Resume() error {
	return c.patch("/vm", VMState{State: "Resumed"})
}

// --- Snapshot Operations ---

func (c *Client) CreateSnapshot(memPath, snapshotPath string) error {
	// A jailed Firecracker writes both files inside its chroot; the
	// mirrored destination dirs must exist and be writable by the jailed
	// uid first, and the results are linked back out to the requested
	// host paths afterwards so callers see them where they asked.
	for _, p := range []string{memPath, snapshotPath} {
		if err := c.ensureJailedDir(filepath.Dir(p)); err != nil {
			return fmt.Errorf("prepare snapshot dir in chroot: %w", err)
		}
	}
	if err := c.put("/snapshot/create", SnapshotCreate{
		SnapshotType: "Full",
		SnapshotPath: snapshotPath,
		MemFilePath:  memPath,
	}); err != nil {
		return err
	}
	for _, p := range []string{memPath, snapshotPath} {
		if err := c.exportFromJail(p); err != nil {
			return fmt.Errorf("export snapshot file from chroot: %w", err)
		}
	}
	return nil
}

// exportFromJail hard-links a file Firecracker produced inside the
// chroot back to its host path (copy on EXDEV). No-op for direct exec.
//
// The destination host path is bounded to the forkd data dir with a
// canonical containment check (filepath.Clean plus a separator-anchored
// prefix) before it reaches any os.* sink. This is the CodeQL-recognized
// sanitizer for the snapshot export flow (go/path-injection): the snapshot
// mem and vmstate paths originate from caller-supplied sandbox ids, and this
// barrier guarantees a cleaned path cannot escape the data dir.
func (c *Client) exportFromJail(hostPath string) error {
	if c.chrootDir == "" {
		return nil
	}
	if err := guardExportPath(hostPath, c.dataDir); err != nil {
		return err
	}
	src := c.HostPath(hostPath)
	if same, err := sameInode(src, hostPath); err == nil && same {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return err
	}
	if err := os.Remove(hostPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(src, hostPath); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		return copyFile(src, hostPath)
	}
	return nil
}

func (c *Client) LoadSnapshot(memPath, snapshotPath string, resumeVM bool) error {
	return c.LoadSnapshotWithOverrides(memPath, snapshotPath, resumeVM, nil)
}

// LoadSnapshotWithOverrides loads a snapshot like LoadSnapshot but additionally
// remaps the snapshot's network interfaces to fresh host taps via the
// network_overrides field (Firecracker >= v1.12; pinned CI is v1.15). This is
// how each fork of one shared snapshot binds its OWN tap: the snapshot bakes a
// placeholder NIC by iface_id, and every fork passes an override mapping that
// iface_id to the fork's freshly created tap. Passing nil overrides is
// identical to LoadSnapshot (the field is omitted, preserving prior behavior).
func (c *Client) LoadSnapshotWithOverrides(memPath, snapshotPath string, resumeVM bool, overrides []NetworkOverride) error {
	return c.put("/snapshot/load", SnapshotLoad{
		SnapshotPath:        snapshotPath,
		MemFilePath:         memPath,
		EnableDiffSnapshots: false,
		ResumeVM:            resumeVM,
		NetworkOverrides:    overrides,
	})
}

// --- Process Management ---

func (c *Client) Kill() error {
	var killErr error
	if c.process != nil {
		killErr = c.process.Kill()
		// Reap the killed process BEFORE releasing its uid (I2): until
		// the wait returns, the process can still run under the jailed
		// uid, and releasing first could hand that uid to a new VM while
		// the old one lives. The wait error is ignored; it reports the
		// kill signal, and the zombie is reaped either way.
		if c.wait != nil {
			_ = c.wait()
		} else {
			_, _ = c.process.Wait()
		}
	}
	// Jailed VMs: return the dedicated uid to the pool and remove the
	// per-VM chroot workspace (hard links only; originals stay put).
	if c.allocator != nil {
		c.allocator.Release(c.jailedUID)
		c.allocator = nil
	}
	if c.jailerVMDir != "" {
		if err := os.RemoveAll(c.jailerVMDir); err != nil {
			fmt.Fprintf(os.Stderr, "firecracker: remove jailer dir %s: %v\n", c.jailerVMDir, err)
		}
	}
	return killErr
}

func (c *Client) PID() int {
	if c.process != nil {
		return c.process.Pid
	}
	return 0
}

// --- HTTP helpers ---

func (c *Client) put(path string, body interface{}) error {
	return c.do(http.MethodPut, path, body)
}

func (c *Client) patch(path string, body interface{}) error {
	return c.do(http.MethodPatch, path, body)
}

func (c *Client) get(path string) ([]byte, error) {
	resp, err := c.http.Get("http://localhost" + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, string(data))
	}
	return data, nil
}

func (c *Client) do(method, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	return nil
}
