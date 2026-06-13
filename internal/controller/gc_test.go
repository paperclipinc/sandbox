package controller_test

// Envtest coverage for the GC orphan sweep. A forkd sandbox with no backing
// Ready claim and an uptime past the grace is terminated by a GC pass; a
// sandbox WITH a backing Ready claim is left alone; and a fresh orphan (uptime
// under the grace) survives so a just-forked VM whose claim status has not
// landed yet is never killed.

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGCSweepsOrphanVMs(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "gc-node-1", "gc1-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A claim that reaches Ready: its backing VM must NOT be swept.
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "gc1-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "gc1-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "gc1-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "gc1-claim", Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "gc1-pool"}},
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

	ready := waitClaimReady(t, "gc1-claim")
	backedID := ready.Status.SandboxID
	if backedID == "" {
		t.Fatal("ready claim has empty sandbox id")
	}

	// Inject an orphan VM (no backing claim) old enough to exceed the grace.
	const orphanID = "orphan-old"
	engine.InjectSandbox(orphanID, time.Now().Add(-10*time.Minute))

	// Inject a FRESH orphan (no backing claim) under the grace: must survive.
	const freshID = "orphan-fresh"
	engine.InjectSandbox(freshID, time.Now())

	gc := &controller.GarbageCollector{
		Client:      k8sClient,
		Registry:    testRegistry,
		OrphanGrace: 60 * time.Second,
	}
	gc.RunOnce(ctx)

	// The old orphan was terminated.
	terminated := false
	for _, id := range engine.TerminatedIDs() {
		if id == orphanID {
			terminated = true
		}
		if id == backedID {
			t.Fatalf("GC terminated the backed claim's sandbox %s", backedID)
		}
		if id == freshID {
			t.Fatalf("GC terminated a fresh orphan %s under the grace", freshID)
		}
	}
	if !terminated {
		t.Fatalf("GC did not terminate orphan %s; terminated = %v", orphanID, engine.TerminatedIDs())
	}

	// And the orphan is gone from the live listing while the others remain.
	for _, r := range engine.ListSandboxes() {
		if r.ID == orphanID {
			t.Fatalf("orphan %s still live after GC sweep", orphanID)
		}
	}
}
