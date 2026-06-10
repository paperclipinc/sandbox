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
)

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

type Response struct {
	OK           bool                  `json:"ok"`
	Error        string                `json:"error,omitempty"`
	Exec         *ExecResponse         `json:"exec,omitempty"`
	ReadFile     *ReadFileResponse     `json:"read_file,omitempty"`
	ListDir      *ListDirResponse      `json:"list_dir,omitempty"`
	Ping         *PingResponse         `json:"ping,omitempty"`
	NotifyForked *NotifyForkedResponse `json:"notify_forked,omitempty"`
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
