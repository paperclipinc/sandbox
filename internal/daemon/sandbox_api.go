package daemon

import (
	"bytes"
	"context"
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

	"github.com/paperclipinc/mitos/internal/apierr"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// SandboxAPI exposes HTTP endpoints for exec/files on sandboxes managed by this forkd.
// The SDK and sandbox-server talk to this API to interact with running sandboxes.
type SandboxAPI struct {
	mu       sync.RWMutex
	agents   map[string]*vsock.Client // sandbox ID → agent connection
	tokens   map[string]string        // sandbox ID → bearer token; values never logged
	vsockDir string                   // directory containing vsock UDS files
	// streamPaths maps sandbox ID to the vsock UDS path used to open a DEDICATED
	// connection per streaming exec (so a long stream never interleaves with the
	// shared agents[id] connection). Guarded by mu.
	streamPaths map[string]string
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
	// singleSandbox, when set, switches the API into single-sandbox mode: it
	// serves exactly ONE sandbox, registered locally under singleSandboxID, and
	// the auth gate (requireBearer, ptyAuth) validates the presented bearer
	// against that one sandbox's token regardless of the request's "sandbox" id,
	// then routes the request to singleSandboxID. This is used ONLY by the
	// husk-stub, whose OnActivated hook registers its single VM under a fixed
	// local id while the SDK addresses the in-pod API with the claim's
	// status.sandboxID (the husk pod name), which never equals that local id.
	// forkd NEVER sets it, so forkd's multi-sandbox per-id token lookup is
	// byte-identical: a token for sandbox A still cannot authorize sandbox B.
	// Opt-in: see SetSingleSandbox.
	singleSandbox bool
	// singleSandboxID is the one local sandbox id served in single-sandbox mode.
	singleSandboxID string
	// auditor records a structured event after each exec/file op. Defaults to
	// NopAuditor (auditing off); set via SetAuditor. It only ever sees safe
	// summaries (command, path, byte count): never file content or secrets.
	auditor Auditor

	// maxStreams is the per-sandbox ceiling on concurrent OPEN streams
	// (production-blocker #2, cap 3). Each streaming exec, run_code, and PTY
	// holds a dedicated vsock connection plus host goroutines for the command
	// lifetime; without a cap a single tenant could open unbounded streams and
	// exhaust host vsock connections and goroutines. acquireStream enforces it at
	// stream OPEN (off the activate path); a NEW stream over the cap is rejected
	// with 429, existing streams are never killed. Zero or negative disables the
	// cap (unbounded, the prior behavior). Set via SetMaxStreamsPerSandbox.
	maxStreams int
	// openStreams counts the currently OPEN streams per sandbox id, guarded by
	// mu. acquireStream increments on open and the returned release decrements on
	// close, deleting the entry at zero so the map does not grow across sandbox
	// lifetimes.
	openStreams map[string]int
}

func NewSandboxAPI(vsockDir string) *SandboxAPI {
	return &SandboxAPI{
		agents:       make(map[string]*vsock.Client),
		tokens:       make(map[string]string),
		vsockDir:     vsockDir,
		streamPaths:  make(map[string]string),
		lastActivity: make(map[string]time.Time),
		now:          time.Now,
		auditor:      NopAuditor{},
		openStreams:  make(map[string]int),
	}
}

// SetMaxStreamsPerSandbox sets the per-sandbox ceiling on concurrent OPEN
// streams (streaming exec, run_code, PTY). A NEW stream opened while a sandbox
// is already at the cap is rejected with 429; existing streams are never
// killed. n<=0 disables the cap (unbounded). Must be called before the API
// serves requests; the field is not synchronized.
func (api *SandboxAPI) SetMaxStreamsPerSandbox(n int) {
	api.maxStreams = n
}

// acquireStream reserves one concurrent-stream slot for sandboxID, enforcing the
// per-sandbox cap (production-blocker #2, cap 3). It returns a release func and
// true when admitted; the caller MUST call release exactly once (defer) when the
// stream closes. It returns false when the sandbox is already at the cap, in
// which case the caller must reject the NEW stream and never call release. The
// cap is checked here, at stream OPEN, before the dedicated vsock connection is
// dialed; it is a single map lookup under mu and never touches the activate or
// fork hot path. maxStreams<=0 disables the cap.
func (api *SandboxAPI) acquireStream(sandboxID string) (release func(), ok bool) {
	if api.maxStreams <= 0 {
		return func() {}, true
	}
	api.mu.Lock()
	if api.openStreams[sandboxID] >= api.maxStreams {
		api.mu.Unlock()
		return nil, false
	}
	api.openStreams[sandboxID]++
	api.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			api.mu.Lock()
			if n := api.openStreams[sandboxID] - 1; n > 0 {
				api.openStreams[sandboxID] = n
			} else {
				delete(api.openStreams, sandboxID)
			}
			api.mu.Unlock()
		})
	}, true
}

