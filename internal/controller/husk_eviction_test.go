package controller_test

// Coverage for husk pod eviction, disruption, and drain (issue #18, slice 4b):
//
//   - ensureHuskPDB creates a policy/v1 PodDisruptionBudget owned by the pool,
//     selecting the pool's husk pods, with the documented minAvailable
//     (max(1, Replicas-1)); a second call updates minAvailable in place.
//   - the warm pool SELF-HEALS: deleting a husk pod (the manager's Owns(pods)
//     edge enqueues the pool) makes reconcileHuskPods recreate the replacement
//     so the count returns to Replicas. Here the self-heal is driven directly
//     (the suite's manager pool reconciler runs raw mode) to keep the assertion
//     deterministic and independent of the manager wiring.
//   - a claim whose husk pod is deleted RE-PENDS (Phase Pending, Endpoint/Node
//     cleared, recordClaimPending) and re-activates on a replacement dormant pod
//     on the next reconcile (the fake activator).
//   - DrainPolicy defaults to Kill (empty), and a Checkpoint pool routes through
//     the checkpointer seam (a fake records the call) before re-pending.

import (
	"context"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/husk"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEnsureHuskPDB(t *testing.T) {
	c := k8sClient

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "pdb-tmpl"},
			Replicas:    3,
		},
	}
	for _, obj := range []client.Object{template, pool} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = c.Delete(ctx, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "pdb-pool-husk", Namespace: "default"}})
		_ = c.Delete(ctx, pool)
		_ = c.Delete(ctx, template)
	})

	// Re-fetch so SetControllerReference has the server UID.
	var live v1alpha1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}

	r := &controller.SandboxPoolReconciler{Client: c, NodeRegistry: controller.NewNodeRegistry()}
	if err := r.EnsureHuskPDBForTest(ctx, &live); err != nil {
		t.Fatalf("ensureHuskPDB: %v", err)
	}

	var pdb policyv1.PodDisruptionBudget
	if err := c.Get(ctx, types.NamespacedName{Name: "pdb-pool-husk", Namespace: "default"}, &pdb); err != nil {
		t.Fatalf("get PDB: %v", err)
	}

	// Owner-ref to the pool.
	owner := metav1.GetControllerOf(&pdb)
	if owner == nil || owner.Kind != "SandboxPool" || owner.Name != "pdb-pool" {
		t.Fatalf("PDB owner = %+v, want SandboxPool pdb-pool", owner)
	}
	// Selector matches the husk pods.
	if pdb.Spec.Selector == nil {
		t.Fatal("PDB has no selector")
	}
	if pdb.Spec.Selector.MatchLabels["mitos.run/pool"] != "pdb-pool" {
		t.Errorf("PDB pool selector = %q, want pdb-pool", pdb.Spec.Selector.MatchLabels["mitos.run/pool"])
	}
	if pdb.Spec.Selector.MatchLabels["mitos.run/husk"] != "true" {
		t.Errorf("PDB husk selector = %q, want true", pdb.Spec.Selector.MatchLabels["mitos.run/husk"])
	}
	// minAvailable = max(1, Replicas-1) = 2 for Replicas=3.
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Fatalf("PDB minAvailable = %v, want 2 (max(1, Replicas-1) for 3 replicas)", pdb.Spec.MinAvailable)
	}

	// Idempotent update: scale to 1 and re-ensure; minAvailable floors at 1.
	live.Spec.Replicas = 1
	if err := r.EnsureHuskPDBForTest(ctx, &live); err != nil {
		t.Fatalf("ensureHuskPDB (update): %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "pdb-pool-husk", Namespace: "default"}, &pdb); err != nil {
		t.Fatal(err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Fatalf("PDB minAvailable after scale to 1 = %v, want 1 (floor)", pdb.Spec.MinAvailable)
	}
}

func TestHuskPoolSelfHealsDeletedPod(t *testing.T) {
	c := k8sClient

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "heal-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "heal-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "heal-tmpl"},
			Replicas:    2,
		},
	}
	for _, obj := range []client.Object{template, pool} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, "heal-pool") {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
		_ = c.Delete(ctx, template)
	})

	var live v1alpha1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}

	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    controller.NewNodeRegistry(),
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}

	// Bring the warm pool up to Replicas=2.
	if _, err := r.ReconcileHuskPodsForTest(ctx, &live, template); err != nil {
		t.Fatalf("reconcileHuskPods (initial): %v", err)
	}
	pods := waitHuskPodCount(t, c, "heal-pool", 2)

	// Record a victim name, delete it (a drain/eviction in production deletes the
	// pod and Owns(pods) enqueues the pool; here we drive the reconcile directly).
	victim := pods[0]
	if err := c.Delete(ctx, &victim); err != nil {
		t.Fatalf("delete husk pod: %v", err)
	}
	// Wait for the delete to be observed (envtest has no kubelet, so a deleted
	// pod with no finalizer disappears immediately).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(listHuskPods(t, c, "heal-pool")) < 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Self-heal: a reconcile recreates the replacement, count back to Replicas.
	if _, err := r.ReconcileHuskPodsForTest(ctx, &live, template); err != nil {
		t.Fatalf("reconcileHuskPods (self-heal): %v", err)
	}
	healed := waitHuskPodCount(t, c, "heal-pool", 2)

	// A NEW pod (a name different from the victim) is present and owned by the pool.
	foundNew := false
	for _, p := range healed {
		if p.Name != victim.Name {
			foundNew = true
		}
		owner := metav1.GetControllerOf(&p)
		if owner == nil || owner.Name != "heal-pool" {
			t.Fatalf("healed husk pod %s owner = %+v, want SandboxPool heal-pool", p.Name, owner)
		}
	}
	if !foundNew {
		t.Fatalf("self-heal produced no new pod (still only the victim %s)", victim.Name)
	}
}

