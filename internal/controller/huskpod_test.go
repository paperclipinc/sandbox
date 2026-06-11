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
	"strings"
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

// TestBuildHuskPodPSARestricted asserts the husk pod satisfies every PSA
// `restricted` control it CAN, both at the pod AND the container securityContext
// level, so a regression that adds a privilege (drops the seccomp profile, flips
// privileged on, allows escalation, or stops dropping ALL capabilities) is caught
// here. It also pins the DOCUMENTED EXCEPTIONS so they cannot drift silently: the
// husk pod is admitted into a baseline/restricted namespace only EXCEPT the
// read-only snapshot hostPath (forbidden under both baseline and restricted) and
// runAsNonRoot=false (forbidden under restricted, the /dev/kvm device exception),
// plus the agentrun.dev/kvm device-plugin resource. The empirical PSA finding (a
// restricted namespace rejects the husk pod on exactly hostPath + runAsNonRoot,
// and the SAME securityContext minus those two is admitted into restricted) is
// proven object-level on kind in the conformance job; this unit test pins the
// spec fields those exceptions and the satisfied controls correspond to.
func TestBuildHuskPodPSARestricted(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "psa-pool", Namespace: "default", UID: "pool-uid-psa"},
		Spec:       v1alpha1.SandboxPoolSpec{TemplateRef: v1alpha1.LocalObjectReference{Name: "psa-tmpl"}, Replicas: 1},
	}
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "psa-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, template, controller.HuskPodOptions{
		StubImage:  "agent-run-husk-stub:test",
		SnapshotID: "psa-tmpl",
		DataDir:    "/var/lib/agent-run",
	})

	// POD-LEVEL securityContext: PSA restricted checks seccompProfile at the pod
	// OR the container level; we set BOTH so the pod-level control is satisfied
	// even if a future container is added without its own profile.
	psc := pod.Spec.SecurityContext
	if psc == nil {
		t.Fatal("pod-level SecurityContext is nil; PSA restricted checks the pod-level seccompProfile")
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod SeccompProfile = %+v, want RuntimeDefault", psc.SeccompProfile)
	}
	// runAsNonRoot at the pod level is the documented exception: it is FALSE so
	// Firecracker can open the device-plugin-injected /dev/kvm as uid 0 WITHOUT
	// privileged. This is the ONLY restricted securityContext control the husk pod
	// does not satisfy; it is documented, not accidental.
	if psc.RunAsNonRoot == nil || *psc.RunAsNonRoot {
		t.Error("pod RunAsNonRoot must be explicitly false (the documented /dev/kvm device exception)")
	}

	// CONTAINER-LEVEL securityContext: every other restricted control IS satisfied,
	// so the husk pod's securityContext is restricted-clean and only the hostPath +
	// runAsNonRoot exceptions keep it out of a restricted namespace.
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("container SecurityContext is nil")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("container Privileged must be explicitly false (restricted: privileged forbidden)")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("container AllowPrivilegeEscalation must be explicitly false (restricted control)")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container Capabilities.Drop = %+v, want [ALL] (restricted control)", sc.Capabilities)
	}
	if len(sc.Capabilities.Add) != 0 {
		t.Errorf("container Capabilities.Add = %+v, want none (restricted forbids adding back)", sc.Capabilities.Add)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("container SeccompProfile = %+v, want RuntimeDefault (restricted control)", sc.SeccompProfile)
	}

	// DOCUMENTED EXCEPTION: the read-only snapshot hostPath. It is forbidden under
	// BOTH baseline and restricted; the husk pod carries it as the documented
	// node-snapshot-read exception. Pin it read-only so a regression to a writable
	// snapshot mount is caught.
	var snapVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "snapshot" {
			snapVol = &pod.Spec.Volumes[i]
		}
	}
	if snapVol == nil || snapVol.HostPath == nil {
		t.Fatal("snapshot volume must be a hostPath (the documented node-snapshot exception)")
	}
	var snapMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == "snapshot" {
			snapMount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if snapMount == nil || !snapMount.ReadOnly {
		t.Errorf("snapshot mount = %+v, want present and ReadOnly", snapMount)
	}

	// DOCUMENTED EXCEPTION: the /dev/kvm device-plugin resource request (request
	// AND limit), which replaces privileged: true.
	kvm := corev1.ResourceName("agentrun.dev/kvm")
	if got := pod.Spec.Containers[0].Resources.Requests[kvm]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("kvm request = %s, want 1 (the device-plugin exception)", got.String())
	}
}

