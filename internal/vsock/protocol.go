package vsock

// Protocol for host ↔ guest agent communication over vsock.
// JSON-encoded messages, newline-delimited.

type RequestType string

const (
	TypeExec         RequestType = "exec"
	TypeReadFile     RequestType = "read_file"
	TypeWriteFile    RequestType = "write_file"
	TypeListDir      RequestType = "list_dir"
	TypeMkdir        RequestType = "mkdir"
	TypeRemove       RequestType = "remove"
	TypePing         RequestType = "ping"
	TypeConfigure    RequestType = "configure"
	TypeNotifyForked RequestType = "notify_forked"
	TypeTarDir       RequestType = "tar_dir"
	TypeUntarDir     RequestType = "untar_dir"
	TypeExecStream   RequestType = "exec_stream"
)

// MaxTarBytes bounds a single TarDir/UntarDir payload (the raw tar bytes, before
// the base64 the JSON encoder applies to a []byte field). The tar of a guest
// directory is buffered whole in memory on both the guest and host (this slice
// does not stream), so the cap keeps the workspace transfer to one bounded
// allocation per side. A directory whose tar would exceed this is refused rather
// than risking an unbounded guest allocation; a streaming (chunked) transfer for
// very large workspaces is a later W4 slice. The guest enforces the cap on the
// tar it produces (TarDir) and on the tar it accepts (UntarDir); the host client
// enforces it before sending. MaxMessageBytes is the matching line-buffer size
// on both vsock ends (the base64 JSON message is ~4/3 the raw bytes plus
// framing).
const MaxTarBytes = 64 << 20

// MaxMessageBytes is the vsock line-buffer capacity on both the host client and
// the guest agent. It must hold the largest framed JSON message, which for the
// tar ops is the base64 encoding of up to MaxTarBytes (~4/3) plus the JSON
// envelope, with headroom.
const MaxMessageBytes = 96 << 20

type Request struct {
	Type         RequestType          `json:"type"`
	Exec         *ExecRequest         `json:"exec,omitempty"`
	ReadFile     *ReadFileRequest     `json:"read_file,omitempty"`
	WriteFile    *WriteFileRequest    `json:"write_file,omitempty"`
	ListDir      *ListDirRequest      `json:"list_dir,omitempty"`
	Mkdir        *MkdirRequest        `json:"mkdir,omitempty"`
	Remove       *RemoveRequest       `json:"remove,omitempty"`
	Configure    *ConfigureRequest    `json:"configure,omitempty"`
	NotifyForked *NotifyForkedRequest `json:"notify_forked,omitempty"`
	TarDir       *TarDirRequest       `json:"tar_dir,omitempty"`
	UntarDir     *UntarDirRequest     `json:"untar_dir,omitempty"`
	ExecStream   *ExecRequest         `json:"exec_stream,omitempty"`
}

// NotifyForkedRequest tells the guest a restore just happened so it can repair
// fork-shared state: reseed the kernel CRNG with fresh host entropy, step the
// wall clock back to host time, and signal userspace runtimes to reseed their
// own PRNGs. The host sends fresh values on every fork.
//
// Entropy and HostWallClockNanos are sensitive: Entropy is raw CRNG seed
// material and the clock can leak host timing. Neither value is ever logged by
// host or guest; only counts and applied-step magnitudes are logged.
type NotifyForkedRequest struct {
	Generation         uint64 `json:"generation"`
	HostWallClockNanos int64  `json:"host_wall_clock_nanos"`
	Entropy            []byte `json:"entropy"`
	// Network, when set, carries this fork's per-fork network identity. Every
	// fork restores the SAME snapshot (and thus the same baked guest IP), so
	// the host remaps the NIC to a distinct tap via snapshot/load
	// network_overrides and delivers the fork's distinct guest IP + gateway
	// here; the guest agent reconfigures eth0 (ip addr add, default route) on
	// receipt. Without this step every fork would share one guest IP and the
	// host could not route return traffic per fork. IPs and prefix length are
	// safe to log.
	Network *NotifyForkedNetwork `json:"network,omitempty"`
	// Volumes is the per-fork volume mount table. The host rebinds each baked
	// placeholder drive to this fork's backing (PATCH /drives) BEFORE sending
	// this notification, so the devices are in place; the guest then mounts each
	// entry's Device at MountPath. Empty (the default) means the fork has no
	// volumes and the guest mounts nothing. Device nodes and paths carry no
	// secrets and are safe to log.
	Volumes []VolumeMountEntry `json:"volumes,omitempty"`
}

// VolumeMountEntry is one volume the guest agent mounts after a restore. Device
// is the guest block device node (e.g. /dev/vdb) the host assigned by the drive
// attach order (rootfs is /dev/vda, the i-th volume drive is /dev/vd{b+i}).
// MountPath is where the guest mounts it, and ReadOnly attaches it MS_RDONLY so
// a read-only or shared volume cannot be written from the guest. All fields are
// config (no secrets) and safe to log.
type VolumeMountEntry struct {
	Device    string `json:"device"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only"`
}

