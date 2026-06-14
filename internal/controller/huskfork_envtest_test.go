package controller_test

// Envtest coverage for the husk-pod live fork path (live SandboxFork on the husk
// pod-native path). With EnableHuskPods a husk-backed source claim forked with
// replicas=N snapshots the source pod's running VM once and activates N child
// husk pods from that fork snapshot, each reaching Ready through the same
// warm-pod Activate path (which runs the fail-closed RNG/clock reseed handshake).
//
// envtest has no kubelet, so the test forces each created child Running+Ready and
// drives the fork-snapshot / activate / remove transports through the suite's
// swappable fakes (setForkSnapshotter / setForkActivator / setForkSnapshotRemover).

import (
	"context"
	"crypto/tls"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/husk"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// forceHuskPodReady forces a husk pod Running+Ready with its PodIP set, so the
// husk fork reconciler can dial it (envtest has no kubelet to do this).
func forceHuskPodReady(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	var got corev1.Pod
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &got); err != nil {
		t.Fatalf("get pod %s: %v", pod.Name, err)
	}
	if got.Status.Phase == corev1.PodRunning && got.Status.PodIP != "" {
		return
	}
	got.Status.Phase = corev1.PodRunning
	if got.Status.PodIP == "" {
		got.Status.PodIP = "10.0.1.1"
	}
	got.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := k8sClient.Status().Update(ctx, &got); err != nil {
		t.Fatalf("force pod %s ready: %v", pod.Name, err)
	}
}

// listForkChildren matches the husk pods owned by a fork.
func listForkChildren(forkName string) client.ListOption {
	return client.MatchingLabels{"mitos.run/fork": forkName}
}

// waitUntilForkReady polls cond until true or the deadline elapses.
func waitUntilForkReady(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// makeForkSourceClaim creates a Ready husk-backed source claim pointing at srcPod.
func makeForkSourceClaim(t *testing.T, name, poolName string, srcPod *corev1.Pod) *v1alpha1.SandboxClaim {
	t.Helper()
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: poolName}},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })
	claim.Status.Phase = v1alpha1.SandboxReady
	claim.Status.Node = srcPod.Spec.NodeName
	claim.Status.SandboxID = srcPod.Name
	if err := k8sClient.Status().Update(ctx, claim); err != nil {
		t.Fatalf("update claim status: %v", err)
	}
	return claim
}

func TestHuskForkProducesReadyChildren(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-hf", "10.0.0.5")
	makeForkSourceClaim(t, "src-claim", "pool-hf", srcPod)

	var snapCalls int
	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		snapCalls++
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	fork := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hf-1",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1alpha1.SandboxForkSpec{SourceRef: v1alpha1.LocalObjectReference{Name: "src-claim"}, Replicas: 2},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hf-1"))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		var got v1alpha1.SandboxFork
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hf-1", Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyForks == 2
	})

	if snapCalls < 1 {
		t.Fatalf("expected at least one fork-snapshot call, got %d", snapCalls)
	}
}

// TestHuskForkSnapshotTakenExactlyOnce is the BUG 2 regression: the fork
// snapshot must be taken EXACTLY ONCE for a SandboxFork and reused for all
// children across reconcile passes. Children take several passes to reach Ready;
// re-snapshotting on each pass re-pauses the source and overwrites the fork
// mem/vmstate, so a child activated in a later pass would restore a NEWER source
// memory state than an earlier child: the N children would not be a coherent
// single fork point. The children are deliberately NOT forced Ready until after
// several reconcile passes have elapsed, so the bug (per-pass re-snapshot) would
// show snapCalls > 1.
func TestHuskForkSnapshotTakenExactlyOnce(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-once", "10.0.0.7")
	makeForkSourceClaim(t, "src-claim-once", "pool-once", srcPod)

	var snapCalls int32
	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		atomic.AddInt32(&snapCalls, 1)
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	fork := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hf-once",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1alpha1.SandboxForkSpec{SourceRef: v1alpha1.LocalObjectReference{Name: "src-claim-once"}, Replicas: 2},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Wait for the child pods to be created (this guarantees the snapshot op has
	// run at least once) WITHOUT forcing them Ready, so the reconciler requeues
	// repeatedly with children pending. A per-pass re-snapshot would bump
	// snapCalls above 1 during this window.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hf-once"))
		return len(pods.Items) == 2
	})
	// Let several requeue passes elapse with children still pending.
	time.Sleep(3 * time.Second)

	if got := atomic.LoadInt32(&snapCalls); got != 1 {
		t.Fatalf("fork snapshot must be taken exactly once across passes; got %d calls", got)
	}

	// Now drive the children Ready and confirm the fork still completes WITHOUT
	// any further snapshot calls (reuse of the single snapshot).
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hf-once"))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		var got v1alpha1.SandboxFork
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hf-once", Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyForks == 2
	})

	if got := atomic.LoadInt32(&snapCalls); got != 1 {
		t.Fatalf("fork snapshot re-taken after children came Ready; got %d calls, want 1", got)
	}
}

func TestHuskForkRemovesSnapshotOnDelete(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-hd", "10.0.0.6")
	makeForkSourceClaim(t, "src-claim-d", "pool-hd", srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/x"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	removeCalls := make(chan struct{}, 4)
	setForkSnapshotRemover(func(_ context.Context, _ string, _ *tls.Config, _ husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		select {
		case removeCalls <- struct{}{}:
		default:
		}
		return husk.ForkSnapshotResult{OK: true}, nil
	})
	t.Cleanup(func() { setForkSnapshotRemover(nil) })

	fork := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hd-1",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1alpha1.SandboxForkSpec{SourceRef: v1alpha1.LocalObjectReference{Name: "src-claim-d"}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hd-1"))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		var got v1alpha1.SandboxFork
		_ = k8sClient.Get(ctx, types.NamespacedName{Name: "hd-1", Namespace: "default"}, &got)
		return got.Status.ReadyForks == 1
	})

	if err := k8sClient.Delete(ctx, fork); err != nil {
		t.Fatalf("delete fork: %v", err)
	}
	select {
	case <-removeCalls:
	case <-time.After(15 * time.Second):
		t.Fatalf("remove-fork-snapshot was not called on delete")
	}
}