// SetAuditor installs the auditor that records a structured event after each
// exec and file operation. Passing nil installs the NopAuditor (auditing off).
// Must be called before the API serves requests; the field is not synchronized.
func (api *SandboxAPI) SetAuditor(a Auditor) {
	if a == nil {
		a = NopAuditor{}
	}
	api.auditor = a
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

// SetSingleSandbox switches the API into single-sandbox mode for the husk-stub,
// which serves exactly ONE VM per pod. In this mode the auth gate (requireBearer
// and ptyAuth) validates the presented bearer against the single sandbox's
// registered token regardless of the request's "sandbox" id, then routes the
// request to id. This is required because the husk-stub registers its one VM
// under a fixed local id while the SDK addresses the in-pod API with the claim's
// status.sandboxID (the husk pod name), which never equals that local id; a
// strict per-id lookup would 401 every SDK request.
//
// The token gate is NOT weakened: a wrong or absent bearer is still rejected
// (401), the comparison stays constant-time, and a sandbox with no registered
// token still fails closed (unless AllowTokenless). forkd never calls this, so
// its multi-sandbox per-id token lookup is unchanged: a token for sandbox A
// cannot authorize sandbox B.
//
// Must be called before the API serves requests; the fields are not synchronized.
func (api *SandboxAPI) SetSingleSandbox(id string) {
	api.singleSandbox = true
	api.singleSandboxID = id
}

// resolveSandboxID maps the request's sandbox id to the id the API operates on.
// In single-sandbox mode every request resolves to the one served sandbox id,
// so an SDK that sends the husk pod name still reaches the single VM. In the
// default (forkd) multi-sandbox mode it returns the request id unchanged, so the
// per-id token lookup and agent routing are exactly as before.
func (api *SandboxAPI) resolveSandboxID(requested string) string {
	if api.singleSandbox {
		return api.singleSandboxID
	}
	return requested
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
	delete(api.streamPaths, sandboxID)
	api.mu.Unlock()
}

// RegisterStreamPath records the vsock UDS path for opening per-stream
// dedicated connections to a sandbox's guest agent. forkd calls this with the
// same path it passed to RegisterSandbox; the standalone server uses the unix
// fallback path. Without a recorded path, /v1/exec/stream falls back to the
// shared connection's aggregated Exec (no incremental output).
func (api *SandboxAPI) RegisterStreamPath(sandboxID, vsockPath string) {
	api.mu.Lock()
	api.streamPaths[sandboxID] = vsockPath
	api.mu.Unlock()
}

// dialStream opens a dedicated streaming connection for sandboxID, honoring the
// unix fallback the standalone server enables.
func (api *SandboxAPI) dialStream(sandboxID string) (*vsock.StreamConn, error) {
	api.mu.RLock()
	path, ok := api.streamPaths[sandboxID]
	api.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s has no stream path", sandboxID)
	}
	sc, err := vsock.DialStream(path, vsock.AgentPort)
	if err == nil {
		return sc, nil
	}
	if api.unixFallback && errors.Is(err, fs.ErrNotExist) {
		return vsock.DialStreamUnix(fmt.Sprintf("/tmp/sandbox-agent-%d.sock", vsock.AgentPort))
	}
	return nil, err
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
// reseed the kernel CRNG, step the wall clock, and signal userspace. When
// guestNet is non-nil it also carries this fork's distinct eth0 address +
// gateway so the guest re-addresses its NIC (every fork restores the same
// snapshot-baked guest IP). When volumes is non-empty it carries the per-fork
// volume mount table (device, mount path, read-only) the guest mounts after the
// host rebound the drives. Entropy is sensitive seed material and is never
// logged; the network addresses, device nodes, and paths are safe to log.
//
// It RETURNS the guest's NotifyForkedResponse so the caller can enforce the
// fork-correctness gate (ReseededRNG): a transport success alone does not mean
// the guest reseeded its CRNG. The response carries booleans and counts only,
// never entropy bytes.
func (api *SandboxAPI) NotifyForked(sandboxID string, generation uint64, entropy []byte, guestNet *vsock.NotifyForkedNetwork, volumes []vsock.VolumeMountEntry) (*vsock.NotifyForkedResponse, error) {
	agent, err := api.getAgent(sandboxID)
	if err != nil {
		return nil, err
	}
	return agent.NotifyForkedWithConfig(generation, entropy, guestNet, volumes)
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
	mux.HandleFunc("POST /v1/exec/stream", api.handleExecStream)
	mux.HandleFunc("POST /v1/run_code/stream", api.handleRunCodeStream)
	mux.HandleFunc("POST /v1/files/read", api.handleReadFile)
	mux.HandleFunc("POST /v1/files/write", api.handleWriteFile)
	mux.HandleFunc("POST /v1/files/list", api.handleListDir)
	mux.HandleFunc("POST /v1/files/mkdir", api.handleMkdir)
	mux.HandleFunc("POST /v1/files/remove", api.handleRemove)

	// The PTY WebSocket upgrade is a bodyless GET, so it cannot go through the
	// body-peeking requireBearer middleware; it authenticates itself in
	// handlePty (ptyAuth: ?sandbox= + Authorization: Bearer). It is mounted on
	// a separate outer mux that is NOT wrapped.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /v1/pty", api.handlePty)
	outer.Handle("/", api.requireBearer(mux))
	return outer
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

		// In single-sandbox mode (husk-stub) the request id is whatever the SDK
		// sent (the husk pod name); resolve it to the one served sandbox id so
		// the token lookup hits the single registered token. In forkd's default
		// multi-sandbox mode this is the request id unchanged, so the per-id gate
		// is byte-identical.
		sandboxID := api.resolveSandboxID(peek.Sandbox)

		api.mu.RLock()
		token, hasToken := api.tokens[sandboxID]
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

		// Single-sandbox mode: the downstream handlers route the agent and stream
		// by the body's "sandbox" field, but the SDK sent the pod name, which is
		// not the local id the VM is registered under. Rewrite the body so the
		// handlers reach the single VM. This rewrite ONLY happens in single-
		// sandbox mode; in forkd's multi-sandbox mode the body is untouched.
		if api.singleSandbox && peek.Sandbox != sandboxID {
			if rewritten, err := rewriteSandboxField(body, sandboxID); err == nil {
				body = rewritten
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			} else {
				writeErr(w, "invalid json", 400)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// rewriteSandboxField returns body with its top-level "sandbox" field set to
// id, preserving every other field. Used only in single-sandbox mode to route
// the SDK's request (which carries the husk pod name) to the one local sandbox
// id the VM is registered under. The body was already buffered and size-bounded
// by requireBearer before this is called.
func rewriteSandboxField(body []byte, id string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("rewrite sandbox field: %w", err)
	}
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("rewrite sandbox field: %w", err)
	}
	if m == nil {
		m = make(map[string]json.RawMessage, 1)
	}
	m["sandbox"] = idJSON
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("rewrite sandbox field: %w", err)
	}
	return out, nil
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

	if _, err := api.getAgent(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	var out, errb strings.Builder
	exit, err := api.runExecStream(r.Context(), req, func(stream vsock.StreamName, data []byte) error {
		if stream == vsock.StreamStdout {
			out.Write(data)
		} else {
			errb.Write(data)
		}
		return nil
	})
	if err != nil {
		writeAPIErr(w, apierr.Catalogue["exec_failed"].WithCause(fmt.Sprintf("exec failed: %v", err)))
		return
	}

	result := &vsock.ExecResponse{
		ExitCode:   exit.ExitCode,
		Stdout:     out.String(),
		Stderr:     errb.String(),
		ExecTimeMs: exit.ExecTimeMs,
	}

	// The command is safe to record (commands are not secret values); it is
	// truncated to a bound. The exit code rides in Detail. OK reports that the
	// call was served, not the exit code.
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "exec",
		Detail:    fmt.Sprintf("exit=%d cmd=%s", result.ExitCode, truncateCommand(req.Command)),
		OK:        true,
	})

	writeJSON(w, result)
}

