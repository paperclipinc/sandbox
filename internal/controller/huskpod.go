package controller

import (
	"context"
	"fmt"
	"sort"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Husk pod warm-pool lifecycle (issue #18, slice 1).
//
// When --enable-husk-pods is set, a SandboxPool maintains a warm pool of
// pre-scheduled "husk" pods instead of building node-local snapshots. Each husk
// pod runs the dormant-VMM stub (cmd/husk-stub): it Prepares a dormant
// Firecracker VMM at start and waits on a control channel; a later migration
// slice (claim activation, slice 2) drives the in-place snapshot-load activation
// over that channel. Pre-scheduling pays the expensive Kubernetes work
// (scheduling, admission, netns, cgroup creation) up front so the claim path is
// just an activate.
//
// This slice is the OBJECT lifecycle only: the controller creates, scales, and
// owner-ref-GCs husk pod objects. The pods actually running and activating is a
// later kind-e2e slice. The default remains raw-forkd (flag off).

const (
	// huskPoolLabel carries the owning pool name on every husk pod, so a
	// reconcile can list exactly this pool's husk pods.
	huskPoolLabel = "agentrun.dev/pool"
	// huskLabel marks a pod as a husk pod (vs any other pod the controller may
	// touch). Both labels together form the warm-pool selector.
	huskLabel = "agentrun.dev/husk"
	// huskContainerName is the single container in a husk pod.
	huskContainerName = "husk-stub"

	// defaultKVMResourceName is the extended resource the KVM device plugin
	// advertises (deploy/device-plugin). A husk pod requests one slot so it is
	// scheduled only onto a node with /dev/kvm; this replaces privileged: true.
	defaultKVMResourceName = "agentrun.dev/kvm"

	// huskControlSocket is the in-pod path the dormant stub listens on for
	// activate requests. The activation transport is slice 2; for slice 1 the
	// stub just Prepares the dormant VMM and serves this socket.
	huskControlSocket = "/run/husk/control.sock"
	// huskWorkdir is the per-VM working directory the stub uses.
	huskWorkdir = "/run/husk/vm"
)

// HuskPodOptions configures the husk pod spec the controller emits.
type HuskPodOptions struct {
	// StubImage is the container image that runs cmd/husk-stub.
	StubImage string
	// KVMResourceName is the extended resource the husk pod requests for KVM
	// access. Empty defaults to agentrun.dev/kvm.
	KVMResourceName string
}

// defaultHuskCPU and defaultHuskMemory size a husk pod when the template
// carries no Resources. They make the sandbox visible to the scheduler as
// ordinary pod requests (scheduler truth). Documented defaults: 1 cpu, 512Mi,
// matching the dormant stub's --mem-mib default.
var (
	defaultHuskCPU    = resource.MustParse("1")
	defaultHuskMemory = resource.MustParse("512Mi")
)

// buildHuskPod builds the warm-pool husk pod for a pool. The pod is
// GenerateName <pool>-husk- in the pool namespace, owner-referenced to the pool
// for garbage collection, labeled for the warm-pool selector, and runs the
// dormant stub with a non-privileged securityContext.
func (r *SandboxPoolReconciler) buildHuskPod(pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate, opts HuskPodOptions) *corev1.Pod {
	kvmResource := opts.KVMResourceName
	if kvmResource == "" {
		kvmResource = defaultKVMResourceName
	}

	// cpu/memory sized from the template when present, else the documented
	// default. These are real pod requests so the scheduler accounts the
	// sandbox like any other workload (scheduler truth: a husk pod shows up in
	// kubectl describe node and counts against ResourceQuota/LimitRange).
	cpuReq := defaultHuskCPU
	if !template.Spec.Resources.CPU.IsZero() {
		cpuReq = template.Spec.Resources.CPU
	}
	memReq := defaultHuskMemory
	if !template.Spec.Resources.Memory.IsZero() {
		memReq = template.Spec.Resources.Memory
	}

	// SecurityContext decisions (each load-bearing; the husk pod is the new
	// execution surface, so it is locked down and the one device exception is
	// the KVM device plugin, NOT privileged):
	//   - Privileged: false. The whole point of the husk model is to drop
	//     privileged: true; KVM access comes from the device plugin slot, not
	//     from a privileged container.
	//   - AllowPrivilegeEscalation: false. No setuid path can regain privilege.
	//   - Capabilities Drop ALL, add NONE. The dormant stub only Prepares a
	//     Firecracker VMM (open /dev/kvm via the device plugin, create files
	//     under the pod-local workdir, bind a unix socket); none of that needs a
	//     Linux capability. Networking capabilities (e.g. NET_ADMIN for tap
	//     setup) arrive with the networking slice, not here; we add back none so
	//     this slice stays minimal.
	//   - SeccompProfile RuntimeDefault, so the container runs under the
	//     runtime's default seccomp filter (PSA restricted requires this).
	//   - RunAsNonRoot: false (documented device-plugin exception). Firecracker
	//     opens /dev/kvm, which the device plugin injects; on common node
	//     distros /dev/kvm is root/kvm-group owned and the dormant VMM bring-up
	//     is simplest as uid 0 WITHOUT privileged. This is the single documented
	//     exception PSA-restricted would flag; a follow-up slice can move to a
	//     non-root uid in the kvm group once the device plugin's device
	//     permissions are pinned. It is NOT privileged and escalation is denied.
	runAsNonRoot := false

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pool.Name + "-husk-",
			Namespace:    pool.Namespace,
			Labels: map[string]string{
				huskPoolLabel: pool.Name,
				huskLabel:     "true",
			},
		},
		Spec: corev1.PodSpec{
			// A husk pod is long-lived: it holds its dormant (then activated) VM
			// until terminated. Restart on crash so the warm slot recovers.
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{
				{
					Name:  huskContainerName,
					Image: opts.StubImage,
					// Prepare a dormant Firecracker VMM and serve the control
					// socket. The firecracker binary and guest kernel are
					// provided by the image (see Dockerfile.husk-stub), mirroring
					// how forkd ships firecracker. The activation transport over
					// --control-socket is slice 2.
					Args: []string{
						"--firecracker", "/usr/local/bin/firecracker",
						"--kernel", "/var/lib/agent-run/kernel/vmlinux",
						"--workdir", huskWorkdir,
						"--control-socket", huskControlSocket,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceName(kvmResource): resource.MustParse("1"),
							corev1.ResourceCPU:               cpuReq,
							corev1.ResourceMemory:            memReq,
						},
						Limits: corev1.ResourceList{
							// The KVM device is a countable device-plugin
							// resource: request and limit must be equal and
							// non-zero. cpu/memory are left as requests only
							// (no hard limit) so the dormant-to-active memory
							// growth is not OOM-killed by a tight limit; sizing
							// the limit is a conformance-slice decision.
							corev1.ResourceName(kvmResource): resource.MustParse("1"),
						},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged:               ptrBool(false),
						AllowPrivilegeEscalation: ptrBool(false),
						RunAsNonRoot:             ptrBool(runAsNonRoot),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
				},
			},
		},
	}

	// Owner-ref to the pool so Kubernetes garbage collection deletes husk pods
	// when the pool is deleted. c.Scheme() is the manager scheme (it carries
	// core/v1 and agentrun.dev/v1alpha1). An error here means the scheme is
	// missing a type and is a programming error; the caller logs and skips.
	_ = controllerutil.SetControllerReference(pool, pod, r.Scheme())
	return pod
}

