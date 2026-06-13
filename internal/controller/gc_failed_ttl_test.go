package controller_test

// Envtest coverage that early-Failed claims are TTL-eligible. The claim
// reconciler's early-failure paths (volume prep, secret resolution, fork) set
// Phase=Failed; they must also stamp Status.FinishedAt so a GC pass can TTL the
// claim instead of leaking it in etcd forever.

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitClaimFailed polls until the named claim reaches the Failed phase and
// returns it, failing the test if it does not within the window.
func waitClaimFailed(t *testing.T, name string) *v1alpha1.SandboxClaim {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxFailed {
				return &got
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim %s did not become Failed within 20s", name)
	return nil
}

func TestGCTTLsEarlyFailedClaim(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "ef-node-1", "ef-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A claim that references a secret that does not exist: secret resolution
	// fails, driving the reconciler down the early-failure path that sets
	// Phase=Failed. The node has a ready snapshot so selectNode succeeds and the
	// reconciler reaches resolveSecrets.
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "ef-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ef-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "ef-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ef-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "ef-pool"},
			Secrets: []v1alpha1.SecretMount{{
				Name:      "missing",
				SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "does-not-exist"}, Key: "K"},
			}},
		},
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

	failed := waitClaimFailed(t, "ef-claim")
	if failed.Status.FinishedAt == nil {
		t.Fatal("early-Failed claim has no FinishedAt; it would leak in etcd forever")
	}

	// Backdate FinishedAt well past a short TTL, then a GC pass must delete it.
	var got v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ef-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	ttl := int32(10)
	got.Spec.TTLSecondsAfterFinished = &ttl
	if err := k8sClient.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ef-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	old := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	got.Status.FinishedAt = &old
	if err := k8sClient.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}

	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	waitClaimGone(t, "ef-claim")
}