// runExecStream opens a dedicated stream connection and drives one exec,
// invoking onChunk per chunk and returning the terminal frame. It falls back to
// the shared connection's aggregated Exec when no stream path is registered so
// callers still work on hosts that have not wired streaming.
func (api *SandboxAPI) runExecStream(ctx context.Context, req execRequest, onChunk vsock.ChunkFunc) (*vsock.ExecStreamFrame, error) {
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30
	}
	sc, err := api.dialStream(req.Sandbox)
	if err != nil {
		// Fallback: aggregate via the shared connection (no incremental output).
		agent, gerr := api.getAgent(req.Sandbox)
		if gerr != nil {
			return nil, gerr
		}
		resp, eerr := agent.Exec(req.Command, req.WorkingDir, req.Env, timeout)
		if eerr != nil {
			return nil, eerr
		}
		_ = onChunk(vsock.StreamStdout, []byte(resp.Stdout))
		_ = onChunk(vsock.StreamStderr, []byte(resp.Stderr))
		return &vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: resp.ExitCode, ExecTimeMs: resp.ExecTimeMs}, nil
	}
	defer sc.Close()
	return sc.ExecStream(ctx, &vsock.ExecRequest{
		Command:    req.Command,
		WorkingDir: req.WorkingDir,
		Env:        req.Env,
		Timeout:    timeout,
	}, onChunk)
}