// NotifyForkedNetwork is the per-fork eth0 configuration the guest agent
// applies after a restore: assign GuestIP/PrefixLen to eth0 and install a
// default route via GatewayIP (the host side of the per-sandbox /30). All
// fields are plain addresses and safe to log.
type NotifyForkedNetwork struct {
	GuestIP   string `json:"guest_ip"`
	GatewayIP string `json:"gateway_ip"`
	PrefixLen int    `json:"prefix_len"`
	// ResolverIP, when non-empty, is the node-wide DNS resolver the guest must
	// query for name-based egress. The guest agent writes it as the sole
	// nameserver in /etc/resolv.conf so every name lookup goes through the
	// controlled resolver (which is the only address the egress chain allows on
	// port 53). Empty means name-based egress is disabled and the guest's
	// existing resolv.conf is left untouched. The address is config, not a
	// secret, and is safe to log.
	ResolverIP string `json:"resolver_ip,omitempty"`
}

// ConfigureRequest delivers claim-time environment and secrets to the guest
// after restore. Values must never be logged or echoed by either side; they
// exist only in the request payload and the guest process environment.
type ConfigureRequest struct {
	Env     map[string]string `json:"env,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

type ExecRequest struct {
	Command    string            `json:"command"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`
}

type ReadFileRequest struct {
	Path string `json:"path"`
}

type WriteFileRequest struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}

type ListDirRequest struct {
	Path string `json:"path"`
}

type MkdirRequest struct {
	Path string `json:"path"`
}

type RemoveRequest struct {
	Path string `json:"path"`
}

// TarDirRequest asks the guest agent to tar a directory tree and return the tar
// bytes. Path is restricted by the guest to a workspace-transfer allowlist (only
// /workspace and paths under it); the guest never tars / or any secret/token
// path. The whole tar is buffered in the response, bounded by MaxTarBytes.
type TarDirRequest struct {
	Path string `json:"path"`
}

// UntarDirRequest asks the guest agent to extract a tar (produced by TarDir or by
// the host's CAS materialize -> tar path) into Path. The guest sanitizes every
// member name against traversal (no absolute paths, no ".." escape outside Path)
// before writing. The tar is bounded by MaxTarBytes.
type UntarDirRequest struct {
	Path string `json:"path"`
	Tar  []byte `json:"tar"`
}

type Response struct {
	OK           bool                  `json:"ok"`
	Error        string                `json:"error,omitempty"`
	Exec         *ExecResponse         `json:"exec,omitempty"`
	ReadFile     *ReadFileResponse     `json:"read_file,omitempty"`
	ListDir      *ListDirResponse      `json:"list_dir,omitempty"`
	Ping         *PingResponse         `json:"ping,omitempty"`
	NotifyForked *NotifyForkedResponse `json:"notify_forked,omitempty"`
	TarDir       *TarDirResponse       `json:"tar_dir,omitempty"`
}

// TarDirResponse carries the tar bytes of the requested directory. The tar is
// buffered whole, bounded by MaxTarBytes.
type TarDirResponse struct {
	Tar []byte `json:"tar"`
}

// NotifyForkedResponse reports what the guest did in response to a fork
// notification, for host-side observability. AppliedClockStepNanos is the
// signed adjustment applied to CLOCK_REALTIME (0 when drift was within
// tolerance), ReseededRNG is true when at least one entropy-injection path
// succeeded, and SignaledProcesses counts userspace processes that received
// the reseed signal.
type NotifyForkedResponse struct {
	AppliedClockStepNanos int64 `json:"applied_clock_step_nanos"`
	ReseededRNG           bool  `json:"reseeded_rng"`
	SignaledProcesses     int   `json:"signaled_processes"`
}

type ExecResponse struct {
	ExitCode   int     `json:"exit_code"`
	Stdout     string  `json:"stdout"`
	Stderr     string  `json:"stderr"`
	ExecTimeMs float64 `json:"exec_time_ms"`
}

// FrameKind tags an ExecStreamFrame as a data chunk or the terminal exit.
type FrameKind string

const (
	FrameChunk FrameKind = "chunk"
	FrameExit  FrameKind = "exit"
)

// StreamName identifies which standard stream a chunk came from.
type StreamName string

const (
	StreamStdout StreamName = "stdout"
	StreamStderr StreamName = "stderr"
)

// ExecStreamFrame is one newline-delimited JSON line in a streaming exec reply.
// The guest emits zero or more FrameChunk frames (each carrying a slice of one
// stream's bytes) followed by exactly one FrameExit frame. Data is a []byte so
// the JSON encoder base64s it: binary output survives and no embedded newline
// is mistaken for a frame boundary. The stream uses a dedicated vsock
// connection, so these multi-line replies never interleave with the shared
// connection's one-shot Response calls.
type ExecStreamFrame struct {
	Kind       FrameKind  `json:"kind"`
	Stream     StreamName `json:"stream,omitempty"`
	Data       []byte     `json:"data,omitempty"`
	ExitCode   int        `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	ExecTimeMs float64    `json:"exec_time_ms,omitempty"`
}

type ReadFileResponse struct {
	Content []byte `json:"content"`
	Size    int64  `json:"size"`
}

type FileEntry struct {
	Name       string `json:"name"`
	IsDir      bool   `json:"is_dir"`
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	ModifiedAt int64  `json:"modified_at"`
}

type ListDirResponse struct {
	Entries []FileEntry `json:"entries"`
}

type PingResponse struct {
	Uptime float64 `json:"uptime_seconds"`
}

const (
	// GuestCID is the vsock CID assigned to the guest by Firecracker.
	GuestCID = 3
	// AgentPort is the vsock port the guest agent listens on.
	AgentPort = 52
)
