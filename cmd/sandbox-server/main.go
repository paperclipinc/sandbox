package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/paperclipinc/sandbox/internal/daemon"
	"github.com/paperclipinc/sandbox/internal/firecracker"
)

// sandbox-server is a standalone REST API. No Kubernetes required.
// For production on k8s, use the controller + forkd instead.
// Both share the same fork engine and guest agent protocol.

type server struct {
	mu          sync.RWMutex
	templateMgr *firecracker.TemplateManager
	rootfsPath  string
	templates   map[string]*templateInfo
	sandboxes   map[string]*sandboxInfo
	mockMode    bool
	sandboxAPI  *daemon.SandboxAPI
}

type templateInfo struct {
	ID        string    `json:"id"`
	Ready     bool      `json:"ready"`
	CreatedAt time.Time `json:"created_at"`
	TimeMs    float64   `json:"creation_time_ms"`
}

type sandboxInfo struct {
	ID         string    `json:"id"`
	TemplateID string    `json:"template_id"`
	Endpoint   string    `json:"endpoint"`
	CreatedAt  time.Time `json:"created_at"`
	ForkTimeMs float64   `json:"fork_time_ms"`
}

func main() {
	var (
		addr           string
		dataDir        string
		firecrackerBin string
		kernelPath     string
		rootfsPath     string
		mockMode       bool
	)

	flag.StringVar(&addr, "addr", ":8080", "Listen address")
	flag.StringVar(&dataDir, "data-dir", "/tmp/sandbox-server", "Data directory")
	flag.StringVar(&firecrackerBin, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	flag.StringVar(&kernelPath, "kernel", "", "Guest kernel path (required unless --mock)")
	flag.StringVar(&rootfsPath, "rootfs", "", "Guest rootfs path (required unless --mock)")
	flag.BoolVar(&mockMode, "mock", false, "Mock mode (no KVM, simulated responses)")
	flag.Parse()

	if !mockMode && (kernelPath == "" || rootfsPath == "") {
		fmt.Fprintln(os.Stderr, "error: --kernel and --rootfs are required (or use --mock)")
		os.Exit(1)
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error: create data dir %s: %v\n", dataDir, err)
		os.Exit(1)
	}

	s := &server{
		rootfsPath: rootfsPath,
		templates:  make(map[string]*templateInfo),
		sandboxes:  make(map[string]*sandboxInfo),
		mockMode:   mockMode,
		sandboxAPI: daemon.NewSandboxAPI(dataDir),
	}
	// Standalone local-testing path: if the Firecracker vsock UDS does not
	// exist, fall back to a guest agent running directly on the host
	// (/tmp/sandbox-agent-52.sock). forkd does not opt in to this fallback.
	s.sandboxAPI.EnableUnixFallback()
	// Standalone mode has no token-minting control plane; its sandboxes are
	// tokenless by design. forkd never sets this: there, a sandbox without
	// a registered token fails closed with 401.
	s.sandboxAPI.AllowTokenless()

	if !mockMode {
		s.templateMgr = firecracker.NewTemplateManager(firecrackerBin, kernelPath, dataDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/templates", s.handleCreateTemplate)
	mux.HandleFunc("GET /v1/templates", s.handleListTemplates)
	mux.HandleFunc("POST /v1/fork", s.handleFork)
	mux.HandleFunc("GET /v1/sandboxes", s.handleListSandboxes)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.handleTerminate)

	// Exec and files go through SandboxAPI → vsock → guest agent
	apiHandler := s.sandboxAPI.Handler()
	mux.Handle("POST /v1/exec", apiHandler)
	mux.Handle("POST /v1/files/", apiHandler)

	log.Printf("sandbox-server listening on %s (mock=%v)", addr, mockMode)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp(w, map[string]any{
		"status": "ok", "mock": s.mockMode,
		"templates": len(s.templates), "sandboxes": len(s.sandboxes),
	})
}

type createTemplateReq struct {
	ID           string `json:"id"`
	InitWaitSecs int    `json:"init_wait_seconds"`
}

func (s *server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req createTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, "invalid json", 400)
		return
	}
	if req.ID == "" {
		errResp(w, "id is required", 400)
		return
	}
	if req.InitWaitSecs == 0 {
		req.InitWaitSecs = 5
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(100 * time.Millisecond)
	} else {
		cfg := firecracker.DefaultVMConfig()
		cfg.RootfsPath = s.rootfsPath
		if _, err := s.templateMgr.CreateTemplate(req.ID, cfg, req.InitWaitSecs); err != nil {
			errResp(w, fmt.Sprintf("create template: %v", err), 500)
			return
		}
	}

	info := &templateInfo{
		ID: req.ID, Ready: true, CreatedAt: time.Now(),
		TimeMs: float64(time.Since(start).Milliseconds()),
	}

	s.mu.Lock()
	s.templates[req.ID] = info
	s.mu.Unlock()

	log.Printf("template %q created in %.0fms", req.ID, info.TimeMs)
	resp(w, info)
}

func (s *server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	templates := make([]*templateInfo, 0, len(s.templates))
	for _, t := range s.templates {
		templates = append(templates, t)
	}
	resp(w, templates)
}

type forkReq struct {
	Template string `json:"template"`
	ID       string `json:"id"`
}

func (s *server) handleFork(w http.ResponseWriter, r *http.Request) {
	var req forkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, "invalid json", 400)
		return
	}

	s.mu.RLock()
	_, ok := s.templates[req.Template]
	s.mu.RUnlock()
	if !ok {
		errResp(w, fmt.Sprintf("template %q not found", req.Template), 404)
		return
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(800 * time.Microsecond)
	}

	info := &sandboxInfo{
		ID: req.ID, TemplateID: req.Template,
		Endpoint: "http://localhost:8080", CreatedAt: time.Now(),
		ForkTimeMs: float64(time.Since(start).Microseconds()) / 1000.0,
	}

	s.mu.Lock()
	s.sandboxes[req.ID] = info
	s.mu.Unlock()

	// In real mode, register the vsock connection for exec/files
	if !s.mockMode {
		vsockPath := fmt.Sprintf("/tmp/sandbox-server/sandboxes/%s/vsock.sock", req.ID)
		if err := s.sandboxAPI.RegisterSandbox(req.ID, vsockPath); err != nil {
			// Non-fatal: the sandbox exists but exec/files won't work until the agent is reachable.
			log.Printf("register agent for sandbox %q: %v", req.ID, err)
		}
	}

	log.Printf("fork %q from %q in %.2fms", req.ID, req.Template, info.ForkTimeMs)
	resp(w, info)
}

func (s *server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sandboxes := make([]*sandboxInfo, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		sandboxes = append(sandboxes, sb)
	}
	resp(w, sandboxes)
}

func (s *server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, ok := s.sandboxes[id]
	if ok {
		delete(s.sandboxes, id)
	}
	s.mu.Unlock()

	if !ok {
		errResp(w, fmt.Sprintf("sandbox %q not found", id), 404)
		return
	}

	s.sandboxAPI.UnregisterSandbox(id)
	log.Printf("terminated sandbox %q", id)
	resp(w, map[string]string{"status": "terminated", "id": id})
}

func resp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func errResp(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