func (api *SandboxAPI) handleExecStream(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	if _, err := api.getAgent(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	// Per-sandbox concurrent-stream cap (production-blocker #2, cap 3): reject a
	// NEW stream when the sandbox is already at the cap, BEFORE writing the 200
	// header or dialing the dedicated vsock connection. Existing streams are
	// never touched. Checked at OPEN, off the activate path.
	release, ok := api.acquireStream(req.Sandbox)
	if !ok {
		writeAPIErr(w, apierr.Catalogue["too_many_streams"].WithCause(fmt.Sprintf("sandbox %s is at its concurrent-stream limit", req.Sandbox)))
		return
	}
	defer release()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)

	writeLine := func(v any) error {
		if err := enc.Encode(v); err != nil {
			return err
		}
		return rc.Flush()
	}

	exit, err := api.runExecStream(r.Context(), req, func(stream vsock.StreamName, data []byte) error {
		return writeLine(map[string]any{"stream": string(stream), "data": data})
	})
	if err != nil {
		// The stream has already started; emit a terminal error frame rather
		// than an HTTP status (status was sent 200 with the first byte). The
		// message carries actionable text and never echoes secrets.
		_ = writeLine(map[string]any{"exit_code": 1, "error": fmt.Sprintf("exec stream failed: %v", err)})
		return
	}
	_ = writeLine(map[string]any{"exit_code": exit.ExitCode, "exec_time_ms": exit.ExecTimeMs, "error": exit.Error})

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "exec_stream",
		Detail:    fmt.Sprintf("exit=%d cmd=%s", exit.ExitCode, truncateCommand(req.Command)),
		OK:        true,
	})
}

type runCodeRequest struct {
	Sandbox  string `json:"sandbox"`
	Code     string `json:"code"`
	Language string `json:"language,omitempty"`
	Timeout  int    `json:"timeout,omitempty"`
}

// runRunCodeStream opens a dedicated stream connection and drives one run_code
// against the guest kernel, invoking onFrame per ExecStreamFrame. Unlike exec,
// there is no aggregated fallback: run_code requires the streaming path (a
// registered stream UDS), since the kernel reply is a frame stream.
func (api *SandboxAPI) runRunCodeStream(ctx context.Context, req runCodeRequest, onFrame func(vsock.ExecStreamFrame)) error {
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 60
	}
	sc, err := api.dialStream(req.Sandbox)
	if err != nil {
		return err
	}
	defer sc.Close()
	return sc.RunCode(ctx, &vsock.RunCodeRequest{
		Code:     req.Code,
		Language: req.Language,
		Timeout:  timeout,
	}, onFrame)
}

