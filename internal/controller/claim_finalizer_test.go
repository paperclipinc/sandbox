package controller_test

// Envtest coverage for the claim terminate-finalizer: a claim that reaches
// Ready carries mitos.run/forkd-terminate, and deleting it reaps the
// backing VM via forkd Terminate before the object disappears. A claim whose
// node has left the registry still deletes cleanly (the VM died with the
// node), never hanging on the finalizer.

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitClaimGone polls until the named claim no longer exists, failing the
// test if it lingers (a stuck finalizer).
func waitClaimGone(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got)
		if apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim %s still present after 15s; finalizer likely stuck", name)
}

func waitTerminated(t *testing.T, engine interface{ TerminatedIDs() []string }, sandboxID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, id := range engine.TerminatedIDs() {
			if id == sandboxID {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("forkd never received Terminate for sandbox %s", sandboxID)
}

func TestClaimDeleteReapsBackingVM(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "fin-node-1", "fin-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "fin-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fin-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "fin-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "fin-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "fin-pool"},
		},
	}
	for _, obj := range []client.Object{template, pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	got := waitClaimReady(t, "fin-claim")
	sandboxID := got.Status.SandboxID
	if sandboxID == "" {
		t.Fatal("ready claim has empty sandbox id")
	}

	// The finalizer must be present on a Ready claim.
	hasFinalizer := false
	for _, f := range got.Finalizers {
		if f == controller.FinalizerTerminate {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Fatalf("ready claim missing finalizer %s; finalizers = %v", controller.FinalizerTerminate, got.Finalizers)
	}

	if err := k8sClient.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}

	waitTerminated(t, engine, sandboxID)
	waitClaimGone(t, "fin-claim")
}

func TestClaimDeleteWithGoneNodeCompletes(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "fin-node-2", "fin2-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "fin2-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fin2-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "fin2-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "fin2-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "fin2-pool"},
		},
	}
	for _, obj := range []client.Object{template, pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	got := waitClaimReady(t, "fin2-claim")

	// Drop the node from the registry (and shut down forkd): the VM died with
	// it. Deletion must still complete; terminateOnNode treats a vanished node
	// as already-terminated.
	stop()
	stopped = true

	if err := k8sClient.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	waitClaimGone(t, "fin2-claim")
}

// TestClaimDeleteWithUnreachableForkdCompletes covers the deletion-wedge fix:
// the node stays REGISTERED and heartbeating, but its forkd server is stopped
// so Terminate errors Unavailable. Deletion must still complete within the
// bounded window: a confirm-terminate we cannot make must not wedge the object.
func TestClaimDeleteWithUnreachableForkdCompletes(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "fin-node-3", "fin3-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "fin3-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fin3-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "fin3-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "fin3-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "fin3-pool"},
		},
	}
	for _, obj := range []client.Object{template, pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	got := waitClaimReady(t, "fin3-claim")

	// Stop only the forkd gRPC/HTTP servers (and remove the registry entry via
	// the stop func). To exercise the unreachable-but-registered path we
	// instead leave the node registered: re-register it after stopping the
	// servers so the dial target points at a dead listener and Terminate errors
	// Unavailable.
	endpoint := mustNodeEndpoint(t, "fin-node-3")
	stop()
	stopped = true
	testRegistry.Register(&controller.NodeInfo{
		Name:         "fin-node-3",
		Endpoint:     endpoint,
		MaxSandboxes: 100,
	})
	t.Cleanup(func() { testRegistry.Unregister("fin-node-3") })

	if err := k8sClient.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	// Bounded: the Terminate RPC times out / errors Unavailable, which the
	// finalizer treats as already-terminated, so the object is freed.
	waitClaimGone(t, "fin3-claim")
}

// mustNodeEndpoint returns the gRPC endpoint of a registered node.
func mustNodeEndpoint(t *testing.T, name string) string {
	t.Helper()
	node, ok := testRegistry.GetNode(name)
	if !ok {
		t.Fatalf("node %s not registered", name)
	}
	return node.Endpoint
}
