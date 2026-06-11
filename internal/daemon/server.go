package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/paperclipinc/sandbox/internal/fork"
	"github.com/paperclipinc/sandbox/internal/volume"
	"github.com/paperclipinc/sandbox/internal/vsock"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		Help: "CoW-aware shared memory: each template's shared page set counted once",
	})
	memoryUnique = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentrun_memory_unique_bytes",
		Help: "Per-fork unique memory (dirty pages) summed over all sandboxes",
	})
	cowMemorySavings = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentrun_cow_memory_savings_bytes",
		Help: "Memory the CoW model reveals is not consumed per-fork (naive minus CoW-aware)",
	})
	meteredDisk = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentrun_metered_disk_bytes",
		Help: "CoW-aware metered backing storage: template volume seeds counted once",
	})
)

func init() {
	prometheus.MustRegister(forkDuration, activeSandboxes, memoryShared, memoryUnique, cowMemorySavings, meteredDisk)
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

	// Node-level CoW-aware metering report for operators/billing. This is
	// node-scoped operational data (the same access class as /metrics and
	// /healthz, which are served unauthenticated on this operational mux), NOT
	// per-sandbox traffic, so it is deliberately NOT behind the per-sandbox
	// bearer-token middleware that wraps the /v1/exec and /v1/files routes. A
	// sandbox bearer token grants no access here, and this endpoint never
	// returns secret values: only ids, template names, and byte counts. It is
	// registered before the catch-all /v1/ handler so it takes precedence.
	mux.Handle("GET /v1/metering", meteringHandler(engine))

	// Sandbox exec/files API: this is what the SDK talks to
	apiHandler := sandboxAPI.Handler()
	mux.Handle("/v1/", apiHandler)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("forkd: http server: %v", err)
	}
}

// meteringHandler serves the node-level CoW-aware metering report as JSON. It
// is operator/billing data, not per-sandbox traffic, so it carries no
// per-sandbox bearer auth (it shares the access class of /metrics and
// /healthz). The report holds only ids, template names, and byte counts, never
// secret values.
func meteringHandler(engine ForkEngine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		report := engine.Metering()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(report); err != nil {
			log.Printf("forkd: encode metering report: %v", err)
		}
	})
}