// TestHuskWarmPoolDecoupledFromSnapshotBuild proves the warm pool of husk pods
// is maintained to Replicas and self-heals a deleted pod EVEN WHEN the template
// snapshot is never built. This is exactly the kind-e2e failure mode: on kind
// forkd cannot boot a VM to snapshot (the nested-VMM boundary), so the build
// never produces a ready snapshot, yet the warm pool of dormant pods must still
// exist and self-heal. Here the registry has NO forkd node, so ensureTemplateBuilt
// can never register a holder (readySnapshotCount stays 0). The full Reconcile is
// driven (not just reconcileHuskPods) so the build-vs-warm-pool decoupling in
// Reconcile itself is under test: a build that produces no ready snapshot must not
// short-circuit warm-pool maintenance.
func TestHuskWarmPoolDecoupledFromSnapshotBuild(t *testing.T) {
	c := k8sClient

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "nobuild-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "nobuild-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "nobuild-tmpl"},
			Replicas:    2,
		},
	}
	for _, obj := range []client.Object{template, pool} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, "nobuild-pool") {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "nobuild-pool-husk", Namespace: "default"}})
		_ = c.Delete(ctx, pool)
		_ = c.Delete(ctx, template)
	})

	// An empty registry: no forkd node holds or can build the template, so
	// readySnapshotCount stays 0 for the whole test (the kind nested-VMM boundary).
	reg := controller.NewNodeRegistry()
	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    reg,
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}

	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}

	// The suite registers a manager-level raw-mode pool reconciler that also
	// reconciles this pool and writes its status, so a direct full Reconcile can
	// race it on the status subresource (a benign optimistic-lock conflict). The
	// reconcileReady helper drives the husk Reconcile and tolerates that conflict;
	// the WARM-POOL effect (husk pod creation) is conflict-free, so it lands
	// regardless. This is the realistic behavior: controller-runtime requeues a
	// conflicting reconcile and the next pass converges.
	reconcileOnce := func(stage string) ctrl.Result {
		res, err := r.Reconcile(ctx, req)
		if err != nil && !apierrors.IsConflict(err) {
			t.Fatalf("reconcile (%s): %v", stage, err)
		}
		return res
	}

	// First full reconcile: the snapshot is NOT built (no holder), yet the warm
	// pool must come up to Replicas=2.
	reconcileOnce("initial")
	pods := waitHuskPodCount(t, c, "nobuild-pool", 2)

	// Sanity: the snapshot really is not built (no holder), so this asserts the
	// warm pool was maintained INDEPENDENT of the build.
	if holders := reg.NodesWithTemplate("nobuild-tmpl"); len(holders) != 0 {
		t.Fatalf("expected no snapshot holders (decoupling test), got %v", holders)
	}

	// Self-heal: delete a husk pod and reconcile again. The replacement must be
	// recreated to Replicas even though the snapshot is STILL not built.
	victim := pods[0]
	if err := c.Delete(ctx, &victim); err != nil {
		t.Fatalf("delete husk pod: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(listHuskPods(t, c, "nobuild-pool")) < 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	reconcileOnce("self-heal")
	healed := waitHuskPodCount(t, c, "nobuild-pool", 2)

	foundNew := false
	for _, p := range healed {
		if p.Name != victim.Name {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatalf("self-heal produced no new pod (still only the victim %s); warm pool did not recover with the snapshot unbuilt", victim.Name)
	}
}

// activeHuskClaim creates a template, pool (with the given drain policy), a
// dormant husk pod, drives the husk-test claim to Ready (active on the pod), and
// returns the claim and the activated pod.
func activeHuskClaim(t *testing.T, prefix string, drain v1alpha1.HuskDrainPolicy) (*v1alpha1.SandboxClaim, *corev1.Pod) {
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
			DrainPolicy: drain,
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

	pod := makeDormantHuskPod(t, prefix+"-pool", "10.7.7.7")

	act := &fakeActivator{result: husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.0}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefix + "-claim",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskTestClaimLabel: "true"},
		},
		Spec: v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: prefix + "-pool"}},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })

	got := waitClaimPhase(t, claim.Name, func(cl *v1alpha1.SandboxClaim) bool {
		return cl.Status.Phase == v1alpha1.SandboxReady
	})
	return got, pod
}

