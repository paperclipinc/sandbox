package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/paperclipinc/sandbox/internal/vsock"
)

// SandboxAPI exposes HTTP endpoints for exec/files on sandboxes managed by this forkd.
// The SDK and sandbox-server talk to this API to interact with running sandboxes.
type SandboxAPI struct {
	mu       sync.RWMutex
	agents   map[string]*vsock.Client // sandbox ID → agent connection
	vsockDir string                   // directory containing vsock UDS files
}

func NewSandboxAPI(vsockDir string) *SandboxAPI {
	return &SandboxAPI{
		agents:   make(map[string]*vsock.Client),
		vsockDir: vsockDir,
	}
}

// RegisterSandbox connects to a sandbox's guest agent.
func (api *SandboxAPI) RegisterSandbox(sandboxID, vsockPath string) error {
	client, err := vsock.Connect(vsockPath, vsock.AgentPort)
	if err != nil {
		// Fallback: try Unix socket (for mock/local testing)
		sockPath := fmt.Sprintf("/tmp/sandbox-agent-%d.sock", vsock.AgentPort)
		client, err = vsock.ConnectUnix(sockPath)
		if err != nil {
			return fmt.Errorf("connect to agent for sandbox %s: %w", sandboxID, err)
		}
	}

	api.mu.Lock()
	api.agents[sandboxID] = client
	api.mu.Unlock()
	return nil
}

// UnregisterSandbox closes the agent connection.
func (api *SandboxAPI) UnregisterSandbox(sandboxID string) {
	api.mu.Lock()
	if client, ok := api.agents[sandboxID]; ok {
		client.Close()
		delete(api.agents, sandboxID)
	}
	api.mu.Unlock()
}

func (api *SandboxAPI) getAgent(sandboxID string) (*vsock.Client, error) {
	api.mu.RLock()
	client, ok := api.agents[sandboxID]
	api.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found or agent not connected", sandboxID)
	}
	return client, nil
}

// Handler returns an http.Handler for the sandbox exec/files API.
func (api *SandboxAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/exec", api.handleExec)
	mux.HandleFunc("POST /v1/files/read", api.handleReadFile)
	mux.HandleFunc("POST /v1/files/write", api.handleWriteFile)
	mux.HandleFunc("POST /v1/files/list", api.handleListDir)
	mux.HandleFunc("POST /v1/files/mkdir", api.handleMkdir)
	mux.HandleFunc("POST /v1/files/remove", api.handleRemove)
	return mux
}

type execRequest struct {
	Sandbox    string            `json:"sandbox"`
	Command    string            `json:"command"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`
}

func (api *SandboxAPI) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}

	agent, err := api.getAgent(req.Sandbox)
	if err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30
	}

	result, err := agent.Exec(req.Command, req.WorkingDir, req.Env, timeout)
	if err != nil {
		writeErr(w, fmt.Sprintf("exec failed: %v", err), 500)
		return
	}

	writeJSON(w, result)
}

type filePathRequest struct {
	Sandbox string `json:"sandbox"`
	Path    string `json:"path"`
}

type fileWriteRequest struct {
	Sandbox string `json:"sandbox"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}

func (api *SandboxAPI) handleReadFile(w http.ResponseWriter, r *http.Request) {
	var req filePathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}

	agent, err := api.getAgent(req.Sandbox)
	if err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	content, err := agent.ReadFile(req.Path)
	if err != nil {
		writeErr(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"content": string(content), "size": len(content)})
}

func (api *SandboxAPI) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	var req fileWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}

	agent, err := api.getAgent(req.Sandbox)
	if err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	mode := req.Mode
	if mode == 0 {
		mode = 0644
	}

	if err := agent.WriteFile(req.Path, []byte(req.Content), mode); err != nil {
		writeErr(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

func (api *SandboxAPI) handleListDir(w http.ResponseWriter, r *http.Request) {
	var req filePathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}

	agent, err := api.getAgent(req.Sandbox)
	if err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	entries, err := agent.ListDir(req.Path)
	if err != nil {
		writeErr(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"entries": entries})
}

func (api *SandboxAPI) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req filePathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}

	agent, err := api.getAgent(req.Sandbox)
	if err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	if err := agent.Mkdir(req.Path); err != nil {
		writeErr(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

func (api *SandboxAPI) handleRemove(w http.ResponseWriter, r *http.Request) {
	var req filePathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}

	agent, err := api.getAgent(req.Sandbox)
	if err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	if err := agent.Remove(req.Path); err != nil {
		writeErr(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
