package controller_test

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestClaimReachesReadyEndToEnd(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "e2e-node-1", "e2e-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "e2e-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "e2e-pool"},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "e2e-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxReady {
				if got.Status.Endpoint == "" {
					t.Fatal("ready claim has empty endpoint")
				}
				if got.Status.SandboxID == "" {
					t.Fatal("ready claim has empty sandboxID")
				}
				if got.Status.Node != "e2e-node-1" {
					t.Fatalf("node = %q, want e2e-node-1", got.Status.Node)
				}
				return
			}
			if got.Status.Phase == v1alpha1.SandboxFailed {
				t.Fatalf("claim failed: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("claim did not become Ready within 15s")
}
