package controller_test

// Envtest coverage for binding a SandboxClaim to a Workspace (W4 slice 2).
//
// The suite's raw claim reconciler routes the workspace hydrate/dehydrate seams
// through per-test swappable fakes (setWSTransfer). These tests assert:
//   - a claim with workspaceRef + a workspace-with-head hydrates the head
//     manifest into the sandbox on activate;
//   - terminate dehydrates and creates a new committed WorkspaceRevision with
//     fromClaim lineage, and the workspace head advances to it;
//   - the dehydrate is passed the secret exclude list;
//   - a second claim on a busy workspace pends with reason WorkspaceBusy;
//   - a claim without workspaceRef is unaffected (no hydrate/dehydrate).

import (
	"context"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/cas"
	"github.com/paperclipinc/sandbox/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// wsRecorder records what the hydrate/dehydrate fakes were asked to do, so a
// test can assert the seam was invoked with the right manifest and excludes.
type wsRecorder struct {
	mu              sync.Mutex
	hydratedDigest  cas.Digest
	hydrateCalls    int
	dehydrateCalls  int
	excludesPassed  []string
	dehydrateDigest cas.Digest
}

func (r *wsRecorder) install(t *testing.T, dehydrateDigest cas.Digest) {
	r.dehydrateDigest = dehydrateDigest
	setWSTransfer(
		func(_ context.Context, _ *v1alpha1.SandboxClaim, manifest cas.Digest) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.hydrateCalls++
			r.hydratedDigest = manifest
			return nil
		},
		func(_ context.Context, _ *v1alpha1.SandboxClaim, excludePaths []string) (cas.Digest, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.dehydrateCalls++
			r.excludesPassed = append([]string(nil), excludePaths...)
			return r.dehydrateDigest, nil
		},
	)
	t.Cleanup(func() { setWSTransfer(nil, nil) })
}

func (r *wsRecorder) snapshot() (hydrateCalls int, hydrated cas.Digest, dehydrateCalls int, excludes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hydrateCalls, r.hydratedDigest, r.dehydrateCalls, append([]string(nil), r.excludesPassed...)
}

// makeBoundClaim creates a template, pool, and claim bound to wsName.
func makeBoundClaim(t *testing.T, prefix, wsName string, spec v1alpha1.SandboxClaimSpec) *v1alpha1.SandboxClaim {
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
	spec.PoolRef = v1alpha1.LocalObjectReference{Name: prefix + "-pool"}
	if wsName != "" {
		spec.WorkspaceRef = &v1alpha1.LocalObjectReference{Name: wsName}
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-claim", Namespace: "default"},
		Spec:       spec,
	}
	for _, obj := range []client.Object{template, pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})
	return claim
}

// waitBoundPhase waits until the named claim reaches the given phase, reusing the
// shared predicate-based waitClaimPhase helper.
func waitBoundPhase(t *testing.T, name string, phase v1alpha1.SandboxPhase) *v1alpha1.SandboxClaim {
	t.Helper()
	return waitClaimPhase(t, name, func(c *v1alpha1.SandboxClaim) bool {
		return c.Status.Phase == phase
	})
}

func claimReadyReason(t *testing.T, name string) (phase v1alpha1.SandboxPhase, reason string) {
	t.Helper()
	var got v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" {
			return got.Status.Phase, c.Reason
		}
	}
	return got.Status.Phase, ""
}

func TestClaimWorkspaceHydrateOnActivate(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0xaa)))

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-hydrate-node", "wsh-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	headManifest := testManifest(0xbe)
	makeWorkspace(t, "ws-bind-hydrate", v1alpha1.WorkspaceRetention{})
	makeRevision(t, "ws-bind-hydrate-r1", "ws-bind-hydrate", headManifest, nil, nil)
	// Wait for the workspace head to converge to the committed revision.
	waitWorkspace(t, "ws-bind-hydrate", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head == "ws-bind-hydrate-r1"
	}, "head committed")

	makeBoundClaim(t, "wsh", "ws-bind-hydrate", v1alpha1.SandboxClaimSpec{NodeName: "ws-hydrate-node"})
	waitBoundPhase(t, "wsh-claim", v1alpha1.SandboxReady)

	// The hydrate seam must have been invoked with the head's manifest.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		calls, hydrated, _, _ := rec.snapshot()
		if calls >= 1 {
			if string(hydrated) != headManifest {
				t.Fatalf("hydrate invoked with manifest %q, want head %q", hydrated, headManifest)
			}
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("hydrate seam was not invoked for the bound claim within 10s")
}

