package vsock

// Protocol for host ↔ guest agent communication over vsock.
// JSON-encoded messages, newline-delimited.

type RequestType string

const (
	TypeExec      RequestType = "exec"
	TypeReadFile  RequestType = "read_file"
	TypeWriteFile RequestType = "write_file"
	TypeListDir   RequestType = "list_dir"
	TypeMkdir     RequestType = "mkdir"
	TypeRemove    RequestType = "remove"
	TypePing      RequestType = "ping"
	TypeConfigure RequestType = "configure"
)

type Request struct {
	Type      RequestType       `json:"type"`
	Exec      *ExecRequest      `json:"exec,omitempty"`
	ReadFile  *ReadFileRequest  `json:"read_file,omitempty"`
	WriteFile *WriteFileRequest `json:"write_file,omitempty"`
	ListDir   *ListDirRequest   `json:"list_dir,omitempty"`
	Mkdir     *MkdirRequest     `json:"mkdir,omitempty"`
	Remove    *RemoveRequest    `json:"remove,omitempty"`
	Configure *ConfigureRequest `json:"configure,omitempty"`
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
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Exec     *ExecResponse     `json:"exec,omitempty"`
	ReadFile *ReadFileResponse `json:"read_file,omitempty"`
	ListDir  *ListDirResponse  `json:"list_dir,omitempty"`
	Ping     *PingResponse     `json:"ping,omitempty"`
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