func TestBuildHuskPodControlAndSnapshot(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-pool", Namespace: "default", UID: "pool-uid-9"},
		Spec:       v1alpha1.SandboxPoolSpec{TemplateRef: v1alpha1.LocalObjectReference{Name: "ctl-tmpl"}, Replicas: 1},
	}
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, template, controller.HuskPodOptions{
		StubImage:     "agent-run-husk-stub:test",
		SnapshotID:    "ctl-tmpl",
		DataDir:       "/var/lib/agent-run",
		TLSSecretName: "forkd-tls",
		CASecretName:  "agent-run-ca",
	})

	ctr := pod.Spec.Containers[0]
	args := strings.Join(ctr.Args, " ")

	// mTLS network control: the control-listen port + the three TLS PEM args.
	if !strings.Contains(args, "--control-listen") {
		t.Errorf("args missing --control-listen: %v", ctr.Args)
	}
	// The in-pod sandbox HTTP API is served on the sandbox port so the endpoint
	// the claim advertises (podIP:9091) is reachable and token-gated.
	if !strings.Contains(args, "--sandbox-listen :9091") {
		t.Errorf("args missing --sandbox-listen :9091: %v", ctr.Args)
	}
	for _, flag := range []string{"--tls-cert", "--tls-key", "--tls-ca"} {
		if !strings.Contains(args, flag) {
			t.Errorf("args missing %s: %v", flag, ctr.Args)
		}
	}

	// The sandbox endpoint port is exposed as a container port so the claim's
	// Status.Endpoint (podIP:port) is reachable.
	var hasSandboxPort bool
	for _, p := range ctr.Ports {
		if p.ContainerPort == 9091 {
			hasSandboxPort = true
		}
	}
	if !hasSandboxPort {
		t.Errorf("container ports = %+v, want one with 9091 (sandbox endpoint)", ctr.Ports)
	}

	// A read-only mount of the node's template snapshot dir and the kernel, plus
	// the TLS Secret mount.
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.Name] = m
	}
	if m, ok := mounts["snapshot"]; !ok || !m.ReadOnly {
		t.Errorf("snapshot mount missing or not read-only: %+v", mounts)
	}
	if m, ok := mounts["kernel"]; !ok || !m.ReadOnly {
		t.Errorf("kernel mount missing or not read-only: %+v", mounts)
	}
	if m, ok := mounts["husk-tls"]; !ok || !m.ReadOnly {
		t.Errorf("husk-tls mount missing or not read-only: %+v", mounts)
	}
	if m, ok := mounts["husk-ca"]; !ok || !m.ReadOnly {
		t.Errorf("husk-ca mount missing or not read-only: %+v", mounts)
	}

	// The snapshot hostPath points at <dataDir>/templates/<snapshotID>/snapshot.
	var snapVol *corev1.Volume
	var tlsVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		switch pod.Spec.Volumes[i].Name {
		case "snapshot":
			snapVol = &pod.Spec.Volumes[i]
		case "husk-tls":
			tlsVol = &pod.Spec.Volumes[i]
		}
	}
	if snapVol == nil || snapVol.HostPath == nil {
		t.Fatalf("snapshot volume is not a hostPath: %+v", snapVol)
	}
	if snapVol.HostPath.Path != "/var/lib/agent-run/templates/ctl-tmpl/snapshot" {
		t.Errorf("snapshot hostPath = %q, want /var/lib/agent-run/templates/ctl-tmpl/snapshot", snapVol.HostPath.Path)
	}
	if tlsVol == nil || tlsVol.Secret == nil || tlsVol.Secret.SecretName != "forkd-tls" {
		t.Errorf("husk-tls volume should mount the forkd-tls Secret: %+v", tlsVol)
	}

	// Placement: the pod must land on a KVM node.
	if pod.Spec.NodeSelector["agentrun.dev/kvm"] != "true" {
		t.Errorf("nodeSelector = %+v, want agentrun.dev/kvm=true", pod.Spec.NodeSelector)
	}
}

