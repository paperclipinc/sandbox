package controller_test

// Coverage for the husk pod warm-pool lifecycle (issue #18, slice 1).
//
// Two layers:
//   - a pure unit test of buildHuskPod that asserts the spec the controller
//     emits: the agentrun.dev/kvm request+limit, the documented non-privileged
//     securityContext, the owner-ref to the pool, the two husk labels, the
//     cpu/memory requests, and the stub image.
//   - an envtest of reconcileHuskPods that drives the warm pool through create
//     (Replicas=3 -> 3 husk pod objects owned by the pool), scale-down
//     (Replicas=1 -> 2 deleted), and the flag-off case (no husk pods). envtest
//     has no kubelet, so the pods never run; the reconcile converges on object
//     EXISTENCE, which this test asserts (the real-vs-envtest readiness nuance
//     is documented in huskpod.go).

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBuildHuskPodSpec(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "spec-pool", Namespace: "default", UID: "pool-uid-1"},
		Spec:       v1alpha1.SandboxPoolSpec{TemplateRef: v1alpha1.LocalObjectReference{Name: "spec-tmpl"}, Replicas: 2},
	}
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "spec-tmpl", Namespace: "default"},
		Spec: v1alpha1.SandboxTemplateSpec{
			Image: "python:3.12-slim",
			Resources: v1alpha1.SandboxResources{
				CPU:    resource.MustParse("2"),
				Memory: resource.MustParse("1Gi"),
			},
		},
	}

	c := k8sClient
	r := &controller.SandboxPoolReconciler{Client: c}
	pod := r.BuildHuskPodForTest(pool, template, controller.HuskPodOptions{
		StubImage:       "agent-run-husk-stub:test",
		KVMResourceName: "agentrun.dev/kvm",
	})

	if pod.GenerateName != "spec-pool-husk-" {
		t.Errorf("GenerateName = %q, want spec-pool-husk-", pod.GenerateName)
	}
	if pod.Namespace != "default" {
		t.Errorf("Namespace = %q, want default", pod.Namespace)
	}
	if pod.Labels["agentrun.dev/pool"] != "spec-pool" {
		t.Errorf("pool label = %q, want spec-pool", pod.Labels["agentrun.dev/pool"])
	}
	if pod.Labels["agentrun.dev/husk"] != "true" {
		t.Errorf("husk label = %q, want true", pod.Labels["agentrun.dev/husk"])
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("RestartPolicy = %q, want Always", pod.Spec.RestartPolicy)
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "SandboxPool" || owner.Name != "spec-pool" {
		t.Fatalf("controller owner = %+v, want SandboxPool spec-pool", owner)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}
	ctr := pod.Spec.Containers[0]
	if ctr.Name != "husk-stub" {
		t.Errorf("container name = %q, want husk-stub", ctr.Name)
	}
	if ctr.Image != "agent-run-husk-stub:test" {
		t.Errorf("container image = %q, want agent-run-husk-stub:test", ctr.Image)
	}

	kvm := corev1.ResourceName("agentrun.dev/kvm")
	if got := ctr.Resources.Requests[kvm]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("kvm request = %s, want 1", got.String())
	}
	if got := ctr.Resources.Limits[kvm]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("kvm limit = %s, want 1", got.String())
	}
	// cpu/memory requests sized from the template so the sandbox shows as
	// ordinary pod requests (scheduler truth).
	if got := ctr.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("cpu request = %s, want 2 (from template)", got.String())
	}
	if got := ctr.Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("memory request = %s, want 1Gi (from template)", got.String())
	}

	sc := ctr.SecurityContext
	if sc == nil {
		t.Fatal("container SecurityContext is nil")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("Privileged must be explicitly false")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be explicitly false")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("Capabilities.Drop = %+v, want [ALL]", sc.Capabilities)
	}
	if len(sc.Capabilities.Add) != 0 {
		t.Errorf("Capabilities.Add = %+v, want none (networking caps come with the networking slice)", sc.Capabilities.Add)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile = %+v, want RuntimeDefault", sc.SeccompProfile)
	}
}

