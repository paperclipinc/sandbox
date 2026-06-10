package daemon

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/paperclipinc/sandbox/internal/fork"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
)

var (
	forkDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agentrun_fork_duration_seconds",
		Help:    "Time to fork a sandbox from snapshot",
		Buckets: []float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1},
	})
	activeSandboxes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentrun_active_sandboxes",
		Help: "Number of currently running sandboxes on this node",
	})
	memoryShared = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentrun_memory_shared_bytes",
		Help: "CoW shared memory across forks",
	})
	memoryUnique = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentrun_memory_unique_bytes",
		Help: "Per-fork unique memory (dirty pages)",
	})
)

func init() {
	prometheus.MustRegister(forkDuration, activeSandboxes, memoryShared, memoryUnique)
}

type Server struct {
	engine     ForkEngine
	sandboxAPI *SandboxAPI
}

func NewServer(engine ForkEngine, sandboxAPI *SandboxAPI) *Server {
	return &Server{engine: engine, sandboxAPI: sandboxAPI}
}

// RegisterForkDaemonServer registers the gRPC service.
func RegisterForkDaemonServer(s *grpc.Server, srv *Server) {
	forkdpb.RegisterForkDaemonServer(s, &grpcService{srv: srv})
}

// ServeHTTP starts the HTTP server for metrics, health, and sandbox API.
func ServeHTTP(addr string, engine ForkEngine, sandboxAPI *SandboxAPI) {
	mux := http.NewServeMux()

	// Metrics
	mux.Handle("/metrics", promhttp.Handler())

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		cap := engine.GetCapacity()
		if cap.KVMAvailable {
			fmt.Fprint(w, "ok (kvm)")
		} else {
			fmt.Fprint(w, "ok (mock)")
		}
	})

	// Sandbox exec/files API — this is what the SDK talks to
	apiHandler := sandboxAPI.Handler()
	mux.Handle("/v1/", apiHandler)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("forkd: http server: %v", err)
	}
}

// Fork handles a fork request from the controller.
func (s *Server) Fork(ctx context.Context, snapshotID, sandboxID string, env, secrets map[string]string) (*fork.ForkResult, error) {
	result, err := s.engine.Fork(snapshotID, sandboxID, fork.ForkOpts{
		Env:     env,
		Secrets: secrets,
	})
	if err != nil {
		return nil, err
	}

	forkDuration.Observe(result.ForkTimeMs / 1000.0)
	activeSandboxes.Inc()

	if err := s.deliverConfig(result.SandboxID, result.VsockPath, env, secrets); err != nil {
		// A sandbox that reports Ready without its secrets is a lie; reap it.
		_ = s.engine.Terminate(result.SandboxID)
		activeSandboxes.Dec()
		s.sandboxAPI.UnregisterSandbox(result.SandboxID)
		return nil, fmt.Errorf("sandbox %s: secret delivery failed: %w", result.SandboxID, err)
	}

	return result, nil
}

// deliverConfig connects the guest agent and delivers claim-time env+secrets.
// Strict when the engine is real and secrets are present: failure is returned
// so the caller can reap the sandbox. Env-only failures are logged
// (best-effort), and the mock engine is skipped entirely — no guest exists.
// Secret values are never logged.
func (s *Server) deliverConfig(sandboxID, vsockPath string, env, secrets map[string]string) error {
	if !s.engine.GetCapacity().KVMAvailable {
		return nil // mock engine: no guest to deliver to
	}

	strict := len(secrets) > 0

	if err := s.sandboxAPI.RegisterSandbox(sandboxID, vsockPath); err != nil {
		if strict {
			return fmt.Errorf("guest agent not connected: %w", err)
		}
		log.Printf("forkd: sandbox %s: guest agent not connected: %v", sandboxID, err)
		return nil
	}

	if len(env) == 0 && len(secrets) == 0 {
		return nil
	}
	if err := s.sandboxAPI.Configure(sandboxID, env, secrets); err != nil {
		if strict {
			return fmt.Errorf("configure guest: %w", err)
		}
		log.Printf("forkd: sandbox %s: env delivery failed (best-effort): %v", sandboxID, err)
	}
	return nil
}

// ForkRunning checkpoints a running sandbox and forks it.
//
// ForkRunning deliberately does NOT deliver new config: forks inherit the
// source VM's memory, including any previously delivered env+secrets.
// Fresh-credential reissue for live forks is issue #7's end state.
func (s *Server) ForkRunning(ctx context.Context, sourceSandboxID, newSandboxID string, pauseSource bool) (*fork.ForkResult, error) {
	result, err := s.engine.ForkRunning(sourceSandboxID, newSandboxID, pauseSource)
	if err != nil {
		return nil, err
	}

	forkDuration.Observe(result.ForkTimeMs / 1000.0)
	activeSandboxes.Inc()

	s.registerAgent(result.SandboxID, result.VsockPath)
	return result, nil
}

// registerAgent connects the sandbox API to the guest agent. Failure is
// logged, not fatal: the sandbox is running, but exec/files will 404 until
// an agent connection is established (mock mode has no agent at all).
func (s *Server) registerAgent(sandboxID, vsockPath string) {
	if err := s.sandboxAPI.RegisterSandbox(sandboxID, vsockPath); err != nil {
		log.Printf("forkd: sandbox %s: guest agent not connected: %v", sandboxID, err)
	}
}

// Terminate handles a sandbox termination request.
func (s *Server) Terminate(ctx context.Context, sandboxID string) error {
	s.sandboxAPI.UnregisterSandbox(sandboxID)

	if err := s.engine.Terminate(sandboxID); err != nil {
		return err
	}
	activeSandboxes.Dec()
	return nil
}

// UpdateMetrics refreshes capacity metrics.
func (s *Server) UpdateMetrics() {
	cap := s.engine.GetCapacity()
	activeSandboxes.Set(float64(cap.ActiveSandboxes))
	memoryShared.Set(float64(cap.MemoryShared))
	memoryUnique.Set(float64(cap.MemoryUsed - cap.MemoryShared))
}
