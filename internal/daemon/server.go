package daemon

import (
	"context"
	"fmt"
	"net/http"

	"github.com/paperclipinc/sandbox/internal/fork"
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
	// TODO: register generated protobuf service
	_ = s
	_ = srv
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

	http.ListenAndServe(addr, mux)
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

	// Connect to the guest agent so exec/files work
	vsockPath := fmt.Sprintf("/var/lib/agent-run/sandboxes/%s/vsock.sock", sandboxID)
	s.sandboxAPI.RegisterSandbox(sandboxID, vsockPath)

	return result, nil
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
