package controller

// Test support: used by envtest suites. Kept in the main package so external
// test packages (controller_test) can start fake forkd nodes.

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/daemon"
	"github.com/paperclipinc/mitos/internal/eventfeed"
	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/husk"
	"github.com/paperclipinc/mitos/internal/observability"
	"github.com/paperclipinc/mitos/internal/workspace"
	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// TraceIDAnnotationsForTest exposes traceIDAnnotations to the external
// controller_test package so the trace-id stamp omit branch (tracing off ->
// nil, no fake id) can be unit-tested deterministically without the OTel global
// provider one-time-delegate gotcha.
func TraceIDAnnotationsForTest(ctx context.Context) map[string]string {
	return traceIDAnnotations(ctx)
}

// BuildHuskPodForTest exposes buildHuskPod to the external controller_test
// package so the husk pod spec can be unit-tested.
func (r *SandboxPoolReconciler) BuildHuskPodForTest(pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate, opts HuskPodOptions) *corev1.Pod {
	return r.buildHuskPod(pool, template, opts)
}

// HuskTestClaimLabel marks a claim as owned by the husk-activation tests. The
// suite registers the raw claim reconciler to SKIP these (so it does not fight a
// manually driven husk reconciler over the same object) and a husk-enabled
// reconciler to handle ONLY these.
const HuskTestClaimLabel = "mitos.run/husk-test"

// SkipLabel restricts this reconciler to claims WITHOUT the given label; only
// used by the test harness so a raw and a husk reconciler can share one manager.
func (r *SandboxClaimReconciler) SkipLabel(label string) {
	r.eventFilter = predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[label] == ""
	})
	r.controllerName = "sandboxclaim-raw"
}

// OnlyLabel restricts this reconciler to claims WITH the given label.
func (r *SandboxClaimReconciler) OnlyLabel(label string) {
	r.eventFilter = predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[label] != ""
	})
	r.controllerName = "sandboxclaim-husk"
}

// SetActivateForTest injects a fake husk activator (the test seam).
func (r *SandboxClaimReconciler) SetActivateForTest(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)) {
	r.Activate = fn
}

// SetFeedForTest wires the change feed (the Kubernetes Event recorder, the
// CloudEvents sink, and a pinned clock) so envtest can assert both the Event
// mirror and the CloudEvents emit without a real webhook or wall clock.
func (r *SandboxClaimReconciler) SetFeedForTest(recorder record.EventRecorder, sink eventfeed.Sink, clock func() time.Time) {
	r.Feed = NewEmitFeed(recorder, sink, clock)
}

// EmitRevisionCreatedForTest exposes emitRevisionCreated to the external
// controller_test package so the revision.created payload mapping (including the
// mitos.run/trace-id annotation -> TraceID correlation field) can be
// unit-tested directly against a recording sink, without a full reconcile.
func EmitRevisionCreatedForTest(recorder record.EventRecorder, sink eventfeed.Sink, rev *v1alpha1.WorkspaceRevision) {
	NewEmitFeed(recorder, sink, nil).emitRevisionCreated(context.Background(), rev)
}

// SetCheckpointForTest injects a fake live-VM checkpointer (the drain seam).
// The fake records whether the Checkpoint drain policy routed through it and
// returns the scripted captured/error. nil restores the default.
func (r *SandboxClaimReconciler) SetCheckpointForTest(fn func(ctx context.Context, claim *v1alpha1.SandboxClaim, pod *corev1.Pod) (bool, error)) {
	r.Checkpoint = fn
}

// SetWorkspaceTransferForTest injects the workspace hydrate/dehydrate/diff/git
// seams so envtest can drive the binding lifecycle without a VM. hydrate records
// the manifest it was asked to restore; dehydrate returns a scripted digest and
// records the exclude and capture lists it was passed; diff returns a scripted
// content diff; rendezvous records the git push it was asked to make. A nil diff
// or rendezvous leaves the production default in place.
func (r *SandboxClaimReconciler) SetWorkspaceTransferForTest(
	hydrate func(ctx context.Context, claim *v1alpha1.SandboxClaim, manifest cas.Digest) error,
	dehydrate func(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths, capturePaths []string) (cas.Digest, error),
	diff func(ctx context.Context, claim *v1alpha1.SandboxClaim, parent, child cas.Digest) (workspace.Diff, error),
	rendezvous func(ctx context.Context, repoFiles map[string]string, remote, branch string) error,
	repoFiles func(ctx context.Context, claim *v1alpha1.SandboxClaim, digest cas.Digest, gitPaths []string) (map[string]string, error),
) {
	r.HydrateWorkspace = hydrate
	r.DehydrateWorkspace = dehydrate
	r.DiffWorkspace = diff
	r.RendezvousGit = rendezvous
	r.RepoFilesForGit = repoFiles
}

