package controller_test

// Regression guard for the kind-e2e-husk re-pend probe ("did not settle Ready on
// the stamped backing pod"). The probe stamps a husk claim Ready with a merge
// patch and waits for it to settle. Settling failed because a husk claim that
// cannot place (its pool has no dormant pod: NoHuskPod) was hot-looping: each
// reconcile re-asserted the SAME NoHuskPod condition with a FRESH
// LastTransitionTime, which made the Status().Update a real change, which
// re-triggered the claim's own watch, which re-reconciled, ad infinitum. That
// loop of full-object status writes from a stale read clobbered the e2e's
// externally merge-patched Ready stamp, so the claim never settled.
//
// The fix makes setCondition preserve LastTransitionTime when the Status is
// unchanged, so re-asserting an unchanged condition is a no-op write and the loop
// terminates. These tests pin both halves: a pending NoHuskPod claim does not
// churn (no hot loop), and an externally-stamped Ready husk claim with a healthy
// backing pod stays Ready across reconciles.

import (
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestHuskNoPodClaimDoesNotChurn pins the root cause: a husk claim whose pool has
// no dormant pod pends with NoHuskPod and must SETTLE (a stable resourceVersion),
// not hot-loop. A hot loop here is what clobbered the e2e's Ready stamp. The claim
// references an empty pool (replicas 0), exactly the kind-e2e-husk re-pend probe
// shape, so the controller can never select a pod for it and always takes the
// NoHuskPod pend path.
func TestHuskNoPodClaimDoesNotChurn(t *testing.T) {
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "churn-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "churn-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "churn-tmpl"},
			Replicas:    0,
		},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "churn-claim",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskTestClaimLabel: "true"},
		},
		Spec: v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "churn-pool"}},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	// Wait for the claim to reach Pending (NoHuskPod).
	waitClaimPhase(t, claim.Name, func(cl *v1alpha1.SandboxClaim) bool {
		return cl.Status.Phase == v1alpha1.SandboxPending
	})

	// Let it settle, then record the resourceVersion and confirm it does NOT keep
	// advancing: a stable resourceVersion across a sustained window means the
	// reconcile is not churning the object (no hot loop). The capacity-pending
	// requeue is 5s, so a healthy reconciler writes nothing in between; a churning
	// one bumps the resourceVersion many times per second.
	time.Sleep(2 * time.Second)
	var first v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: claim.Name, Namespace: "default"}, &first); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	rv := first.ResourceVersion
	bumps := 0
	for i := 0; i < 20; i++ {
		time.Sleep(150 * time.Millisecond)
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: claim.Name, Namespace: "default"}, &got); err != nil {
			t.Fatalf("get claim: %v", err)
		}
		if got.ResourceVersion != rv {
			bumps++
			rv = got.ResourceVersion
		}
	}
	// One or two bumps from the periodic 5s requeue are tolerable; a hot loop
	// produces dozens. Allow a small margin, fail on a churn.
	if bumps > 2 {
		t.Fatalf("pending NoHuskPod claim churned its status %d times over ~3s (hot loop: each reconcile re-stamped an unchanged condition with a fresh timestamp); this clobbers an external Ready stamp and is the kind-e2e-husk settle regression", bumps)
	}
}

// TestHuskClaimStampedReadyStaysReady mirrors the kind-e2e-husk re-pend probe
// setup: an empty pool (replicas 0) for the claim so the controller can never
// pick a dormant pod for it, a borrowed dormant husk pod from a different pool
// labeled with the claim, and the claim status stamped Ready on that pod via a
// server-side merge patch (the Go analog of `kubectl patch
// --subresource=status --type=merge`). The controller must leave the Ready claim
// Ready (its backing pod is healthy) and must not flap it to Restoring/Pending.
func TestHuskClaimStampedReadyStaysReady(t *testing.T) {
	emptyTemplate := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "stamp-empty-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	emptyPool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "stamp-empty-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "stamp-empty-tmpl"},
			Replicas:    0,
		},
	}
	if err := k8sClient.Create(ctx, emptyTemplate); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, emptyPool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, emptyPool)
		_ = k8sClient.Delete(ctx, emptyTemplate)
	})

	// A dormant pod in a DIFFERENT pool, borrowed to back the claim. It is labeled
	// with the claim so checkHuskPodLost keys off it (pool independent).
	backing := makeDormantHuskPod(t, "stamp-other-pool", "10.9.9.9")

	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stamp-ready-claim",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskTestClaimLabel: "true"},
		},
		Spec: v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "stamp-empty-pool"}},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })

	// Borrow the pod (object-level claim commit) then stamp the claim status Ready
	// on it with a server-side merge patch that always lands on the latest object.
	backing.Labels["mitos.run/claim"] = claim.Name
	if err := k8sClient.Update(ctx, backing); err != nil {
		t.Fatal(err)
	}
	stamp := []byte(fmt.Sprintf(
		`{"status":{"phase":%q,"node":%q,"endpoint":%q,"sandboxID":%q}}`,
		v1alpha1.SandboxReady, backing.Spec.NodeName, backing.Status.PodIP+":9091", backing.Name,
	))
	if err := k8sClient.Status().Patch(ctx, claim, client.RawPatch(types.MergePatchType, stamp)); err != nil {
		t.Fatalf("stamp claim Ready: %v", err)
	}

	// The claim must settle Ready on the stamped pod and HOLD it for a sustained
	// window: the reconcile must not flap it off Ready while the backing pod is
	// healthy.
	stableSince := time.Time{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: claim.Name, Namespace: "default"}, &got); err != nil {
			t.Fatalf("get claim: %v", err)
		}
		switch got.Status.Phase {
		case v1alpha1.SandboxReady:
			if got.Status.SandboxID != backing.Name {
				t.Fatalf("Ready claim points at sandboxID %q, want the stamped pod %q", got.Status.SandboxID, backing.Name)
			}
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
			if time.Since(stableSince) >= 3*time.Second {
				return // Held Ready for a sustained window: no flap.
			}
		default:
			t.Fatalf("externally-stamped Ready husk claim flapped to phase %q (regression: reconcile destabilized a Ready husk claim with a healthy backing pod)", got.Status.Phase)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim never held Ready for a sustained window")
}
