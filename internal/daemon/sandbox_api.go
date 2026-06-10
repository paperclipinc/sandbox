package daemon

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/paperclipinc/sandbox/internal/vsock"
)

// SandboxAPI exposes HTTP endpoints for exec/files on sandboxes managed by this forkd.
// The SDK and sandbox-server talk to this API to interact with running sandboxes.
type SandboxAPI struct {
	mu       sync.RWMutex
	agents   map[string]*vsock.Client // sandbox ID → agent connection
	tokens   map[string]string        // sandbox ID → bearer token; values never logged
	vsockDir string                   // directory containing vsock UDS files
	// lastActivity records the time of the most recent exec or file call per
	// sandbox, guarded by mu. Absent until the first touch; used by the GC
	// reconciler via ListSandboxes to drive idle reaping.
	lastActivity map[string]time.Time
	// now is the clock used to stamp lastActivity. Defaults to time.Now;
	// tests override it for determinism.
	now func() time.Time
	// unixFallback allows RegisterSandbox to fall back to the agent's fixed
	// local unix socket. Opt-in: see EnableUnixFallback.
	unixFallback bool
	// allowTokenless permits requests for sandboxes that have NO registered
	// token. Opt-in: see AllowTokenless.
	allowTokenless bool
}

func NewSandboxAPI(vsockDir string) *SandboxAPI {
	return &SandboxAPI{
		agents:       make(map[string]*vsock.Client),
		tokens:       make(map[string]string),
		vsockDir:     vsockDir,
		lastActivity: make(map[string]time.Time),
		now:          time.Now,
	}
}

// touch stamps the current time as the sandbox's last activity. Called at the
// top of every exec and file handler.
func (api *SandboxAPI) touch(sandboxID string) {
	api.mu.Lock()
	api.lastActivity[sandboxID] = api.now()
	api.mu.Unlock()
}

// RecordActivity stamps t as the sandbox's last-activity time, overriding the
// clock-based touch. It exists so callers (and tests of the GC reconciler) can
// set a known last-activity for a sandbox id; forkd itself relies on the
// implicit touch from exec and file handlers.
func (api *SandboxAPI) RecordActivity(sandboxID string, t time.Time) {
	api.mu.Lock()
	api.lastActivity[sandboxID] = t
	api.mu.Unlock()
}

// LastActivity returns the time of the most recent exec or file call on the
// sandbox. The bool is false when the sandbox has never been accessed.
func (api *SandboxAPI) LastActivity(sandboxID string) (time.Time, bool) {
	api.mu.RLock()
	t, ok := api.lastActivity[sandboxID]
	api.mu.RUnlock()
	return t, ok
}

// AllowTokenless permits requests targeting sandboxes that have no
// registered bearer token. Used ONLY by the standalone sandbox-server
// (which has no token-minting control plane) and by unit tests of other
// layers. forkd never sets it: a forkd sandbox without a token fails
// closed with 401. Sandboxes WITH a registered token are always enforced,
// even under AllowTokenless.
//
// Must be called before the API serves requests; the flag is not synchronized.
func (api *SandboxAPI) AllowTokenless() {
	api.allowTokenless = true
}

// RegisterToken registers the bearer token required on every HTTP request
// targeting sandboxID. An empty token is a no-op: the sandbox stays
// tokenless and fails closed (unless AllowTokenless). Token values are
// never logged.
func (api *SandboxAPI) RegisterToken(sandboxID, token string) {
	if token == "" {
		return
	}
	api.mu.Lock()
	api.tokens[sandboxID] = token
	api.mu.Unlock()
}

// EnableUnixFallback lets RegisterSandbox fall back to the guest agent's
// fixed local unix socket (/tmp/sandbox-agent-<port>.sock) when the vsock
// UDS path does not exist. This supports the standalone sandbox-server's
// local-testing workflow (agent running on the host, no Firecracker).
//
// forkd deliberately does NOT enable this: its vsock paths come from the
// fork engine, and a fallback to a global socket could deliver claim-time
// secrets to an unrelated local process.
//
// Must be called before the API serves requests; the flag is not synchronized.
func (api *SandboxAPI) EnableUnixFallback() {
	api.unixFallback = true
}