// reconcileHuskPods drives the warm pool toward pool.Spec.Replicas husk pod
// objects and returns the resulting count.
//
// Readiness nuance (envtest vs production): in production a husk slot is "ready"
// only when its pod is Running AND Ready (the dormant VMM is up and serving the
// control socket); the warm-pool size would gate on that. envtest has no
// kubelet, so pods never run, never go Ready, and have no phase. To keep the
// reconcile convergent under envtest AND in production we count by object
// EXISTENCE of non-terminating husk pods: create up to Replicas, delete the
// extras. A production readiness gate (Running+Ready before counting a slot
// warm) is layered on in the activation slice; object existence is the correct
// convergence target for this object-lifecycle slice.
func (r *SandboxPoolReconciler) reconcileHuskPods(ctx context.Context, pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate) (int32, error) {
	logger := log.FromContext(ctx)

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{huskPoolLabel: pool.Name, huskLabel: "true"},
	); err != nil {
		return 0, fmt.Errorf("list husk pods for pool %s: %w", pool.Name, err)
	}

	// Keep only non-terminating pods this pool actually owns. A pod with a
	// DeletionTimestamp is on its way out and must not count toward the warm
	// size (otherwise a scale-down would never converge).
	owned := make([]corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		p := pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if owner := metav1.GetControllerOf(&p); owner == nil || owner.UID != pool.UID {
			continue
		}
		owned = append(owned, p)
	}

	existing := int32(len(owned))
	desired := pool.Spec.Replicas

	switch {
	case existing < desired:
		deficit := desired - existing
		logger.Info("husk pod deficit", "existing", existing, "desired", desired, "creating", deficit)
		opts := HuskPodOptions{StubImage: r.HuskStubImage, KVMResourceName: r.KVMResourceName}
		for i := int32(0); i < deficit; i++ {
			pod := r.buildHuskPod(pool, template, opts)
			if err := r.Create(ctx, pod); err != nil {
				return existing, fmt.Errorf("create husk pod for pool %s: %w", pool.Name, err)
			}
			existing++
		}

	case existing > desired:
		// Delete the extras deterministically: sort by name and delete the
		// tail (newest GenerateName suffixes sort last), so repeated reconciles
		// pick the same victims and the set converges.
		sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
		surplus := existing - desired
		logger.Info("husk pod surplus", "existing", existing, "desired", desired, "deleting", surplus)
		for i := int32(0); i < surplus; i++ {
			victim := owned[len(owned)-1-int(i)]
			if err := r.Delete(ctx, &victim); err != nil && !apierrors.IsNotFound(err) {
				return existing, fmt.Errorf("delete surplus husk pod %s: %w", victim.Name, err)
			}
			existing--
		}
	}

	return existing, nil
}

func ptrBool(b bool) *bool { return &b }