// Fork handles a fork request from the controller. apiToken is the bearer
// token the HTTP sandbox API will require for this sandbox; an empty token
// registers NOTHING, so HTTP calls to the sandbox fail closed with 401
// (forkd never runs the API in tokenless mode). The token value is never
// logged.
//
// netConf carries the template's NetworkPolicy (egress policy + allowlist).
// It is parsed into fork.NetworkOpts and threaded into the engine, which uses
// it to build the per-fork egress ruleset when networking is enabled. When
// netConf is nil the fork gets no network identity (networking disabled or no
// policy on the template). The egress policy and allowlist entries are safe to
// log.
func (s *Server) Fork(ctx context.Context, snapshotID, sandboxID string, env, secrets map[string]string, netConf *forkdpb.NetworkConfig, volumes []*forkdpb.VolumeMount, apiToken string) (*fork.ForkResult, error) {
	vols, err := volumeSpecs(volumes)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: invalid volume spec: %w", sandboxID, err)
	}
	// engine.fork wraps the snapshot restore (the <2ms hot path) as a child of
	// forkd.Fork. The engine signature takes no ctx, so the span is started
	// here around the call rather than threaded into the engine. Only ids and
	// the resulting fork time are recorded; no secret values.
	_, engineSpan := tracer.Start(ctx, "engine.fork", trace.WithAttributes(
		attribute.String("snapshot.id", snapshotID),
		attribute.String("sandbox.id", sandboxID),
	))
	result, err := s.engine.Fork(snapshotID, sandboxID, fork.ForkOpts{
		Env:     env,
		Secrets: secrets,
		Network: networkOpts(netConf),
		Volumes: vols,
	})
	if err != nil {
		engineSpan.RecordError(err)
		engineSpan.End()
		return nil, err
	}
	engineSpan.SetAttributes(attribute.Float64("fork_time_ms", result.ForkTimeMs))
	engineSpan.End()

	forkDuration.Observe(result.ForkTimeMs / 1000.0)
	activeSandboxes.Inc()

	if err := s.deliverConfig(result.SandboxID, result.VsockPath, env, secrets, result.GuestNetwork, result.VolumeMounts); err != nil {
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
func (s *Server) deliverConfig(sandboxID, vsockPath string, env, secrets map[string]string, guestNet *vsock.NotifyForkedNetwork, volumes []vsock.VolumeMountEntry) error {
	if !s.engine.GetCapacity().KVMAvailable {
		return nil // mock engine: no guest to deliver to
	}

	if err := s.sandboxAPI.RegisterSandbox(sandboxID, vsockPath); err != nil {
		return fmt.Errorf("guest agent not connected: %w", err)
	}

	if err := s.notifyForked(sandboxID, guestNet, volumes); err != nil {
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
func (s *Server) notifyForked(sandboxID string, guestNet *vsock.NotifyForkedNetwork, volumes []vsock.VolumeMountEntry) error {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("generate fork entropy: %w", err)
	}
	gen := s.forkGeneration.Add(1)
	if err := s.sandboxAPI.NotifyForked(sandboxID, gen, entropy, guestNet, volumes); err != nil {
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
	// Live forks inherit the source VM's baked network identity in memory; the
	// engine does not (yet) re-address them, so no per-fork network config is
	// delivered here. Distinct-identity live forks are a follow-up (#18). Live
	// forks also inherit the source's mounted volumes, so no mount table is sent.
	return s.notifyForked(sandboxID, nil, nil)
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

// ListSandboxes returns one SandboxInfo per sandbox the engine currently
// holds, merging the engine's created-at with the SandboxAPI's last-activity
// time. last_activity_unix is zero for sandboxes that have never been
// accessed; uptime_seconds is computed from created-at against the current
// time.
func (s *Server) ListSandboxes() []*forkdpb.SandboxInfo {
	records := s.engine.ListSandboxes()
	now := time.Now()
	out := make([]*forkdpb.SandboxInfo, 0, len(records))
	for _, rec := range records {
		var lastActivityUnix int64
		if last, ok := s.sandboxAPI.LastActivity(rec.ID); ok {
			lastActivityUnix = last.Unix()
		}
		var uptimeSeconds int64
		if !rec.CreatedAt.IsZero() {
			uptimeSeconds = int64(now.Sub(rec.CreatedAt).Seconds())
		}
		out = append(out, &forkdpb.SandboxInfo{
			SandboxId:        rec.ID,
			CreatedAtUnix:    rec.CreatedAt.Unix(),
			LastActivityUnix: lastActivityUnix,
			UptimeSeconds:    uptimeSeconds,
		})
	}
	return out
}

// networkOpts converts the proto NetworkConfig from a ForkRequest into the
// engine's fork.NetworkOpts. It returns nil when the request carries no
// network config (so the non-network fork path is untouched) and also when the
// config is effectively empty (no egress policy and no allowlist), which the
// engine treats the same as "no networking requested". The engine itself only
// acts on a non-nil result when networking is enabled.
func networkOpts(c *forkdpb.NetworkConfig) *fork.NetworkOpts {
	if c == nil {
		return nil
	}
	if c.EgressPolicy == "" && len(c.AllowList) == 0 {
		return nil
	}
	return &fork.NetworkOpts{
		EgressPolicy: c.EgressPolicy,
		AllowList:    c.AllowList,
	}
}

// volumeSpecs converts the proto VolumeMounts into engine volume specs. It
// parses each size string into megabytes and carries the fork policy through as
// the API string. An unparseable size fails the whole fork: a volume sized
// wrong is a hard misconfiguration, not something to silently default.
func volumeSpecs(mounts []*forkdpb.VolumeMount) ([]volume.Spec, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	specs := make([]volume.Spec, 0, len(mounts))
	for _, m := range mounts {
		// Volume names land in host backing paths and the Firecracker drive id,
		// so reject any name that could escape the sandbox volumes dir before it
		// reaches the engine or filesystem (the C1 traversal guard, mirrored from
		// validateSandboxID). Return InvalidArgument so the RPC fails loudly.
		if err := validateVolumeName(m.Name); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		sizeMB, err := volume.ParseSizeMB(m.Size)
		if err != nil {
			return nil, fmt.Errorf("volume %s: %w", m.Name, err)
		}
		specs = append(specs, volume.Spec{
			Name:      m.Name,
			SizeMB:    sizeMB,
			MountPath: m.MountPath,
			ReadOnly:  m.ReadOnly,
			Policy:    volume.ForkPolicy(m.ForkPolicy),
		})
	}
	return specs, nil
}

// UpdateMetrics refreshes capacity and metering gauges. Memory gauges are
// CoW-aware: shared is each template's shared set counted once, unique is the
// per-fork dirty total. The disk gauge reflects CoW-aware metered backing
// storage. ActiveSandboxes comes from the cheap capacity path; the rest from
// the full metering report (which also stats backing files).
func (s *Server) UpdateMetrics() {
	activeSandboxes.Set(float64(s.engine.GetCapacity().ActiveSandboxes))

	report := s.engine.Metering()
	memoryShared.Set(float64(report.SharedOnceTotal()))
	memoryUnique.Set(float64(report.TotalUnique))
	cowMemorySavings.Set(float64(report.CoWSavings))
	meteredDisk.Set(float64(report.DiskUsedCoWAware))
}
