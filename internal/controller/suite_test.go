package controller_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/eventfeed"
	"github.com/paperclipinc/mitos/internal/husk"
	"github.com/paperclipinc/mitos/internal/workspace"
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

// recordingSink is the suite's fake CloudEvents sink: it records every emitted
// event so a test can assert the feed envelope and dedupe id without a real
// webhook. Concurrency-safe (the manager emits from reconcile goroutines).
type recordingSink struct {
	mu     sync.Mutex
	events []eventfeed.Event
}

func (s *recordingSink) Emit(_ context.Context, e eventfeed.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

// byType returns the recorded events of the given CloudEvent type.
func (s *recordingSink) byType(t string) []eventfeed.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []eventfeed.Event
	for _, e := range s.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

var (
	// testSink records the feed CloudEvents the suite's raw claim reconciler
	// emits, so the binding/feed tests can assert the envelope and dedupe id.
	testSink = &recordingSink{}
	// testEventRecorder is the suite's Kubernetes Event recorder. It is a
	// non-blocking buffering recorder rather than record.NewFakeRecorder: the
	// FakeRecorder's channel BLOCKS the caller once full, and the suite emits one
	// Event per claim phase transition and per revision across every test on the
	// reconcile path, so a bounded channel would fill and stall reconciles (the
	// claims would time out). This recorder appends under a mutex and never
	// blocks; waitForEvent scans its snapshot.
	testEventRecorder = &bufferingRecorder{}
)

// bufferingRecorder is a non-blocking record.EventRecorder for the suite: it
// accumulates formatted events ("<type> <reason> <message>") under a mutex and
// never blocks the reconcile path. waitForEvent scans snapshot() for a match.
type bufferingRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *bufferingRecorder) record(eventtype, reason, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventtype+" "+reason+" "+msg)
}

func (r *bufferingRecorder) Event(_ runtime.Object, eventtype, reason, message string) {
	r.record(eventtype, reason, message)
}

func (r *bufferingRecorder) Eventf(_ runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.record(eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *bufferingRecorder) AnnotatedEventf(_ runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.record(eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *bufferingRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

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

	// wsTransferMu guards the swappable workspace hydrate/dehydrate fakes the
	// suite's raw claim reconciler drives. Tests set them via setWSTransfer.
	wsTransferMu sync.Mutex
	wsHydrate    func(ctx context.Context, claim *v1alpha1.SandboxClaim, manifest cas.Digest) error
	wsDehydrate  func(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths, capturePaths []string) (cas.Digest, error)
	wsDiff       func(ctx context.Context, claim *v1alpha1.SandboxClaim, parent, child cas.Digest) (workspace.Diff, error)
	wsRendezvous func(ctx context.Context, repoFiles map[string]string, remote, branch string) error
	wsRepoFiles  func(ctx context.Context, claim *v1alpha1.SandboxClaim, digest cas.Digest, gitPaths []string) (map[string]string, error)

	// memSnapshotMu guards the swappable memory-snapshot pairing fakes (W4 Task
	// 2). Tests set them via setMemSnapshot before creating their claim/workspace.
	memSnapshotMu sync.Mutex
	memCheckpoint func(ctx context.Context, claim *v1alpha1.SandboxClaim) (controller.MemSnapshotResultForTest, error)
	memResume     func(ctx context.Context, claim *v1alpha1.SandboxClaim, ref string) error
	memExists     func(ctx context.Context, ref, principal string) (bool, error)
)

// MemSnapshotResultForTest is re-exported below for the suite's swappable fake
// signature; the real result type is unexported in the controller package.

// setMemSnapshot installs the memory-snapshot pairing fakes. nil for any leaves
// a safe default: checkpoint returns nothing captured, resume errors (so a test
// that expects resume but forgot to install it fails), exists returns false.
func setMemSnapshot(
	checkpoint func(ctx context.Context, claim *v1alpha1.SandboxClaim) (controller.MemSnapshotResultForTest, error),
	resume func(ctx context.Context, claim *v1alpha1.SandboxClaim, ref string) error,
	exists func(ctx context.Context, ref, principal string) (bool, error),
) {
	memSnapshotMu.Lock()
	defer memSnapshotMu.Unlock()
	memCheckpoint = checkpoint
	memResume = resume
	memExists = exists
}

func currentMemCheckpoint() func(ctx context.Context, claim *v1alpha1.SandboxClaim) (controller.MemSnapshotResultForTest, error) {
	memSnapshotMu.Lock()
	defer memSnapshotMu.Unlock()
	return memCheckpoint
}

func currentMemResume() func(ctx context.Context, claim *v1alpha1.SandboxClaim, ref string) error {
	memSnapshotMu.Lock()
	defer memSnapshotMu.Unlock()
	return memResume
}

func currentMemExists() func(ctx context.Context, ref, principal string) (bool, error) {
	memSnapshotMu.Lock()
	defer memSnapshotMu.Unlock()
	return memExists
}

// setWSTransfer installs the workspace hydrate/dehydrate fakes; nil restores a
// default that fails closed so a test that forgot to set them does not silently
// pass.
func setWSTransfer(
	hydrate func(ctx context.Context, claim *v1alpha1.SandboxClaim, manifest cas.Digest) error,
	dehydrate func(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths, capturePaths []string) (cas.Digest, error),
) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	wsHydrate = hydrate
	wsDehydrate = dehydrate
}

// setWSDiff installs the workspace diff fake; nil falls back to a default that
// returns an empty diff so a test that does not exercise the diff path is
// unaffected.
func setWSDiff(diff func(ctx context.Context, claim *v1alpha1.SandboxClaim, parent, child cas.Digest) (workspace.Diff, error)) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	wsDiff = diff
}

