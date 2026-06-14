package controller_test

// Envtest coverage for the W4 husk-mode workspace transport DELEGATION.
//
// The controller is not on the node and cannot reach the guest vsock or the node
// CAS, so in husk mode it DELEGATES the hydrate/dehydrate of a claim's /workspace
// to the husk-stub control op (the node component that owns both). These tests
// prove the husk claim reconciler's default hydrate/dehydrate path routes through
// the delegate, and that the controller still owns the WorkspaceRevision commit +
// head advance once the delegate returns the manifest digest:
//   - on terminate, the dehydrate delegate is called (with the secret exclude
//     list), returns a digest, a committed WorkspaceRevision with fromClaim
//     lineage is created, and the workspace head advances to it;
//   - on a fresh claim bound to the head, the hydrate delegate is called with the
//     head's manifest.

import (
	"context"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/husk"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// huskWSDelegateRecorder records what the husk-mode hydrate/dehydrate delegates
// were asked to do, so a test can assert the husk reconciler delegated with the
// right manifest / excludes.
type huskWSDelegateRecorder struct {
	mu              sync.Mutex
	hydratedDigest  cas.Digest
	hydrateCalls    int
	dehydrateCalls  int
	excludesPassed  []string
	dehydrateDigest cas.Digest
}

func (r *huskWSDelegateRecorder) install(t *testing.T, dehydrateDigest cas.Digest) {
	r.dehydrateDigest = dehydrateDigest
	setHuskWSDelegate(
		func(_ context.Context, _ *v1alpha1.SandboxClaim, manifest cas.Digest) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.hydrateCalls++
			r.hydratedDigest = manifest
			return nil
		},
		func(_ context.Context, _ *v1alpha1.SandboxClaim, excludePaths, _ []string) (cas.Digest, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.dehydrateCalls++
			r.excludesPassed = append([]string(nil), excludePaths...)
			return r.dehydrateDigest, nil
		},
	)
	t.Cleanup(func() { setHuskWSDelegate(nil, nil) })
}

func (r *huskWSDelegateRecorder) snapshot() (hydrateCalls int, hydrated cas.Digest, dehydrateCalls int, excludes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hydrateCalls, r.hydratedDigest, r.dehydrateCalls, append([]string(nil), r.excludesPassed...)
}

// makeHuskWorkspaceClaim creates a template, pool, dormant husk pod, and a
// husk-test-labeled claim bound to wsName, and returns the claim.
func makeHuskWorkspaceClaim(t *testing.T, prefix, wsName, podIP string, spec v1alpha1.SandboxClaimSpec) *v1alpha1.SandboxClaim {
	t.Helper()
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: prefix + "-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})
	makeDormantHuskPod(t, prefix+"-pool", podIP)

	spec.PoolRef = v1alpha1.LocalObjectReference{Name: prefix + "-pool"}
	spec.WorkspaceRef = &v1alpha1.LocalObjectReference{Name: wsName}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefix + "-claim",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskTestClaimLabel: "true"},
		},
		Spec: spec,
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })
	return claim
}

func TestHuskClaimWorkspaceDehydrateDelegates(t *testing.T) {
	rec := &huskWSDelegateRecorder{}
	revDigest := cas.Digest(testManifest(0x5a))
	rec.install(t, revDigest)

	act := &fakeActivator{result: husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.0}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	makeWorkspace(t, "hw-dehydrate-ws", v1alpha1.WorkspaceRetention{})

	claim := makeHuskWorkspaceClaim(t, "hw-de", "hw-dehydrate-ws", "10.20.0.1", v1alpha1.SandboxClaimSpec{
		Timeout: &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, claim.Name, v1alpha1.SandboxReady)

	// Lifetime expiry terminates the claim, which delegates the dehydrate.
	waitBoundPhase(t, claim.Name, v1alpha1.SandboxTerminated)

	// The dehydrate delegate must have been invoked with the secret exclude list.
	deadline := time.Now().Add(10 * time.Second)
	var excludes []string
	for time.Now().Before(deadline) {
		_, _, dcalls, ex := rec.snapshot()
		if dcalls >= 1 {
			excludes = ex
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if len(excludes) == 0 {
		t.Fatal("husk dehydrate delegate was not invoked with an exclude list")
	}
	if !containsExclude(excludes, "/workspace/.netrc") {
		t.Fatalf("husk dehydrate exclude list %v missing the secret paths", excludes)
	}

	// The controller still owns the commit + head advance: a committed
	// WorkspaceRevision with fromClaim lineage exists and the head advanced to the
	// digest the delegate returned.
	ws := waitWorkspace(t, "hw-dehydrate-ws", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != "" && ws.Status.Revisions >= 1
	}, "head advanced after husk dehydrate")

	var head v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: ws.Status.Head}, &head); err != nil {
		t.Fatalf("get head revision: %v", err)
	}
	if head.Spec.Source.FromClaim != claim.Name {
		t.Fatalf("head revision fromClaim = %q, want %q", head.Spec.Source.FromClaim, claim.Name)
	}
	if head.Spec.ContentManifest != string(revDigest) {
		t.Fatalf("head revision contentManifest = %q, want the husk dehydrate digest %q", head.Spec.ContentManifest, revDigest)
	}
}

func TestHuskClaimWorkspaceHydrateDelegates(t *testing.T) {
	rec := &huskWSDelegateRecorder{}
	rec.install(t, cas.Digest(testManifest(0x6b)))

	act := &fakeActivator{result: husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.0}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	headManifest := testManifest(0x7c)
	makeWorkspace(t, "hw-hydrate-ws", v1alpha1.WorkspaceRetention{})
	makeRevision(t, "hw-hydrate-ws-r1", "hw-hydrate-ws", headManifest, nil, nil)
	waitWorkspace(t, "hw-hydrate-ws", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head == "hw-hydrate-ws-r1"
	}, "head committed")

	claim := makeHuskWorkspaceClaim(t, "hw-hy", "hw-hydrate-ws", "10.20.0.2", v1alpha1.SandboxClaimSpec{})
	waitBoundPhase(t, claim.Name, v1alpha1.SandboxReady)

	// The hydrate delegate must have been invoked with the head's manifest.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		calls, hydrated, _, _ := rec.snapshot()
		if calls >= 1 {
			if string(hydrated) != headManifest {
				t.Fatalf("husk hydrate delegate invoked with manifest %q, want head %q", hydrated, headManifest)
			}
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("husk hydrate delegate was not invoked for the bound claim within 10s")
}