func TestClaimWorkspaceDehydrateOnTerminate(t *testing.T) {
	rec := &wsRecorder{}
	revDigest := cas.Digest(testManifest(0xcd))
	rec.install(t, revDigest)

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-dehydrate-node", "wsd-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, "ws-bind-dehydrate", v1alpha1.WorkspaceRetention{})
	// No head yet: the claim starts empty, then dehydrate creates the first
	// committed revision on terminate.

	claim := makeBoundClaim(t, "wsd", "ws-bind-dehydrate", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-dehydrate-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, "wsd-claim", v1alpha1.SandboxReady)

	// Lifetime expiry terminates the claim, which dehydrates first.
	waitBoundPhase(t, "wsd-claim", v1alpha1.SandboxTerminated)

	// A dehydrate must have run with the secret exclude list.
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
		t.Fatal("dehydrate seam was not invoked with an exclude list")
	}
	if !containsExclude(excludes, "/workspace/.netrc") {
		t.Fatalf("dehydrate exclude list %v missing the secret paths", excludes)
	}

	// A new committed WorkspaceRevision with fromClaim lineage must exist and the
	// workspace head must advance to it.
	ws := waitWorkspace(t, "ws-bind-dehydrate", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != "" && ws.Status.Revisions >= 1
	}, "head advanced after dehydrate")

	var head v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: ws.Status.Head}, &head); err != nil {
		t.Fatalf("get head revision: %v", err)
	}
	if head.Spec.Source.FromClaim != claim.Name {
		t.Fatalf("head revision fromClaim = %q, want %q", head.Spec.Source.FromClaim, claim.Name)
	}
	if head.Spec.ContentManifest != string(revDigest) {
		t.Fatalf("head revision contentManifest = %q, want the dehydrate digest %q", head.Spec.ContentManifest, revDigest)
	}
}

func TestClaimWorkspaceSingleWriterBusy(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0xef)))

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-busy-node", "wsb-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, "ws-busy", v1alpha1.WorkspaceRetention{})

	// First claim binds and goes Ready.
	makeBoundClaim(t, "wsb1", "ws-busy", v1alpha1.SandboxClaimSpec{NodeName: "ws-busy-node"})
	waitBoundPhase(t, "wsb1-claim", v1alpha1.SandboxReady)

	// Second claim on the same workspace must pend with WorkspaceBusy.
	makeBoundClaim(t, "wsb2", "ws-busy", v1alpha1.SandboxClaimSpec{NodeName: "ws-busy-node"})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		phase, reason := claimReadyReason(t, "wsb2-claim")
		if phase == v1alpha1.SandboxPending && reason == "WorkspaceBusy" {
			return
		}
		if phase == v1alpha1.SandboxReady {
			t.Fatal("second claim on a busy workspace went Ready; single-writer not enforced")
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("second claim did not pend with WorkspaceBusy within 15s")
}

func TestClaimWithoutWorkspaceRefUnaffected(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0x11)))

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-none-node", "wsn-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeBoundClaim(t, "wsn", "", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-none-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, "wsn-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "wsn-claim", v1alpha1.SandboxTerminated)

	// Neither hydrate nor dehydrate may have been invoked for an unbound claim.
	hcalls, _, dcalls, _ := rec.snapshot()
	if hcalls != 0 || dcalls != 0 {
		t.Fatalf("unbound claim invoked the transfer seams: hydrate=%d dehydrate=%d", hcalls, dcalls)
	}
}

func containsExclude(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