// handleRunCodeStream streams a run_code execution back as chunked NDJSON. Each
// guest ExecStreamFrame is re-encoded with an explicit "kind" field so the SDKs
// can distinguish stdout/stderr/result/error/exit frames; this is a distinct
// wire shape from /v1/exec/stream (which uses keyless chunk/exit maps) because
// run_code carries rich result and structured-error payloads exec does not.
// Result/error payloads are tenant code output and are never logged.
func (api *SandboxAPI) handleRunCodeStream(w http.ResponseWriter, r *http.Request) {
	var req runCodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	if _, err := api.getAgent(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	// Per-sandbox concurrent-stream cap (production-blocker #2, cap 3): a run_code
	// stream holds a dedicated vsock connection for the command lifetime, so it
	// counts against the same per-sandbox ceiling. Reject a NEW one over the cap
	// before writing the 200 header; existing streams are never touched.
	release, ok := api.acquireStream(req.Sandbox)
	if !ok {
		writeAPIErr(w, apierr.Catalogue["too_many_streams"].WithCause(fmt.Sprintf("sandbox %s is at its concurrent-stream limit", req.Sandbox)))
		return
	}
	defer release()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)

	writeLine := func(v any) {
		if err := enc.Encode(v); err != nil {
			return
		}
		_ = rc.Flush()
	}

	var lastExit int
	err := api.runRunCodeStream(r.Context(), req, func(fr vsock.ExecStreamFrame) {
		switch fr.Kind {
		case vsock.FrameChunk:
			if fr.Stream == vsock.StreamStderr {
				writeLine(map[string]any{"kind": "stderr", "stderr": fr.Data})
			} else {
				writeLine(map[string]any{"kind": "stdout", "stdout": fr.Data})
			}
		case vsock.FrameResult:
			writeLine(map[string]any{"kind": "result", "result": fr.Result})
		case vsock.FrameError:
			writeLine(map[string]any{"kind": "error", "error": fr.ErrorInfo})
		case vsock.FrameExit:
			lastExit = fr.ExitCode
			writeLine(map[string]any{"kind": "exit", "exit_code": fr.ExitCode})
		}
	})
	if err != nil {
		// The stream has already started (200 sent); surface the failure as a
		// final error frame rather than an HTTP status. The message carries
		// actionable text and never echoes secrets.
		writeLine(map[string]any{"kind": "error", "error": map[string]any{"name": "KernelStreamError", "value": fmt.Sprintf("run_code stream failed: %v", err)}})
		writeLine(map[string]any{"kind": "exit", "exit_code": 1})
		lastExit = 1
	}

	// The code is safe to record (not a secret value), truncated to a bound.
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "run_code",
		Detail:    fmt.Sprintf("exit=%d code=%s", lastExit, truncateCommand(req.Code)),
		OK:        true,
	})
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
		writeAPIErr(w, apierr.Catalogue["file_failed"].WithCause(err.Error()))
		return
	}

	// Record the path and byte count only; the content is never audited.
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "read",
		Detail:    req.Path,
		Bytes:     len(content),
		OK:        true,
	})

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
		writeAPIErr(w, apierr.Catalogue["file_failed"].WithCause(err.Error()))
		return
	}

	// Record the path and byte count only; the content is never audited.
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "write",
		Detail:    req.Path,
		Bytes:     len(req.Content),
		OK:        true,
	})

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
		writeAPIErr(w, apierr.Catalogue["file_failed"].WithCause(err.Error()))
		return
	}

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "list",
		Detail:    req.Path,
		OK:        true,
	})

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
		writeAPIErr(w, apierr.Catalogue["file_failed"].WithCause(err.Error()))
		return
	}

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "mkdir",
		Detail:    req.Path,
		OK:        true,
	})

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
		writeAPIErr(w, apierr.Catalogue["file_failed"].WithCause(err.Error()))
		return
	}

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "remove",
		Detail:    req.Path,
		OK:        true,
	})

	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeAPIErr writes an LLM-legible error envelope. Prefer this over writeErr at
// new call sites: it carries a stable code and an actionable remediation.
func writeAPIErr(w http.ResponseWriter, e apierr.Error) {
	apierr.Encode(w, e)
}

// writeErr is the legacy shim. It maps a status to the closest catalogue entry
// and uses msg as the cause, so every existing call site now emits the
// {error:{code,message,cause,remediation}} envelope without a per-site rewrite.
// The cause is built from sandbox ids, paths, and operation names only and never
// carries a secret value.
func writeErr(w http.ResponseWriter, msg string, code int) {
	writeAPIErr(w, codeForStatus(code).WithCause(msg))
}

// codeForStatus picks the catalogue entry for an HTTP status used by the legacy
// writeErr call sites.
func codeForStatus(status int) apierr.Error {
	switch status {
	case http.StatusBadRequest:
		return apierr.Catalogue["invalid_json"]
	case http.StatusRequestEntityTooLarge:
		return apierr.Catalogue["body_too_large"]
	case http.StatusUnauthorized:
		return apierr.Catalogue["unauthorized"]
	case http.StatusNotFound:
		return apierr.Catalogue["not_found"]
	default:
		return apierr.Catalogue["internal"]
	}
}