// MemSnapshotResultForTest is the exported alias of the unexported
// memSnapshotResult so the external controller_test package can name the
// checkpoint fake's return type.
type MemSnapshotResultForTest = memSnapshotResult

// NewMemSnapshotResult builds a MemSnapshotResultForTest for tests.
func NewMemSnapshotResult(ref, principal string) MemSnapshotResultForTest {
	return memSnapshotResult{Ref: ref, Principal: principal}
}

// SetMemorySnapshotForTest injects the memory-snapshot pairing seams (W4 Task
// 2): the checkpoint-on-terminate capture, the resume-on-activate restore, and
// the principal-bound existence check. envtest drives the pairing decision and
// the resume/hydrate request without a real VM.
func (r *SandboxClaimReconciler) SetMemorySnapshotForTest(
	checkpoint func(ctx context.Context, claim *v1alpha1.SandboxClaim) (MemSnapshotResultForTest, error),
	resume func(ctx context.Context, claim *v1alpha1.SandboxClaim, ref string) error,
	exists func(ctx context.Context, ref, principal string) (bool, error),
) {
	r.CheckpointMemory = checkpoint
	r.ResumeMemory = resume
	r.MemorySnapshotExists = exists
}

// SetSnapshotExistsForTest injects the workspace reconciler's resumable
// existence check so a test can flip a head's snapshot present/absent.
func (r *WorkspaceReconciler) SetSnapshotExistsForTest(exists func(ctx context.Context, ref, principal string) (bool, error)) {
	r.SnapshotExists = exists
}

// EnsureHuskPDBForTest exposes ensureHuskPDB to the external controller_test
// package so the PDB create-or-update can be envtested directly.
func (r *SandboxPoolReconciler) EnsureHuskPDBForTest(ctx context.Context, pool *v1alpha1.SandboxPool) error {
	return r.ensureHuskPDB(ctx, pool)
}

// ReconcileHuskPodsForTest exposes reconcileHuskPods to the external
// controller_test package so the warm-pool lifecycle can be envtested.
func (r *SandboxPoolReconciler) ReconcileHuskPodsForTest(ctx context.Context, pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate) (int32, error) {
	res, err := r.reconcileHuskPods(ctx, pool, template, pool.Spec.Replicas)
	return res.dormant, err
}

// EnsureTemplateBuiltForTest exposes ensureTemplateBuilt to the external
// controller_test package so the husk-mode "build the snapshot first" half can
// be envtested without driving the full Reconcile (which would race the
// manager's pool reconciler on the pool status subresource).
func (r *SandboxPoolReconciler) EnsureTemplateBuiltForTest(ctx context.Context, pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate) error {
	return r.ensureTemplateBuilt(ctx, pool, template)
}

// EncKeyRecorder records, per RPC, the length of any EncryptionKey the fake
// forkd received. It records presence/length only, NEVER the key value, so a
// test can assert the controller delivered a key without the value ever
// touching test state or logs.
type EncKeyRecorder struct {
	mu            sync.Mutex
	createKeyLen  int
	createKeySeen bool
	createKekID   string
	forkKeyLen    int
	forkKeySeen   bool
	forkKekID     string
}

// CreateTemplateKeyLen returns whether a CreateTemplate carried an encryption
// key (the wrapped DEK) and its length.
func (r *EncKeyRecorder) CreateTemplateKeyLen() (seen bool, length int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createKeySeen, r.createKeyLen
}

// CreateTemplateKekID returns the KEK id a CreateTemplate carried (non-secret).
func (r *EncKeyRecorder) CreateTemplateKekID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createKekID
}

// ForkKeyLen returns whether a Fork carried an encryption key (the wrapped DEK)
// and its length.
func (r *EncKeyRecorder) ForkKeyLen() (seen bool, length int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forkKeySeen, r.forkKeyLen
}

// ForkKekID returns the KEK id a Fork carried (non-secret).
func (r *EncKeyRecorder) ForkKekID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forkKekID
}

func (r *EncKeyRecorder) interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		switch m := req.(type) {
		case *forkdpb.CreateTemplateRequest:
			r.mu.Lock()
			r.createKeySeen = true
			r.createKeyLen = len(m.EncryptionKey)
			r.createKekID = m.KekId
			r.mu.Unlock()
		case *forkdpb.ForkRequest:
			r.mu.Lock()
			r.forkKeySeen = true
			r.forkKeyLen = len(m.EncryptionKey)
			r.forkKekID = m.KekId
			r.mu.Unlock()
		}
		return handler(ctx, req)
	}
}

// StartFakeForkdNodeEncRecording is StartFakeForkdNode that also installs an
// EncKeyRecorder so a test can assert whether the controller delivered an
// encryption key in CreateTemplate and Fork (presence/length only, not the
// value).
func StartFakeForkdNodeEncRecording(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), rec *EncKeyRecorder, err error) {
	rec = &EncKeyRecorder{}
	stop, err = startFakeForkdNodeWithInterceptor(registry, nodeName, rec.interceptor(), templates...)
	return stop, rec, err
}

