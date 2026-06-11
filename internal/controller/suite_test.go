package controller_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/controller"
	"github.com/paperclipinc/sandbox/internal/husk"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	testEnv      *envtest.Environment
	cfg          *rest.Config
	k8sClient    client.Client
	scheme       *runtime.Scheme
	ctx          context.Context
	cancel       context.CancelFunc
	testRegistry *controller.NodeRegistry
	// logBuf accumulates the controller's log output for the whole suite so a
	// test can assert a secret value never appears in any log line. It is
	// concurrency-safe because the manager logs from reconcile goroutines.
	logBuf = &syncBuffer{}

	// huskTestActivatorMu guards the swappable husk activator the suite's
	// husk-enabled claim reconciler dials through. Tests set it via
	// setHuskTestActivator before creating their claim.
	huskTestActivatorMu sync.Mutex
	huskTestActivator   func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)

	// huskTestCheckpointerMu guards the swappable live-VM checkpointer the
	// suite's husk reconciler routes a Checkpoint drain policy through.
	huskTestCheckpointerMu sync.Mutex
	huskTestCheckpointer   func(ctx context.Context, claim *v1alpha1.SandboxClaim, pod *corev1.Pod) (bool, error)
)

// setHuskTestCheckpointer installs the checkpointer the suite reconciler uses
// for the Checkpoint drain policy; nil falls back to the default.
func setHuskTestCheckpointer(fn func(ctx context.Context, claim *v1alpha1.SandboxClaim, pod *corev1.Pod) (bool, error)) {
	huskTestCheckpointerMu.Lock()
	defer huskTestCheckpointerMu.Unlock()
	huskTestCheckpointer = fn
}

// currentHuskTestCheckpointer returns the installed checkpointer, or nil so the
// reconciler uses its default (re-pend without a captured snapshot).
func currentHuskTestCheckpointer() func(ctx context.Context, claim *v1alpha1.SandboxClaim, pod *corev1.Pod) (bool, error) {
	huskTestCheckpointerMu.Lock()
	defer huskTestCheckpointerMu.Unlock()
	return huskTestCheckpointer
}

// setHuskTestActivator installs the husk activator the suite reconciler uses.
func setHuskTestActivator(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)) {
	huskTestActivatorMu.Lock()
	defer huskTestActivatorMu.Unlock()
	huskTestActivator = fn
}

// currentHuskTestActivator returns the installed activator, or a default that
// fails closed (so a test that forgot to set one does not silently pass).
func currentHuskTestActivator() func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error) {
	huskTestActivatorMu.Lock()
	defer huskTestActivatorMu.Unlock()
	if huskTestActivator == nil {
		return func(context.Context, string, *tls.Config, husk.ActivateRequest) (husk.ActivateResult, error) {
			return husk.ActivateResult{OK: false, Error: "no husk test activator installed"}, nil
		}
	}
	return huskTestActivator
}

// syncBuffer is a concurrency-safe io.Writer that accumulates everything
// written and lets a test snapshot the bytes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func TestMain(m *testing.M) {
	// Tee the controller logs into logBuf (and still to stderr) so secret-leak
	// assertions can scan everything the controller logged.
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(io.MultiWriter(os.Stderr, logBuf))))

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	scheme = runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	// core/v1 too: the claim and fork reconcilers create token Secrets.
	_ = clientgoscheme.AddToScheme(scheme)

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "deploy", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic(err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	// Start controller manager in background
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		panic(err)
	}

	nodeRegistry := controller.NewNodeRegistry()
	testRegistry = nodeRegistry

	// The suite's manager-level pool reconciler runs the raw-forkd path
	// explicitly (EnableHuskPods false). The husk-mode pool reconcile (build the
	// snapshot + create husk pods) is covered by a directly driven reconciler in
	// husk_pool_build_test.go, so the manager does not create husk pods for every
	// pool every other test makes. With the default now husk-on in
	// cmd/controller, each test is explicit about its mode so both paths stay
	// covered.
	_ = (&controller.SandboxPoolReconciler{
		Client:         mgr.GetClient(),
		NodeRegistry:   nodeRegistry,
		EnableHuskPods: false,
	}).SetupWithManager(mgr)

	rawClaim := &controller.SandboxClaimReconciler{
		Client:       mgr.GetClient(),
		NodeRegistry: nodeRegistry,
	}
	// The raw (forkd) claim reconciler ignores husk-test claims so it does not
	// fight the husk reconciler over the same object.
	rawClaim.SkipLabel(controller.HuskTestClaimLabel)
	if err := rawClaim.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	// A husk-enabled claim reconciler that handles ONLY husk-test claims. Its
	// activator is swappable per test via setHuskTestActivator.
	huskClaim := &controller.SandboxClaimReconciler{
		Client:         mgr.GetClient(),
		NodeRegistry:   nodeRegistry,
		EnableHuskPods: true,
		HuskTLS:        &tls.Config{}, //nolint:gosec // test stub; the fake activator ignores it
	}
	huskClaim.OnlyLabel(controller.HuskTestClaimLabel)
	huskClaim.SetActivateForTest(func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error) {
		return currentHuskTestActivator()(ctx, addr, tlsConf, req)
	})
	huskClaim.SetCheckpointForTest(func(c context.Context, claim *v1alpha1.SandboxClaim, pod *corev1.Pod) (bool, error) {
		if fn := currentHuskTestCheckpointer(); fn != nil {
			return fn(c, claim, pod)
		}
		return false, nil
	})
	if err := huskClaim.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	_ = (&controller.SandboxForkReconciler{
		Client:       mgr.GetClient(),
		NodeRegistry: nodeRegistry,
	}).SetupWithManager(mgr)

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(err)
		}
	}()

	// Wait for manager cache sync
	time.Sleep(1 * time.Second)

	exitCode := m.Run()

	cancel()
	testEnv.Stop()
	_ = exitCode
}
