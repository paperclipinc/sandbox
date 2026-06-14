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
	"strings"
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

// TestHuskForkNeverOverCreatesChildren is the regression for the live KVM
// over-creation bug: a SandboxFork with Replicas=2 driven through MANY reconcile
// passes while its children stay NOT Ready must create EXACTLY 2 child pods, never
// 3+. The old loop derived child names from (TotalForks + i) and the iteration
// count from (Replicas - ReadyForks); once a child became Ready mid-loop it bumped
// TotalForks, shifting the next index to a NEW name (fork-2, fork-3, ...) so
// ensureForkChildPod created an EXTRA pod instead of reusing a slot, overcommitting
// the single node. The fixed-slot loop uses stable names ("<fork>-fork-<i>" for i
// in [0, Replicas)) so the child count can never exceed Replicas. Children are kept
// pending across passes here to exercise the many-pass path that produced fork-2.
func TestHuskForkNeverOverCreatesChildren(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-noover", "10.0.0.11")
	makeForkSourceClaim(t, "src-claim-noover", "pool-noover", srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	fork := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "noover-1",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1alpha1.SandboxForkSpec{SourceRef: v1alpha1.LocalObjectReference{Name: "src-claim-noover"}, Replicas: 2},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Wait until both slots exist, then let many requeue passes elapse with the
	// children deliberately LEFT not-Ready (forceHuskPodReady is never called), so
	// the reconciler runs the full slot loop repeatedly.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("noover-1"))
		return len(pods.Items) >= 2
	})
	time.Sleep(3 * time.Second)

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren("noover-1")); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != 2 {
		names := make([]string, 0, len(pods.Items))
		for i := range pods.Items {
			names = append(names, pods.Items[i].Name)
		}
		t.Fatalf("expected EXACTLY 2 child pods for Replicas=2, got %d: %v", len(pods.Items), names)
	}

	// Now drive them Ready and confirm the fork completes at exactly 2, still
	// without spawning a third pod.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var p corev1.PodList
		_ = k8sClient.List(ctx, &p, listForkChildren("noover-1"))
		for i := range p.Items {
			forceHuskPodReady(t, &p.Items[i])
		}
		var got v1alpha1.SandboxFork
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "noover-1", Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyForks == 2
	})

	var after corev1.PodList
	if err := k8sClient.List(ctx, &after, listForkChildren("noover-1")); err != nil {
		t.Fatalf("list children after ready: %v", err)
	}
	if len(after.Items) != 2 {
		t.Fatalf("expected EXACTLY 2 child pods after completion, got %d", len(after.Items))
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

// TestHuskForkChildPodHasFullHuskShape is the regression for the live KVM crash
// "husk-stub: read --tls-cert: open /etc/husk/tls/tls.crt: no such file or
// directory": the fork child pod the controller emits must carry EVERYTHING a
// warm husk pod carries (the husk PKI mTLS Secret volumes, the kernel, forks, and
// writable rootfs-CoW hostPaths, the kvm device resource, the POD_NAME downward
// API, and the locked-down securityContext), so the child stub finds its TLS
// material and all the files it activates from. The previous bug threaded the
// fork-specific opts but DROPPED TLSSecretName/CASecretName, so buildHuskPod
// skipped the TLS/CA volumes and the child crash-looped. This drives the real
// controller opts path (not a direct buildForkChildPod call), so it catches the
// wiring, not just the builder.
func TestHuskForkChildPodHasFullHuskShape(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-shape", "10.0.0.9")
	makeForkSourceClaim(t, "src-claim-shape", "pool-shape", srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/x"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	fork := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shape-1",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1alpha1.SandboxForkSpec{SourceRef: v1alpha1.LocalObjectReference{Name: "src-claim-shape"}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Wait for the controller to emit the child pod (forcing it Ready so the
	// reconcile keeps progressing).
	var child corev1.Pod
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("shape-1"))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		if len(pods.Items) == 0 {
			return false
		}
		child = pods.Items[0]
		return true
	})

	// Index the pod's volumes and the stub container's mounts.
	vols := map[string]corev1.Volume{}
	for _, v := range child.Spec.Volumes {
		vols[v.Name] = v
	}
	var stub corev1.Container
	for i := range child.Spec.Containers {
		if child.Spec.Containers[i].Name == "husk-stub" {
			stub = child.Spec.Containers[i]
		}
	}
	if stub.Name == "" {
		t.Fatalf("fork child has no husk-stub container: %+v", child.Spec.Containers)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range stub.VolumeMounts {
		mounts[m.Name] = m
	}

	// The husk PKI TLS Secret volumes + mounts (the crash). The leaf Secret backs
	// /etc/husk/tls (tls.crt + tls.key); the CA Secret backs /etc/husk/ca (ca.crt).
	tlsVol, ok := vols["husk-tls"]
	if !ok || tlsVol.Secret == nil || tlsVol.Secret.SecretName != "mitos-forkd-tls" {
		t.Fatalf("fork child missing husk-tls leaf Secret volume (mitos-forkd-tls): %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["husk-tls"]; !ok || !m.ReadOnly || m.MountPath != "/etc/husk/tls" {
		t.Fatalf("fork child husk-tls mount missing/wrong (want RO /etc/husk/tls): %+v", mounts)
	}
	caVol, ok := vols["husk-ca"]
	if !ok || caVol.Secret == nil || caVol.Secret.SecretName != "mitos-ca" {
		t.Fatalf("fork child missing husk-ca Secret volume (mitos-ca): %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["husk-ca"]; !ok || !m.ReadOnly || m.MountPath != "/etc/husk/ca" {
		t.Fatalf("fork child husk-ca mount missing/wrong (want RO /etc/husk/ca): %+v", mounts)
	}

	// The kernel hostPath + mount.
	if _, ok := vols["kernel"]; !ok {
		t.Fatalf("fork child missing kernel hostPath volume: %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["kernel"]; !ok || m.MountPath != "/var/lib/mitos/kernel/vmlinux" {
		t.Fatalf("fork child kernel mount missing/wrong: %+v", mounts)
	}

	// The forks hostPath dir (read-write so the child can itself be re-forked).
	if _, ok := vols["husk-forks"]; !ok {
		t.Fatalf("fork child missing husk-forks hostPath volume: %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["husk-forks"]; !ok || m.ReadOnly {
		t.Fatalf("fork child husk-forks mount missing or read-only: %+v", mounts)
	}

	// The writable per-activation rootfs-CoW hostPath dir (the child writes its
	// own clone here, so it must NOT be read-only).
	if _, ok := vols["husk-rootfs-cow"]; !ok {
		t.Fatalf("fork child missing husk-rootfs-cow hostPath volume: %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["husk-rootfs-cow"]; !ok || m.ReadOnly {
		t.Fatalf("fork child husk-rootfs-cow mount missing or read-only (child writes its CoW clone): %+v", mounts)
	}

	// The fork snapshot mount itself (read-only), pointing at the fork snapshot.
	if m, ok := mounts["snapshot"]; !ok || !m.ReadOnly {
		t.Fatalf("fork child snapshot mount missing or not read-only: %+v", mounts)
	}

	// The kvm device resource: request == limit == 1.
	req := stub.Resources.Requests[corev1.ResourceName("mitos.run/kvm")]
	lim := stub.Resources.Limits[corev1.ResourceName("mitos.run/kvm")]
	if req.Value() != 1 || lim.Value() != 1 {
		t.Fatalf("fork child kvm device resource not 1/1: req=%s lim=%s", req.String(), lim.String())
	}

	// POD_NAME downward API (scopes the per-pod CoW clone path).
	var hasPodName bool
	for _, e := range stub.Env {
		if e.Name == "POD_NAME" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "metadata.name" {
			hasPodName = true
		}
	}
	if !hasPodName {
		t.Fatalf("fork child missing POD_NAME downward API env: %+v", stub.Env)
	}

	// SecurityContext: same lockdown as a warm pod (no privilege, no escalation,
	// drop ALL caps, RuntimeDefault seccomp).
	sc := stub.SecurityContext
	if sc == nil || sc.Privileged == nil || *sc.Privileged {
		t.Fatalf("fork child must not be privileged: %+v", sc)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatalf("fork child must deny privilege escalation: %+v", sc)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("fork child must drop ALL caps: %+v", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("fork child must use RuntimeDefault seccomp: %+v", sc.SeccompProfile)
	}

	// Fork-specific bits remain: pinned to the source node, and --template-rootfs
	// (the CoW clone source) is the SOURCE pod's live rootfs, not the template's.
	if child.Spec.Affinity == nil || child.Spec.Affinity.NodeAffinity == nil {
		t.Fatalf("fork child must be pinned to the source node via affinity")
	}
	args := strings.Join(stub.Args, " ")
	if !strings.Contains(args, "--template-rootfs /var/lib/mitos/husk-rootfs/"+srcPod.Name+"/rootfs.ext4") {
		t.Fatalf("fork child --template-rootfs must be the source pod rootfs; args=%v", stub.Args)
	}
}
