package controller_test

// Envtest coverage for the NodeLost transition. A Ready claim whose node has
// left the registry (or gone unhealthy) is driven to a terminal Failed phase
// with a NodeLost condition by a GC pass; a claim on a still-healthy node is
// untouched. The orphan sweep and NodeLost do not fight: the sweep only visits
// healthy nodes, so a claim on a lost node is never swept.

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func makeReadyClaim(t *testing.T, prefix, node string) *v1alpha1.SandboxClaim {
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
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-claim", Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: prefix + "-pool"}},
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
	return waitClaimReady(t, prefix+"-claim")
}

func TestGCMarksNodeLost(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "nl-node-1", "nl1-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	makeReadyClaim(t, "nl1", "nl-node-1")

	// The node leaves the registry: the VM died with it. terminateOnNode is
	// never called (nothing to reach); the claim must become NodeLost.
	stop()
	stopped = true

	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	var got v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nl1-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.SandboxFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NodeLost" {
		t.Fatalf("Ready condition = %+v, want Status=False Reason=NodeLost", c)
	}
	if got.Status.FinishedAt == nil {
		t.Fatal("FinishedAt not stamped on NodeLost claim")
	}
	if got.Status.FinishedAt.Time.After(time.Now().Add(time.Second)) {
		t.Fatalf("FinishedAt %v is in the future", got.Status.FinishedAt)
	}
}

// TestGCInHuskModeDoesNotFailNodeLostClaim asserts that in husk mode the GC
// does NOT terminally-fail a Ready claim whose node is lost. Husk node-loss is
// owned by checkHuskPodLost + the husk pod watch, which RE-PEND the claim onto a
// replacement dormant slot. A GC pass winning the race and flipping the claim to
// terminal Failed/NodeLost would defeat the husk self-heal, so the GC skips the
// node-lost-fail entirely when EnableHuskPods is set.
func TestGCInHuskModeDoesNotFailNodeLostClaim(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "nl-node-3", "nl3-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	makeReadyClaim(t, "nl3", "nl-node-3")

	// The node leaves the registry.
	stop()
	stopped = true

	// A GC in husk mode must leave the claim Ready: the husk re-pend path owns
	// node-loss recovery, not the GC.
	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry, EnableHuskPods: true}
	gc.RunOnce(ctx)

	var got v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nl3-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == v1alpha1.SandboxFailed {
		t.Fatalf("husk-mode GC must NOT flip a node-lost claim to Failed; phase = %q", got.Status.Phase)
	}
}

func TestGCLeavesHealthyNodeClaim(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "nl-node-2", "nl2-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeReadyClaim(t, "nl2", "nl-node-2")

	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	var got v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nl2-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.SandboxReady {
		t.Fatalf("phase = %q, want Ready (claim on healthy node must be untouched)", got.Status.Phase)
	}
}
