package daemon

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"

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
	// forkGeneration is a monotonic per-forkd counter handed to the guest on
	// every fork. Uniqueness within this process is what matters (so a guest
	// can tell two restores apart); global ordering does not.
	forkGeneration atomic.Uint64
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

	// Sandbox exec/files API: this is what the SDK talks to
	apiHandler := sandboxAPI.Handler()
	mux.Handle("/v1/", apiHandler)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("forkd: http server: %v", err)
	}
}

// Fork handles a fork request from the controller. apiToken is the bearer
// token the HTTP sandbox API will require for this sandbox; an empty token
// registers NOTHING, so HTTP calls to the sandbox fail closed with 401
// (forkd never runs the API in tokenless mode). The token value is never
// logged.
func (s *Server) Fork(ctx context.Context, snapshotID, sandboxID string, env, secrets map[string]string, apiToken string) (*fork.ForkResult, error) {
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

	s.sandboxAPI.RegisterToken(result.SandboxID, apiToken)
	return result, nil
}

// deliverConfig is the post-restore guest handshake: connect the agent, repair
// fork-shared state via NotifyForked, then deliver claim-time env+secrets.
//
// The mock engine is skipped entirely; no guest exists.
//
// On a real engine the agent connection and NotifyForked are ALWAYS strict:
// failure returns an error so the caller reaps the sandbox. A fork whose guest
// did not reseed its RNG shares CRNG state with its siblings, which is
// incorrect (not merely degraded), so it must never report Ready.
//
// Config delivery keeps its prior policy: strict only when secrets are present
// (a sandbox Ready without its secrets is a lie); env-only failures are
// best-effort. Secret values and entropy are never logged.
func (s *Server) deliverConfig(sandboxID, vsockPath string, env, secrets map[string]string) error {
	if !s.engine.GetCapacity().KVMAvailable {
		return nil // mock engine: no guest to deliver to
	}

	if err := s.sandboxAPI.RegisterSandbox(sandboxID, vsockPath); err != nil {
		return fmt.Errorf("guest agent not connected: %w", err)
	}

	if err := s.notifyForked(sandboxID); err != nil {
		return err
	}

	if len(env) == 0 && len(secrets) == 0 {
		return nil
	}
	if err := s.sandboxAPI.Configure(sandboxID, env, secrets); err != nil {
		if len(secrets) > 0 {
			return fmt.Errorf("configure guest: %w", err)
		}
		log.Printf("forkd: sandbox %s: env delivery failed (best-effort): %v", sandboxID, err)
	}
	return nil
}

// notifyForked sends the fork notification (fresh generation + 32 bytes of
// crypto/rand entropy) to a connected guest. The agent must already be
// registered. Entropy is never logged. Errors are returned so the caller can
// reap the sandbox: a guest that did not reseed shares RNG state.
func (s *Server) notifyForked(sandboxID string) error {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("generate fork entropy: %w", err)
	}
	gen := s.forkGeneration.Add(1)
	if err := s.sandboxAPI.NotifyForked(sandboxID, gen, entropy); err != nil {
		return fmt.Errorf("notify guest of fork: %w", err)
	}
	return nil
}

// ForkRunning checkpoints a running sandbox and forks it.
//
// ForkRunning deliberately does NOT deliver new config: forks inherit the
// source VM's memory, including any previously delivered env+secrets.
// Fresh-credential reissue for live forks is issue #7's end state.
//
// It MUST still send NotifyForked: a live-fork child boots from the parent's
// exact memory image, so it shares the parent's CRNG and userspace PRNG state.
// That is precisely the fork-correctness hazard, so the same fail-closed
// policy as restore-from-snapshot applies on a real engine.
//
// apiToken is the new sandbox's own bearer token (the source's token does
// NOT open the fork). Empty means no token is registered and HTTP calls to
// the fork fail closed with 401.
func (s *Server) ForkRunning(ctx context.Context, sourceSandboxID, newSandboxID string, pauseSource bool, apiToken string) (*fork.ForkResult, error) {
	result, err := s.engine.ForkRunning(sourceSandboxID, newSandboxID, pauseSource)
	if err != nil {
		return nil, err
	}

	forkDuration.Observe(result.ForkTimeMs / 1000.0)
	activeSandboxes.Inc()

	if err := s.notifyForkedRunning(result.SandboxID, result.VsockPath); err != nil {
		// A live fork that did not reseed shares its parent's RNG state; reap it.
		_ = s.engine.Terminate(result.SandboxID)
		activeSandboxes.Dec()
		s.sandboxAPI.UnregisterSandbox(result.SandboxID)
		return nil, fmt.Errorf("sandbox %s: fork notification failed: %w", result.SandboxID, err)
	}

	s.sandboxAPI.RegisterToken(result.SandboxID, apiToken)
	return result, nil
}

// notifyForkedRunning connects the agent and sends NotifyForked for a live
// fork, without delivering config (live forks inherit the parent's env). On
// the mock engine there is no guest, so it is a no-op. Strict on a real
// engine: see ForkRunning.
func (s *Server) notifyForkedRunning(sandboxID, vsockPath string) error {
	if !s.engine.GetCapacity().KVMAvailable {
		return nil // mock engine: no guest to notify
	}
	if err := s.sandboxAPI.RegisterSandbox(sandboxID, vsockPath); err != nil {
		return fmt.Errorf("guest agent not connected: %w", err)
	}
	return s.notifyForked(sandboxID)
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