// TestBuildHuskPodMountsManifestWhenDigestKnown proves that when the pool has a
// recorded template digest, the husk pod mounts the recorded CAS manifest
// read-only and runs the stub with verify ENFORCED (--manifest, no escape
// hatch), so the stub re-verifies the snapshot before loading (fail-closed).
func TestBuildHuskPodMountsManifestWhenDigestKnown(t *testing.T) {
	const digest = "abc1230000000000000000000000000000000000000000000000000000000000"
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "verify-pool", Namespace: "default", UID: "pool-uid-v"},
		Spec:       v1alpha1.SandboxPoolSpec{TemplateRef: v1alpha1.LocalObjectReference{Name: "verify-tmpl"}, Replicas: 1},
	}
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "verify-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, template, controller.HuskPodOptions{
		StubImage:      "agent-run-husk-stub:test",
		SnapshotID:     "verify-tmpl",
		DataDir:        "/var/lib/agent-run",
		ExpectedDigest: digest,
	})

	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--manifest /var/lib/agent-run/manifest.json") {
		t.Errorf("args missing --manifest mount path: %v", pod.Spec.Containers[0].Args)
	}
	if strings.Contains(args, "--allow-unverified-snapshots") {
		t.Errorf("verify must be ENFORCED when a digest is known; escape hatch present: %v", pod.Spec.Containers[0].Args)
	}

	// The manifest is a read-only hostPath at <dataDir>/cas/manifests/<digest>.
	var manVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "snapshot-manifest" {
			manVol = &pod.Spec.Volumes[i]
		}
	}
	if manVol == nil || manVol.HostPath == nil {
		t.Fatalf("snapshot-manifest volume is not a hostPath: %+v", manVol)
	}
	if manVol.HostPath.Path != "/var/lib/agent-run/cas/manifests/"+digest {
		t.Errorf("manifest hostPath = %q, want /var/lib/agent-run/cas/manifests/%s", manVol.HostPath.Path, digest)
	}
	var mounted bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "snapshot-manifest" {
			mounted = true
			if !m.ReadOnly {
				t.Error("manifest mount must be read-only")
			}
		}
	}
	if !mounted {
		t.Error("manifest volume is not mounted into the container")
	}
}

// TestBuildHuskPodEscapeHatchWhenNoDigest proves the fallback: with no recorded
// digest the husk pod mounts no manifest and runs the stub's development escape
// hatch so the warm pool still activates (the stub logs this loudly).
func TestBuildHuskPodEscapeHatchWhenNoDigest(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "nodigest-pool", Namespace: "default", UID: "pool-uid-n"},
		Spec:       v1alpha1.SandboxPoolSpec{TemplateRef: v1alpha1.LocalObjectReference{Name: "nodigest-tmpl"}, Replicas: 1},
	}
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "nodigest-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, template, controller.HuskPodOptions{
		StubImage:  "agent-run-husk-stub:test",
		SnapshotID: "nodigest-tmpl",
		DataDir:    "/var/lib/agent-run",
	})

	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--allow-unverified-snapshots") {
		t.Errorf("with no digest the stub must run the escape hatch: %v", pod.Spec.Containers[0].Args)
	}
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "snapshot-manifest" {
			t.Error("no manifest should be mounted when no digest is recorded")
		}
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
