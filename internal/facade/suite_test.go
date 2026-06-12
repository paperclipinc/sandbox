package facade_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	runv1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/facade"
)

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	scheme    *runtime.Scheme
	testCtx   context.Context
	cancel    context.CancelFunc
)

// TestMain stands up an envtest apiserver with BOTH the upstream
// agents.x-k8s.io Sandbox CRD (vendored under third_party/agent-sandbox) and
// our agentrun.dev CRDs installed, then runs the facade reconciler against it.
// This proves the Sandbox -> husk run-path lifecycle end to end.
func TestMain(m *testing.M) {
	testCtx, cancel = context.WithCancel(context.Background())
	defer cancel()

	scheme = runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := extv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := runv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			// Our agentrun.dev CRDs (the SandboxClaim the facade creates).
			filepath.Join("..", "..", "deploy", "crds"),
			// The vendored upstream agents.x-k8s.io Sandbox CRD.
			filepath.Join("..", "..", "third_party", "agent-sandbox", "crds"),
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

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		panic(err)
	}

	if err := (&facade.SandboxReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		DefaultPool:   "default-pool",
		ClusterDomain: "cluster.local",
	}).SetupWithManager(mgr); err != nil {
		panic(err)
	}
	if err := (&facade.SandboxTemplateReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		panic(err)
	}
	if err := (&facade.SandboxWarmPoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		panic(err)
	}
	if err := (&facade.SandboxClaimReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		ClusterDomain: "cluster.local",
	}).SetupWithManager(mgr); err != nil {
		panic(err)
	}

	go func() {
		if err := mgr.Start(testCtx); err != nil {
			panic(err)
		}
	}()

	code := m.Run()

	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

// eventually polls cond until it returns true or the deadline passes, failing
// the test otherwise. envtest has no informer-cache settle guarantees, so the
// facade's create -> status mirror is observed by polling.
func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// updateWithRetry re-Gets obj and re-applies mutate before each Update attempt,
// so a test spec change does not flake on an optimistic-lock conflict with the
// reconciler's concurrent status writes.
func updateWithRetry(t *testing.T, key types.NamespacedName, obj client.Object, mutate func()) {
	t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(testCtx, key, obj); err != nil {
			return err
		}
		mutate()
		return k8sClient.Update(testCtx, obj)
	}); err != nil {
		t.Fatalf("update %s: %v", key.Name, err)
	}
}

// statusUpdateWithRetry re-Gets obj and re-applies mutate before each
// Status().Update attempt, so a test status write does not flake on an
// optimistic-lock conflict with the reconciler's concurrent writes.
func statusUpdateWithRetry(t *testing.T, key types.NamespacedName, obj client.Object, mutate func()) {
	t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(testCtx, key, obj); err != nil {
			return err
		}
		mutate()
		return k8sClient.Status().Update(testCtx, obj)
	}); err != nil {
		t.Fatalf("status update %s: %v", key.Name, err)
	}
}
