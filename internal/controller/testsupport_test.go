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

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/daemon"
	"github.com/paperclipinc/sandbox/internal/fork"
	"github.com/paperclipinc/sandbox/internal/observability"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
)

// BuildHuskPodForTest exposes buildHuskPod to the external controller_test
// package so the husk pod spec can be unit-tested.
func (r *SandboxPoolReconciler) BuildHuskPodForTest(pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate, opts HuskPodOptions) *corev1.Pod {
	return r.buildHuskPod(pool, template, opts)
}

// ReconcileHuskPodsForTest exposes reconcileHuskPods to the external
// controller_test package so the warm-pool lifecycle can be envtested.
func (r *SandboxPoolReconciler) ReconcileHuskPodsForTest(ctx context.Context, pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate) (int32, error) {
	return r.reconcileHuskPods(ctx, pool, template)
}

// EncKeyRecorder records, per RPC, the length of any EncryptionKey the fake
// forkd received. It records presence/length only, NEVER the key value, so a
// test can assert the controller delivered a key without the value ever
// touching test state or logs.
type EncKeyRecorder struct {
	mu            sync.Mutex
	createKeyLen  int
	createKeySeen bool
	forkKeyLen    int
	forkKeySeen   bool
}

// CreateTemplateKeyLen returns whether a CreateTemplate carried an encryption
// key and its length.
func (r *EncKeyRecorder) CreateTemplateKeyLen() (seen bool, length int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createKeySeen, r.createKeyLen
}

// ForkKeyLen returns whether a Fork carried an encryption key and its length.
func (r *EncKeyRecorder) ForkKeyLen() (seen bool, length int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forkKeySeen, r.forkKeyLen
}

func (r *EncKeyRecorder) interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		switch m := req.(type) {
		case *forkdpb.CreateTemplateRequest:
			r.mu.Lock()
			r.createKeySeen = true
			r.createKeyLen = len(m.EncryptionKey)
			r.mu.Unlock()
		case *forkdpb.ForkRequest:
			r.mu.Lock()
			r.forkKeySeen = true
			r.forkKeyLen = len(m.EncryptionKey)
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