// StartFakeForkdNodeEncRecordingTLS is StartFakeForkdNodeEncRecording over
// mTLS: the gRPC listener is terminated by serverTLS and the registered
// NodeInfo carries clientTLS, so dials to THIS node use TLS. The encryption key
// delivery guard requires an mTLS node, so the happy-path enc tests run here.
func StartFakeForkdNodeEncRecordingTLS(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, templates ...string) (stop func(), rec *EncKeyRecorder, err error) {
	rec = &EncKeyRecorder{}
	stop, _, _, err = startFakeForkdNodeOpts(registry, nodeName, serverTLS, clientTLS, rec.interceptor(), templates...)
	return stop, rec, err
}

// StartFakeForkdNode runs an in-process forkd gRPC server backed by a
// MockEngine with the given templates, registers it in the registry, and
// returns a stop function.
func StartFakeForkdNode(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), err error) {
	stop, _, _, err = startFakeForkdNode(registry, nodeName, nil, nil, templates...)
	return stop, err
}

// StartFakeForkdNodeRecording is StartFakeForkdNode that also returns the
// backing MockEngine, so tests can read engine.TerminatedIDs() to assert a
// VM was reaped via forkd Terminate, and a setActivity closure that stamps a
// sandbox's last-activity time on the node's SandboxAPI (for idle-reap tests).
func StartFakeForkdNodeRecording(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(sandboxID string, t time.Time), err error) {
	return startFakeForkdNode(registry, nodeName, nil, nil, templates...)
}

// StartFakeForkdNodeTLS is StartFakeForkdNode with mTLS: the gRPC listener
// is terminated by serverTLS and the registered NodeInfo carries clientTLS,
// so only dials to THIS node use TLS; other registered fakes stay insecure.
func StartFakeForkdNodeTLS(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, templates ...string) (stop func(), err error) {
	stop, _, _, err = startFakeForkdNode(registry, nodeName, serverTLS, clientTLS, templates...)
	return stop, err
}

// startFakeForkdNodeWithInterceptor starts a fake forkd node with an extra
// unary server interceptor (used to record the request-delivered encryption
// key) and otherwise behaves like StartFakeForkdNode.
func startFakeForkdNodeWithInterceptor(registry *NodeRegistry, nodeName string, interceptor grpc.UnaryServerInterceptor, templates ...string) (stop func(), err error) {
	stop, _, _, err = startFakeForkdNodeOpts(registry, nodeName, nil, nil, interceptor, templates...)
	return stop, err
}

func startFakeForkdNode(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(string, time.Time), err error) {
	return startFakeForkdNodeOpts(registry, nodeName, serverTLS, clientTLS, nil, templates...)
}

func startFakeForkdNodeOpts(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, interceptor grpc.UnaryServerInterceptor, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(string, time.Time), err error) {
	engine = fork.NewMockEngine()
	engine.ForkDelay = 0
	for _, tmpl := range templates {
		if err := engine.CreateTemplate(tmpl, tmpl, nil, nil); err != nil {
			return nil, nil, nil, err
		}
	}
	dir, err := os.MkdirTemp("", "fake-forkd-*")
	if err != nil {
		return nil, nil, nil, err
	}
	sandboxAPI := daemon.NewSandboxAPI(dir)
	srv := daemon.NewServer(engine, sandboxAPI)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, nil, err
	}
	// The otelgrpc server handler mirrors forkd's real gRPC server so the
	// propagated trace context is honored: the forkd.Fork span joins the
	// controller's trace, which the cross-process propagation test asserts.
	opts := []grpc.ServerOption{grpc.StatsHandler(observability.GRPCServerStatsHandler())}
	if serverTLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}
	if interceptor != nil {
		opts = append(opts, grpc.UnaryInterceptor(interceptor))
	}
	gs := grpc.NewServer(opts...)
	daemon.RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)

	// Real HTTP sandbox API on a real listener, exactly the handler forkd
	// serves on :9091, so envtest claims can exercise bearer-token auth
	// end to end against the registered HTTPEndpoint.
	httpSrv := httptest.NewServer(sandboxAPI.Handler())

	registry.Register(&NodeInfo{
		Name:         nodeName,
		Endpoint:     lis.Addr().String(),
		HTTPEndpoint: strings.TrimPrefix(httpSrv.URL, "http://"),
		TemplateIDs:  templates,
		MaxSandboxes: 100,
		TLS:          clientTLS,
	})
	setActivity = func(sandboxID string, t time.Time) {
		sandboxAPI.RecordActivity(sandboxID, t)
	}
	return func() {
		gs.Stop()
		httpSrv.Close()
		os.RemoveAll(dir)
		registry.Unregister(nodeName)
	}, engine, setActivity, nil
}