// RegisterSandbox connects to a sandbox's guest agent.
func (api *SandboxAPI) RegisterSandbox(sandboxID, vsockPath string) error {
	client, err := vsock.Connect(vsockPath, vsock.AgentPort)
	if err != nil {
		// Fallback to the agent's local unix socket only when explicitly
		// enabled (standalone sandbox-server) AND the vsock UDS path does not
		// exist (no Firecracker VM behind it). Never on other dial failures:
		// a half-up VM must surface as an error, not silently connect to a
		// stray local agent.
		if !api.unixFallback || !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("connect to agent for sandbox %s: %w", sandboxID, err)
		}
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

// UnregisterSandbox closes the agent connection and clears the sandbox's
// bearer token.
func (api *SandboxAPI) UnregisterSandbox(sandboxID string) {
	api.mu.Lock()
	if client, ok := api.agents[sandboxID]; ok {
		client.Close()
		delete(api.agents, sandboxID)
	}
	delete(api.tokens, sandboxID)
	delete(api.lastActivity, sandboxID)
	api.mu.Unlock()
}

// Configure delivers claim-time env and secrets to a sandbox's guest agent.
// Values are never logged.
func (api *SandboxAPI) Configure(sandboxID string, env, secrets map[string]string) error {
	agent, err := api.getAgent(sandboxID)
	if err != nil {
		return err
	}
	return agent.Configure(env, secrets)
}

// NotifyForked tells a sandbox's guest agent a restore just happened so it can
// reseed the kernel CRNG, step the wall clock, and signal userspace. Entropy
// is sensitive seed material and is never logged.
func (api *SandboxAPI) NotifyForked(sandboxID string, generation uint64, entropy []byte) error {
	agent, err := api.getAgent(sandboxID)
	if err != nil {
		return err
	}
	_, err = agent.NotifyForked(generation, entropy)
	return err
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

// Handler returns an http.Handler for the sandbox exec/files API. Every
// route is wrapped in the per-sandbox bearer-token middleware.
func (api *SandboxAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/exec", api.handleExec)
	mux.HandleFunc("POST /v1/files/read", api.handleReadFile)
	mux.HandleFunc("POST /v1/files/write", api.handleWriteFile)
	mux.HandleFunc("POST /v1/files/list", api.handleListDir)
	mux.HandleFunc("POST /v1/files/mkdir", api.handleMkdir)
	mux.HandleFunc("POST /v1/files/remove", api.handleRemove)
	return api.requireBearer(mux)
}

// maxAuthBodyBytes bounds how much request body the auth middleware buffers.
// File writes are hex-encoded JSON, so this is the effective request cap.
const maxAuthBodyBytes = 32 << 20 // 32 MiB

// requireBearer enforces per-sandbox bearer tokens. The body is read and
// buffered ONCE: the middleware peeks the JSON "sandbox" field, checks
// Authorization: Bearer against the registered token in constant time, and
// hands the buffered body to the real handler. Failure modes:
//   - no token registered for the sandbox: 401 (fail closed), unless
//     AllowTokenless was set (standalone sandbox-server only)
//   - missing or malformed Authorization header: 401
//   - token mismatch: 401
//
// Token values never appear in responses or logs.
func (api *SandboxAPI) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxAuthBodyBytes+1))
		if err != nil {
			writeErr(w, "read request body", 400)
			return
		}
		if len(body) > maxAuthBodyBytes {
			writeErr(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		var peek struct {
			Sandbox string `json:"sandbox"`
		}
		if err := json.Unmarshal(body, &peek); err != nil {
			writeErr(w, "invalid json", 400)
			return
		}

		api.mu.RLock()
		token, hasToken := api.tokens[peek.Sandbox]
		api.mu.RUnlock()

		if !hasToken {
			if api.allowTokenless {
				next.ServeHTTP(w, r)
				return
			}
			writeErr(w, "unauthorized: no token registered for sandbox", 401)
			return
		}

		auth := r.Header.Get("Authorization")
		presented, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			writeErr(w, "unauthorized: bearer token required", 401)
			return
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			writeErr(w, "unauthorized: invalid token", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	api.touch(req.Sandbox)

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
	api.touch(req.Sandbox)

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
	api.touch(req.Sandbox)

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
	api.touch(req.Sandbox)

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
	api.touch(req.Sandbox)

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
	api.touch(req.Sandbox)

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
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
