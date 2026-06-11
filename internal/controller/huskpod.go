package controller

import (
	"context"
	"fmt"
	"path/filepath"
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

	// huskWorkdir is the per-VM working directory the stub uses.
	huskWorkdir = "/run/husk/vm"

	// huskClaimLabel marks a husk pod as claimed by a specific SandboxClaim.
	// Selection skips any pod carrying it: one claim activates one husk pod.
	huskClaimLabel = "agentrun.dev/claim"

	// huskKVMNodeLabel is the node label the KVM device plugin / node bootstrap
	// sets on a node that has /dev/kvm (deploy/talos). A husk pod is pinned to
	// such a node so the dormant VMM can open KVM AND so it lands where the
	// template snapshot is materialized (the pool's build/distribution machinery
	// places the snapshot on these nodes; see the placement note below).
	huskKVMNodeLabel = "agentrun.dev/kvm"

	// HuskControlPort is the fixed TCP port the husk stub serves the mTLS network
	// control on (--control-listen). The controller dials podIP:HuskControlPort
	// to activate. Exported so cmd/controller can pass the same port to the claim
	// reconciler.
	HuskControlPort = 9443

	// huskSandboxPort is the in-pod port the activated VM's sandbox HTTP API is
	// reachable on (exec/files). The claim's Status.Endpoint is podIP:this, the
	// same shape forkd's HTTPEndpoint uses (forkd_discovery defaults 9091).
	huskSandboxPort = 9091

	// In-pod paths the stub's TLS, snapshot, and kernel mounts land on. The
	// snapshot mount is the directory the ActivateRequest.SnapshotDir points at:
	// the stub reads SnapshotDir/mem and SnapshotDir/vmstate (husk/control.go),
	// which is the forkd snapshot subdir <dataDir>/templates/<id>/snapshot. The
	// leaf cert/key and the CA are SEPARATE Secrets (the CA private key must never
	// reach the husk pod), mirroring the forkd DaemonSet's /etc/forkd/tls +
	// /etc/forkd/ca split.
	huskTLSMountPath      = "/etc/husk/tls"
	huskCAMountPath       = "/etc/husk/ca"
	huskSnapshotMountPath = "/var/lib/agent-run/snapshot"
	huskKernelMountPath   = "/var/lib/agent-run/kernel/vmlinux"
	// huskManifestMountPath is the in-pod path the recorded CAS manifest is
	// mounted at (read-only). The stub decodes it, binds it to the activate
	// request's ExpectedDigest, re-hashes the loaded snapshot files against it,
	// and runs the snapcompat check, all BEFORE loading the snapshot. This is the
	// husk mirror of forkd's verify-on-load gate (issues #9 and #32). The manifest
	// is a content-addressed artifact, not a secret.
	huskManifestMountPath = "/var/lib/agent-run/manifest.json"
)

// HuskSnapshotDir is the in-pod path the husk stub treats as ActivateRequest
// .SnapshotDir: the mounted forkd snapshot subdir holding mem and vmstate. The
// claim reconciler threads this into the activate request.
const HuskSnapshotDir = huskSnapshotMountPath

// HuskPodOptions configures the husk pod spec the controller emits.
type HuskPodOptions struct {
	// StubImage is the container image that runs cmd/husk-stub.
	StubImage string
	// KVMResourceName is the extended resource the husk pod requests for KVM
	// access. Empty defaults to agentrun.dev/kvm.
	KVMResourceName string
	// SnapshotID names the template snapshot the husk pod activates. It is the
	// template id; the node-local snapshot lives at
	// <DataDir>/templates/<SnapshotID>/snapshot. Empty means no snapshot mount is
	// added (the pod cannot activate; only meaningful with the activation slice).
	SnapshotID string
	// DataDir is the forkd data directory on the node (default /var/lib/agent-run).
	// The snapshot hostPath is rooted here. Empty defaults to the forkd default.
	DataDir string
	// ExpectedDigest is the template's recorded CAS manifest digest, as reported
	// by forkd via GetCapacity (the NodeRegistry TemplateDigests). When set, the
	// husk pod mounts the recorded manifest from <DataDir>/cas/manifests/<digest>
	// read-only and runs the stub with verify enforced (--manifest); the stub
	// re-verifies the snapshot against it before loading (fail-closed). Empty means
	// no manifest mount and the stub runs the development escape hatch
	// (--allow-unverified-snapshots) so a pre-digest pool still activates; this is
	// the only non-fail-closed path and is logged loudly by the stub.
	ExpectedDigest string
	// TLSSecretName is the Secret holding the husk stub's mTLS server leaf
	// (tls.crt, tls.key), mounted read-only so the stub can serve the mTLS network
	// control. This mirrors how forkd gets its leaf from a mounted PKI Secret
	// (agent-run-forkd-tls). Empty means no TLS mount is added.
	TLSSecretName string
	// CASecretName is the Secret holding the control plane CA (ca.crt only),
	// mounted read-only so the stub can verify the controller client cert. Kept
	// separate from the leaf so the CA private key never reaches the husk pod,
	// mirroring the forkd DaemonSet's /etc/forkd/ca split. Empty means no CA mount.
	CASecretName string
	// SnapshotNodes is the set of node hostnames the pool has materialized the
	// template snapshot on (the registry's NodesWithTemplate). When non-empty the
	// husk pod carries a nodeAffinity pinning it to exactly these nodes, so its
	// read-only snapshot hostPath always resolves. PLACEMENT COUPLING: the pool
	// reconcile builds the snapshot on these same nodes before creating husk pods.
	// When empty the pod falls back to the kvm nodeSelector alone (the
	// build-on-all-kvm-nodes coupling: the snapshot is on every kvm node).
	SnapshotNodes []string
}