func TestBuildHuskPodDefaultSizing(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "def-pool", Namespace: "default", UID: "pool-uid-2"},
		Spec:       v1alpha1.SandboxPoolSpec{TemplateRef: v1alpha1.LocalObjectReference{Name: "def-tmpl"}, Replicas: 1},
	}
	// A template with no Resources: the builder must fall back to the
	// documented default (1 cpu / 512Mi).
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "def-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}

	c := k8sClient
	r := &controller.SandboxPoolReconciler{Client: c}
	pod := r.BuildHuskPodForTest(pool, template, controller.HuskPodOptions{})

	// Default kvm resource name when opts leaves it empty.
	kvm := corev1.ResourceName("agentrun.dev/kvm")
	if got := pod.Spec.Containers[0].Resources.Requests[kvm]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("default kvm request = %s, want 1", got.String())
	}
	if got := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("default cpu request = %s, want 1", got.String())
	}
	if got := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("default memory request = %s, want 512Mi", got.String())
	}
}

func listHuskPods(t *testing.T, c client.Client, poolName string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace("default"),
		client.MatchingLabels{"agentrun.dev/pool": poolName, "agentrun.dev/husk": "true"},
	); err != nil {
		t.Fatalf("list husk pods: %v", err)
	}
	return pods.Items
}

func waitHuskPodCount(t *testing.T, c client.Client, poolName string, want int) []corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []corev1.Pod
	for time.Now().Before(deadline) {
		last = listHuskPods(t, c, poolName)
		if len(last) == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("husk pod count for %s = %d, want %d", poolName, len(last), want)
	return nil
}

func TestReconcileHuskPodsCreateScaleAndFlagOff(t *testing.T) {
	c := k8sClient

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "husk-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "husk-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "husk-tmpl"},
			Replicas:    3,
		},
	}
	for _, obj := range []client.Object{template, pool} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, "husk-pool") {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
		_ = c.Delete(ctx, template)
	})

	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    controller.NewNodeRegistry(),
		EnableHuskPods:  true,
		HuskStubImage:   "agent-run-husk-stub:test",
		KVMResourceName: "agentrun.dev/kvm",
	}

	// Re-fetch the pool so the reconciler works against a server-populated UID
	// (SetControllerReference requires the owner UID).
	var got v1alpha1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &got); err != nil {
		t.Fatal(err)
	}

	// Create: Replicas=3 -> 3 husk pod objects owned by the pool.
	count, err := r.ReconcileHuskPodsForTest(ctx, &got, template)
	if err != nil {
		t.Fatalf("reconcileHuskPods (create): %v", err)
	}
	if count != 3 {
		t.Fatalf("reconcileHuskPods returned %d, want 3", count)
	}
	pods := waitHuskPodCount(t, c, "husk-pool", 3)
	for _, p := range pods {
		owner := metav1.GetControllerOf(&p)
		if owner == nil || owner.Kind != "SandboxPool" || owner.Name != "husk-pool" {
			t.Fatalf("husk pod %s owner = %+v, want SandboxPool husk-pool", p.Name, owner)
		}
	}

	// Idempotent: a second reconcile at the same Replicas creates nothing new.
	count, err = r.ReconcileHuskPodsForTest(ctx, &got, template)
	if err != nil {
		t.Fatalf("reconcileHuskPods (idempotent): %v", err)
	}
	if count != 3 {
		t.Fatalf("idempotent reconcile returned %d, want 3", count)
	}
	waitHuskPodCount(t, c, "husk-pool", 3)

	// Scale down: Replicas=1 -> 2 deleted.
	got.Spec.Replicas = 1
	count, err = r.ReconcileHuskPodsForTest(ctx, &got, template)
	if err != nil {
		t.Fatalf("reconcileHuskPods (scale down): %v", err)
	}
	if count != 1 {
		t.Fatalf("reconcileHuskPods after scale-down returned %d, want 1", count)
	}
	waitHuskPodCount(t, c, "husk-pool", 1)
}

func TestReconcileHuskPodsFlagOffCreatesNone(t *testing.T) {
	c := k8sClient

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "off-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "off-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "off-tmpl"},
			Replicas:    2,
		},
	}
	for _, obj := range []client.Object{template, pool} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = c.Delete(ctx, pool)
		_ = c.Delete(ctx, template)
	})

	// EnableHuskPods false: the pool reconcile runs the raw-forkd path through
	// the manager (no fake forkd node registered, so no snapshots either). The
	// invariant under test is that NO husk pods exist.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n := len(listHuskPods(t, c, "off-pool")); n != 0 {
			t.Fatalf("husk pods created with flag off: %d", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