// setWSRendezvous installs the git rendezvous fake; nil falls back to the
// production default (workspace.Rendezvous via the git CLI).
func setWSRendezvous(rv func(ctx context.Context, repoFiles map[string]string, remote, branch string) error) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	wsRendezvous = rv
}

// setWSRepoFiles installs the git repo-paths resolver fake; nil falls back to a
// default that resolves no files.
func setWSRepoFiles(fn func(ctx context.Context, claim *v1alpha1.SandboxClaim, digest cas.Digest, gitPaths []string) (map[string]string, error)) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	wsRepoFiles = fn
}

func currentWSRepoFiles() func(ctx context.Context, claim *v1alpha1.SandboxClaim, digest cas.Digest, gitPaths []string) (map[string]string, error) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsRepoFiles
}

func currentWSHydrate() func(ctx context.Context, claim *v1alpha1.SandboxClaim, manifest cas.Digest) error {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsHydrate
}

func currentWSDehydrate() func(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths, capturePaths []string) (cas.Digest, error) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsDehydrate
}

func currentWSDiff() func(ctx context.Context, claim *v1alpha1.SandboxClaim, parent, child cas.Digest) (workspace.Diff, error) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsDiff
}

func currentWSRendezvous() func(ctx context.Context, repoFiles map[string]string, remote, branch string) error {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsRendezvous
}

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
		APIReader:    mgr.GetAPIReader(),
		NodeRegistry: nodeRegistry,
	}
	// The raw (forkd) claim reconciler ignores husk-test claims so it does not
	// fight the husk reconciler over the same object.
	rawClaim.SkipLabel(controller.HuskTestClaimLabel)
	// Wire the change feed: the buffered FakeRecorder for the always-on Event
	// mirror and the recording sink for the CloudEvents egress. A nil clock uses
	// the wall clock; the feed tests assert the envelope, not an exact time.
	rawClaim.SetFeedForTest(testEventRecorder, testSink, nil)
	// Route the memory-snapshot pairing seams through the per-test swappable
	// fakes (W4 Task 2). Safe defaults: checkpoint captures nothing, resume
	// errors (a test that wants resume must install it), exists reports absent.
	rawClaim.SetMemorySnapshotForTest(
		func(ctx context.Context, claim *v1alpha1.SandboxClaim) (controller.MemSnapshotResultForTest, error) {
			if fn := currentMemCheckpoint(); fn != nil {
				return fn(ctx, claim)
			}
			return controller.MemSnapshotResultForTest{}, nil
		},
		func(ctx context.Context, claim *v1alpha1.SandboxClaim, ref string) error {
			if fn := currentMemResume(); fn != nil {
				return fn(ctx, claim, ref)
			}
			return fmt.Errorf("no memory resume fake installed")
		},
		func(ctx context.Context, ref, principal string) (bool, error) {
			if fn := currentMemExists(); fn != nil {
				return fn(ctx, ref, principal)
			}
			return false, nil
		},
	)
	// Route the workspace hydrate/dehydrate seams through the per-test swappable
	// fakes so the binding lifecycle is driven without a VM. A test that does not
	// install fakes but uses a workspaceRef gets a fail-closed default.
	rawClaim.SetWorkspaceTransferForTest(
		func(ctx context.Context, claim *v1alpha1.SandboxClaim, manifest cas.Digest) error {
			if fn := currentWSHydrate(); fn != nil {
				return fn(ctx, claim, manifest)
			}
			return fmt.Errorf("no workspace hydrate fake installed")
		},
		func(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths, capturePaths []string) (cas.Digest, error) {
			if fn := currentWSDehydrate(); fn != nil {
				return fn(ctx, claim, excludePaths, capturePaths)
			}
			return "", fmt.Errorf("no workspace dehydrate fake installed")
		},
		func(ctx context.Context, claim *v1alpha1.SandboxClaim, parent, child cas.Digest) (workspace.Diff, error) {
			if fn := currentWSDiff(); fn != nil {
				return fn(ctx, claim, parent, child)
			}
			return workspace.Diff{}, nil
		},
		func(ctx context.Context, repoFiles map[string]string, remote, branch string) error {
			if fn := currentWSRendezvous(); fn != nil {
				return fn(ctx, repoFiles, remote, branch)
			}
			return nil
		},
		func(ctx context.Context, claim *v1alpha1.SandboxClaim, digest cas.Digest, gitPaths []string) (map[string]string, error) {
			if fn := currentWSRepoFiles(); fn != nil {
				return fn(ctx, claim, digest, gitPaths)
			}
			return nil, nil
		},
	)
	if err := rawClaim.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	// A husk-enabled claim reconciler that handles ONLY husk-test claims. Its
	// activator is swappable per test via setHuskTestActivator.
	huskClaim := &controller.SandboxClaimReconciler{
		Client:         mgr.GetClient(),
		APIReader:      mgr.GetAPIReader(),
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

	// The Workspace reconciler (W4): manages the revision DAG, retention,
	// lineage, and head/revisions/resumable status. Core, not behind any flag.
	wsReconciler := &controller.WorkspaceReconciler{
		Client: mgr.GetClient(),
	}
	// The resumable status verifies a head's paired memory snapshot exists
	// (principal-bound) through the same swappable fake the resume path uses, so
	// a GC'd snapshot flips resumable false in the same test that drove the
	// checkpoint.
	wsReconciler.SetSnapshotExistsForTest(func(ctx context.Context, ref, principal string) (bool, error) {
		if fn := currentMemExists(); fn != nil {
			return fn(ctx, ref, principal)
		}
		return false, nil
	})
	if err := wsReconciler.SetupWithManager(mgr); err != nil {
		panic(err)
	}

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
