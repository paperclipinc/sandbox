package controller_test

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	controller "github.com/paperclipinc/mitos/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWarmPoolAutoscaleUpDown(t *testing.T) {
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "t-autoscale", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatalf("create template: %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, template) }()

	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p-autoscale", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "t-autoscale"},
			Replicas:    1,
			Autoscale: &v1alpha1.PoolAutoscaleSpec{
				MinWarm: 1, MaxWarm: 8, TargetSpare: 2, ScaleDownCooldownSeconds: 60,
			},
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, pool) }()
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, pool); err != nil {
		t.Fatalf("get pool for UID: %v", err)
	}

	// Frozen clock far in the future so the cooldown is satisfiable on demand by
	// moving the clock; demand shares one instance with a (here unused) claim path.
	frozen := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	demand := controller.NewPoolDemand()
	r := &controller.SandboxPoolReconciler{
		Client:         k8sClient,
		NodeRegistry:   controller.NewNodeRegistry(), // real, empty registry: readySnapshotCount==0, build best-effort and skipped on no node
		EnableHuskPods: true,
		HuskStubImage:  "stub:latest",
		Demand:         demand,
		Now:            func() time.Time { return frozen },
	}

	// The suite manager runs its own SandboxPool reconciler (EnableHuskPods=false)
	// that also watches this pool and writes its status, so a status conflict is
	// expected and benign here; retry the manually-driven reconcile on conflict so
	// the test is deterministic. The husk-pod create/delete this reconcile drives
	// is unaffected by that race (only the status subresource conflicts).
	reconcile := func() {
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}}
		var err error
		for i := 0; i < 10; i++ {
			if _, err = r.Reconcile(ctx, req); err == nil {
				return
			}
			if !apierrors.IsConflict(err) {
				t.Fatalf("reconcile: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("reconcile still conflicting after retries: %v", err)
	}

	countDormant := func() int {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.InNamespace("default"),
			client.MatchingLabels{"mitos.run/pool": pool.Name, "mitos.run/husk": "true"},
		); err != nil {
			t.Fatalf("list pods: %v", err)
		}
		n := 0
		for i := range pods.Items {
			p := pods.Items[i]
			if p.DeletionTimestamp != nil {
				continue
			}
			if _, claimed := p.Labels["mitos.run/claim"]; claimed {
				continue
			}
			n++
		}
		return n
	}

	// Phase 1: no in-use, no demand. desired = clamp(0+2,1,8) = 2. Expect 2 dormant.
	reconcile()
	if got := countDormant(); got != 2 {
		t.Fatalf("phase 1 dormant = %d, want 2", got)
	}

	// Phase 2: a burst arrives. Mark 2 existing dormant as in-use and reconcile to
	// create the spare on top: desired = clamp(2+2,1,8)=4, so 4 dormant + 2 in-use.
	demand.RecordArrival("default/"+pool.Name, frozen)
	markClaimed := func(n int) {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, client.InNamespace("default"),
			client.MatchingLabels{"mitos.run/pool": pool.Name, "mitos.run/husk": "true"})
		marked := 0
		for i := range pods.Items {
			if marked >= n {
				break
			}
			p := &pods.Items[i]
			if _, claimed := p.Labels["mitos.run/claim"]; claimed {
				continue
			}
			if p.Labels == nil {
				p.Labels = map[string]string{}
			}
			p.Labels["mitos.run/claim"] = "claim-sim"
			if err := k8sClient.Update(ctx, p); err != nil {
				t.Fatalf("mark claimed: %v", err)
			}
			marked++
		}
	}
	markClaimed(2) // 2 in-use, 0 dormant remain
	reconcile()    // desired = clamp(2+2,1,8)=4 -> create 4 dormant
	if got := countDormant(); got != 4 {
		t.Fatalf("phase 2 dormant = %d, want 4 (burst refilled the spare)", got)
	}

	// Phase 3: idle DOWN. Release the in-use pods (delete them: an active claim
	// ending deletes its husk pod), advance the clock past the cooldown with no new
	// demand. desired = clamp(0+2,1,8)=2 and the cooldown has elapsed, so the pool
	// scales DOWN from 4 to 2.
	var pods corev1.PodList
	_ = k8sClient.List(ctx, &pods, client.InNamespace("default"),
		client.MatchingLabels{"mitos.run/pool": pool.Name, "mitos.run/husk": "true", "mitos.run/claim": "claim-sim"})
	for i := range pods.Items {
		_ = k8sClient.Delete(ctx, &pods.Items[i])
	}
	r.Now = func() time.Time { return frozen.Add(2 * time.Minute) } // past the 60 s cooldown
	reconcile()
	if got := countDormant(); got != 2 {
		t.Fatalf("phase 3 dormant = %d, want 2 (idle scale-down to spare floor)", got)
	}

	// Status reflects the desired count.
	var fresh v1alpha1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, &fresh); err != nil {
		t.Fatalf("get pool status: %v", err)
	}
	if fresh.Status.DesiredWarm != 2 {
		t.Fatalf("Status.DesiredWarm = %d, want 2", fresh.Status.DesiredWarm)
	}
	if fresh.Status.LastScaleDownTime == nil {
		t.Fatal("Status.LastScaleDownTime should be set after a scale-down")
	}

	// Phase 4: a claimed/in-use husk pod MUST survive a scale-down whose desired
	// drops below inUse. This locks in the safety property that reconcileHuskPods
	// only ever deletes SURPLUS DORMANT pods, never a claimed one. We are at 2
	// dormant, 0 in-use here. Mark BOTH dormant pods claimed so inUse=2, dormant=0,
	// then reconcile to refill the spare (desired = clamp(2+2,1,8)=4 -> 4 dormant).
	demand.RecordArrival("default/"+pool.Name, frozen)
	r.Now = func() time.Time { return frozen }
	markClaimed(2)
	reconcile()
	if got := countDormant(); got != 4 {
		t.Fatalf("phase 4 setup dormant = %d, want 4", got)
	}

	// Capture the names of the two claimed pods so we can prove they still exist
	// after the scale-down (not merely that the claimed COUNT held).
	claimedNames := func() map[string]bool {
		var cps corev1.PodList
		if err := k8sClient.List(ctx, &cps, client.InNamespace("default"),
			client.MatchingLabels{"mitos.run/pool": pool.Name, "mitos.run/husk": "true", "mitos.run/claim": "claim-sim"}); err != nil {
			t.Fatalf("list claimed pods: %v", err)
		}
		names := map[string]bool{}
		for i := range cps.Items {
			if cps.Items[i].DeletionTimestamp != nil {
				continue
			}
			names[cps.Items[i].Name] = true
		}
		return names
	}
	before := claimedNames()
	if len(before) != 2 {
		t.Fatalf("phase 4 expected 2 claimed pods before scale-down, got %d", len(before))
	}

	// Drive an idle+cooldown scale-down to a desired BELOW inUse: clear demand and
	// advance past the cooldown so desired = clamp(inUse(2)+spare? no, idle) ... the
	// floor is MinWarm=1, so desired = clamp(2+2,1,8)=4 still includes inUse. To get
	// desired strictly below inUse we shrink the autoscale window so the ceiling
	// MaxWarm forces desired under inUse: with inUse=2 set MaxWarm=1 so
	// desired = clamp(2+2,1,1)=1 < inUse=2. The scale-down must remove dormant pods
	// only and leave BOTH claimed pods intact.
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, pool); err != nil {
		t.Fatalf("get pool to shrink window: %v", err)
	}
	pool.Spec.Autoscale.MaxWarm = 1
	pool.Spec.Autoscale.MinWarm = 1
	if err := k8sClient.Update(ctx, pool); err != nil {
		t.Fatalf("shrink autoscale window: %v", err)
	}
	r.Now = func() time.Time { return frozen.Add(10 * time.Minute) } // well past the 60 s cooldown
	reconcile()

	// The claimed pods must all still be present, undeleted.
	after := claimedNames()
	for name := range before {
		if !after[name] {
			t.Fatalf("claimed husk pod %q was deleted by scale-down; claimed pods must survive (desired below inUse)", name)
		}
	}
	if len(after) != 2 {
		t.Fatalf("phase 4 claimed pods after scale-down = %d, want 2 (claimed pods are never reaped)", len(after))
	}
	// And the dormant set was driven down to the ceiling (only dormant pods removed).
	if got := countDormant(); got > 1 {
		t.Fatalf("phase 4 dormant after scale-down = %d, want <= 1 (surplus dormant removed)", got)
	}
}