// hostnameNodeLabel is the well-known node label carrying the node's hostname.
// A husk pod's nodeAffinity matches it against the snapshot-holding nodes so the
// pod lands only where the template snapshot exists.
const hostnameNodeLabel = "kubernetes.io/hostname"

// defaultDataDir is the forkd data directory default; the snapshot hostPath is
// rooted here when HuskPodOptions.DataDir is empty (matches cmd/forkd's
// --data-dir default).
const defaultDataDir = "/var/lib/agent-run"

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
	// execution surface, so it is locked down and the device exception is the KVM
	// device plugin, NOT privileged).
	//
	// PSA AUDIT (empirically verified against the v1.31 PodSecurity admission
	// plugin on kind, proven object-level in the kind-e2e conformance job): the
	// husk pod's securityContext satisfies EVERY restricted control, but the husk
	// pod is NOT admitted into a baseline or restricted namespace, for exactly two
	// DOCUMENTED EXCEPTIONS, both intrinsic to the husk model:
	//   1. the read-only node hostPaths. hostPath is forbidden under BOTH baseline
	//      and restricted (the "HostPath Volumes" / "Volume Types" controls); the
	//      husk pod mounts the node's read-only template snapshot (mem+vmstate) so
	//      the dormant VMM can load it, the guest kernel, and (when the pool has a
	//      recorded digest) the read-only CAS manifest the stub verifies the
	//      snapshot against before loading. These are all the same node-hostPath
	//      exception category (read-only, intrinsic to the node-local snapshot
	//      model); none is writable.
	//   2. runAsNonRoot=false. restricted requires runAsNonRoot=true; the husk pod
	//      runs uid 0 so Firecracker can open the device-plugin-injected /dev/kvm
	//      WITHOUT privileged (the /dev/kvm device exception).
	// So the HONEST claim is: the husk pod is "restricted EXCEPT the read-only
	// snapshot hostPath + runAsNonRoot-false (the /dev/kvm device) exceptions". Its
	// securityContext is restricted-clean: with those two exceptions removed the
	// SAME securityContext is admitted into a restricted namespace (verified on
	// kind). The agentrun.dev/kvm device-plugin resource replaces privileged: true.
	//
	// The individual controls the husk pod DOES satisfy:
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
	//   - SeccompProfile RuntimeDefault, set at BOTH the pod and the container
	//     securityContext level. restricted checks the profile at the pod OR the
	//     container level; setting both keeps the pod-level control satisfied even
	//     if a future container is added without its own profile.
	//   - RunAsNonRoot: false (the documented /dev/kvm device exception above),
	//     set at both the pod and the container level. A follow-up slice can move
	//     to a non-root uid in the kvm group once the device plugin's device
	//     permissions are pinned. It is NOT privileged and escalation is denied.
	runAsNonRoot := false

	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = defaultDataDir
	}

	// The stub args. The husk pod serves the mTLS NETWORK control on
	// HuskControlPort (not the unix --control-socket the in-CI driver uses): the
	// controller dials podIP:HuskControlPort to activate. The three TLS PEM paths
	// point at the mounted PKI Secret (mirrors how forkd reads its leaf + CA from
	// a mounted Secret). The kernel and snapshot are read-only mounts below.
	args := []string{
		"--firecracker", "/usr/local/bin/firecracker",
		"--kernel", huskKernelMountPath,
		"--workdir", huskWorkdir,
		"--control-listen", fmt.Sprintf(":%d", HuskControlPort),
		// Serve the in-pod sandbox HTTP API (exec/files) on the declared sandbox
		// container port after activation, gated by the per-sandbox bearer token
		// delivered over the control channel. The claim's Status.Endpoint is
		// podIP:huskSandboxPort, so the stub must serve there.
		"--sandbox-listen", fmt.Sprintf(":%d", huskSandboxPort),
		"--tls-cert", filepath.Join(huskTLSMountPath, "tls.crt"),
		"--tls-key", filepath.Join(huskTLSMountPath, "tls.key"),
		"--tls-ca", filepath.Join(huskCAMountPath, "ca.crt"),
	}

	// Snapshot verify gate (fail-closed): when the pool has a recorded template
	// digest, mount the recorded CAS manifest and point the stub at it so it
	// re-verifies the snapshot (digest + snapcompat) before loading. Without a
	// recorded digest (a pool whose snapshot has not been content-addressed yet)
	// fall back to the development escape hatch so the warm pool still activates;
	// the stub logs this loudly. The manifest mount itself is added in the snapshot
	// block below (it shares the snapshot placement requirement).
	if opts.ExpectedDigest != "" {
		args = append(args, "--manifest", huskManifestMountPath)
	} else {
		args = append(args, "--allow-unverified-snapshots")
	}

	// Volumes + mounts: the mTLS Secret, the node's template snapshot subdir
	// (read-only hostPath; the stub reads SnapshotDir/{mem,vmstate}), and the
	// guest kernel. PLACEMENT REQUIREMENT: the snapshot hostPath assumes the
	// template snapshot is materialized on this pod's node. The pod is pinned to a
	// KVM node (nodeSelector below); the pool's existing snapshot
	// build/distribution machinery must ensure the snapshot is present on those
	// nodes. A refinement (CAS-pull the snapshot into the pod) removes the
	// hostPath dependency; documented as a follow-up.
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if opts.TLSSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "husk-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: opts.TLSSecretName},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "husk-tls", MountPath: huskTLSMountPath, ReadOnly: true})
	}
	if opts.CASecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "husk-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: opts.CASecretName,
					// Only the CA certificate is projected; the CA private key in
					// this Secret must never reach the husk pod.
					Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "husk-ca", MountPath: huskCAMountPath, ReadOnly: true})
	}
	if opts.SnapshotID != "" {
		hostType := corev1.HostPathDirectory
		volumes = append(volumes, corev1.Volume{
			Name: "snapshot",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: filepath.Join(dataDir, "templates", opts.SnapshotID, "snapshot"),
					Type: &hostType,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "snapshot", MountPath: huskSnapshotMountPath, ReadOnly: true})

		fileType := corev1.HostPathFile

		// The recorded CAS manifest, mounted read-only so the stub can re-verify
		// the snapshot against it before loading (fail-closed). Only added when the
		// pool has a recorded digest; the file lives at
		// <dataDir>/cas/manifests/<digest> on the same node the snapshot is on.
		if opts.ExpectedDigest != "" {
			volumes = append(volumes, corev1.Volume{
				Name: "snapshot-manifest",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: filepath.Join(dataDir, "cas", "manifests", opts.ExpectedDigest),
						Type: &fileType,
					},
				},
			})
			mounts = append(mounts, corev1.VolumeMount{Name: "snapshot-manifest", MountPath: huskManifestMountPath, ReadOnly: true})
		}
		volumes = append(volumes, corev1.Volume{
			Name: "kernel",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: filepath.Join(dataDir, "vmlinux"),
					Type: &fileType,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "kernel", MountPath: huskKernelMountPath, ReadOnly: true})
	}

	// Placement: the dormant VMM needs /dev/kvm (the kvm nodeSelector) AND the
	// read-only snapshot hostPath must resolve, so the pod must land where the
	// pool materialized the template snapshot. When the pool passes the
	// snapshot-holding node hostnames (NodesWithTemplate), a required nodeAffinity
	// pins the pod to exactly those nodes; without it, the pod falls back to the
	// kvm nodeSelector alone (the snapshot is then assumed present on every kvm
	// node, the documented build-on-all-kvm-nodes coupling).
	var affinity *corev1.Affinity
	if len(opts.SnapshotNodes) > 0 {
		nodes := append([]string(nil), opts.SnapshotNodes...)
		sort.Strings(nodes)
		affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      hostnameNodeLabel,
							Operator: corev1.NodeSelectorOpIn,
							Values:   nodes,
						}},
					}},
				},
			},
		}
	}

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
			// POD-LEVEL securityContext. PSA restricted checks seccompProfile and
			// runAsNonRoot at the pod OR the container level; we set them at the pod
			// level too so the pod-level control is satisfied independently of any
			// container. seccompProfile is RuntimeDefault (a restricted control the
			// husk pod satisfies); runAsNonRoot mirrors the documented /dev/kvm
			// device exception (false). The two PSA exceptions that keep the husk
			// pod out of a restricted namespace are the read-only snapshot hostPath
			// and this runAsNonRoot=false, both documented above.
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptrBool(runAsNonRoot),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			// Pin to a KVM node: the dormant VMM needs /dev/kvm AND the pod must
			// land where the template snapshot hostPath exists. The nodeAffinity
			// above narrows further to the snapshot-holding nodes when known.
			NodeSelector: map[string]string{huskKVMNodeLabel: "true"},
			Affinity:     affinity,
			Volumes:      volumes,
			Containers: []corev1.Container{
				{
					Name:  huskContainerName,
					Image: opts.StubImage,
					// Prepare a dormant Firecracker VMM and serve the mTLS network
					// control. The firecracker binary is provided by the image
					// (see Dockerfile.husk-stub); the guest kernel and the template
					// snapshot are read-only hostPath mounts. The controller dials
					// the control port to activate (slice 2).
					Args: args,
					Ports: []corev1.ContainerPort{{
						// The activated VM's sandbox HTTP API (exec/files). The
						// claim's Status.Endpoint is podIP:this, so it must be a
						// declared container port to be reachable.
						Name:          "sandbox",
						ContainerPort: huskSandboxPort,
						Protocol:      corev1.ProtocolTCP,
					}},
					VolumeMounts: mounts,
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
		opts := HuskPodOptions{
			StubImage:       r.HuskStubImage,
			KVMResourceName: r.KVMResourceName,
			SnapshotID:      pool.Spec.TemplateRef.Name,
			DataDir:         r.DataDir,
			TLSSecretName:   r.HuskTLSSecretName,
			CASecretName:    r.HuskCASecretName,
			// The recorded snapshot manifest digest, so the husk pod mounts the
			// manifest and the stub verifies the snapshot before loading
			// (fail-closed). Empty (no node has reported it yet) falls back to the
			// stub's development escape hatch so the warm pool still activates.
			ExpectedDigest: r.huskTemplateDigest(pool.Spec.TemplateRef.Name),
			// Pin husk pods to the nodes the pool built the snapshot on, so the
			// read-only snapshot hostPath resolves. Empty (no registry, or no node
			// holds it yet) falls back to the kvm nodeSelector alone.
			SnapshotNodes: r.snapshotNodeNames(pool.Spec.TemplateRef.Name),
		}
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

// huskPodReady reports whether a husk pod is a usable dormant slot: Running,
// with a Ready condition True, and a non-empty PodIP (so the controller can
// dial its control channel and set a reachable endpoint).
func huskPodReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// selectDormantHuskPod returns one Running+Ready husk pod for the pool that has
// a PodIP and is not yet claimed (no agentrun.dev/claim label). It is the warm
// slot the claim path activates. Returns nil (no error) when none is available,
// so the caller pends the claim. Selection is deterministic (lowest name) so
// concurrent reconciles converge on the same victim; the optimistic-lock
// claim-label patch in markHuskPodClaimed is the real commit: two concurrent
// claims that both select the SAME pod both attempt the patch, but the patch
// carries the pod's resourceVersion so exactly one wins and the loser gets a 409
// Conflict and requeues to pick a different dormant pod. A pod is therefore
// claimed (and activated) by exactly one claim.
func (r *SandboxClaimReconciler) selectDormantHuskPod(ctx context.Context, pool *v1alpha1.SandboxPool) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{huskPoolLabel: pool.Name, huskLabel: "true"},
	); err != nil {
		return nil, fmt.Errorf("list husk pods for pool %s: %w", pool.Name, err)
	}

	var candidates []corev1.Pod
	for i := range pods.Items {
		p := pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Labels[huskClaimLabel] != "" {
			continue
		}
		if !huskPodReady(&p) {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	chosen := candidates[0]
	return &chosen, nil
}

// markHuskPodClaimed stamps the agentrun.dev/claim label on a husk pod so it is
// not selected again. It uses an OPTIMISTIC-LOCK merge patch: the patch carries
// the pod's resourceVersion, so the API server rejects it with a 409 Conflict if
// the pod was modified (for instance, claimed by a racing reconcile) since this
// reconcile read it. This is the mutual-exclusion guarantee: two concurrent
// claims that both selected the same dormant pod both attempt this patch, but
// only one wins; the other gets apierrors.IsConflict and must NOT activate this
// pod (the caller requeues to pick a different dormant pod). The label-only
// patch still merges cleanly with concurrent kubelet status writes (status is a
// separate subresource), so the optimistic lock fires only on a genuine
// metadata race, which is exactly the double-assignment it must prevent.
func (r *SandboxClaimReconciler) markHuskPodClaimed(ctx context.Context, pod *corev1.Pod, claimName string) error {
	patch := client.MergeFromWithOptions(pod.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[huskClaimLabel] = claimName
	if err := r.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("mark husk pod %s claimed by %s: %w", pod.Name, claimName, err)
	}
	return nil
}