func TestHuskClaimRePendsOnPodLossKill(t *testing.T) {
	// Default (Kill) drain policy via an empty DrainPolicy.
	claim, pod := activeHuskClaim(t, "repend-kill", "")
	if claim.Status.Endpoint == "" || claim.Status.Node == "" {
		t.Fatalf("claim not active: %+v", claim.Status)
	}

	// Delete the backing husk pod: the Watches mapping enqueues the claim, which
	// re-pends. Also remove the claim label so a replacement dormant pod can be
	// activated cleanly.
	if err := k8sClient.Delete(ctx, pod); err != nil {
		t.Fatalf("delete husk pod: %v", err)
	}

	// The claim re-pends: Phase Pending, Endpoint/Node cleared.
	got := waitClaimPhase(t, claim.Name, func(cl *v1alpha1.SandboxClaim) bool {
		return cl.Status.Phase == v1alpha1.SandboxPending
	})
	if got.Status.Endpoint != "" {
		t.Errorf("re-pended claim still has endpoint %q", got.Status.Endpoint)
	}
	if got.Status.Node != "" {
		t.Errorf("re-pended claim still has node %q", got.Status.Node)
	}

	// Provide a replacement dormant pod: the next reconcile re-activates on it.
	replacement := makeDormantHuskPod(t, "repend-kill-pool", "10.8.8.8")
	reactivated := waitClaimPhase(t, claim.Name, func(cl *v1alpha1.SandboxClaim) bool {
		return cl.Status.Phase == v1alpha1.SandboxReady
	})
	if reactivated.Status.Endpoint != "10.8.8.8:9091" {
		t.Errorf("re-activated endpoint = %q, want 10.8.8.8:9091 (the replacement pod)", reactivated.Status.Endpoint)
	}
	_ = replacement
}

func TestHuskClaimDrainCheckpointRoutesThroughSeam(t *testing.T) {
	claim, pod := activeHuskClaim(t, "repend-ckpt", v1alpha1.DrainCheckpoint)
	if claim.Status.Phase != v1alpha1.SandboxReady {
		t.Fatalf("claim not Ready: %+v", claim.Status)
	}

	// Record the checkpointer call. Return ok=false (nothing captured) so the
	// claim still re-pends after routing through the seam.
	called := make(chan struct{}, 4)
	setHuskTestCheckpointer(func(_ context.Context, _ *v1alpha1.SandboxClaim, _ *corev1.Pod) (bool, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		return false, nil
	})
	t.Cleanup(func() { setHuskTestCheckpointer(nil) })

	if err := k8sClient.Delete(ctx, pod); err != nil {
		t.Fatalf("delete husk pod: %v", err)
	}

	// The claim re-pends (Checkpoint degrades to re-pend when nothing is captured).
	waitClaimPhase(t, claim.Name, func(cl *v1alpha1.SandboxClaim) bool {
		return cl.Status.Phase == v1alpha1.SandboxPending
	})

	select {
	case <-called:
		// The Checkpoint drain policy routed through the checkpointer seam.
	case <-time.After(10 * time.Second):
		t.Fatal("Checkpoint drain policy did not route through the checkpointer seam")
	}
}
