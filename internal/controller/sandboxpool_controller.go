package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/kms"
	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type SandboxPoolReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry
	// PeerToken is the shared bearer credential forkd accepts on its token-gated
	// CAS surface. The controller passes it in every PullTemplate so a deficit
	// node can pull a template from a holder. It must match forkd's --peer-token.
	// Empty disables distribution by pull (every deficit node builds its own
	// snapshot, the prior behavior). A SECRET VALUE: it is never logged. A
	// per-pull minted token is a follow-up; the shared-token model matches the
	// forkd side (Task 1).
	PeerToken string

	// EnableHuskPods selects the husk pod warm-pool path (issue #18), the
	// pod-native default. When true the pool does BOTH: it builds the template
	// snapshot on the target nodes (createSnapshotsOnNodes) AND maintains a warm
	// pool of pre-scheduled husk pods, pinned to the snapshot-holding nodes, that
	// run the dormant-VMM stub. When false the raw-forkd fork-per-claim path runs:
	// the snapshot is built and each claim forks on a holder, no husk pods. In
	// cmd/controller this is true by default and turned off by --enable-raw-forkd.
	EnableHuskPods bool
	// HuskStubImage is the container image that runs cmd/husk-stub in a husk
	// pod. Only used when EnableHuskPods is true.
	HuskStubImage string
	// KVMResourceName is the extended resource a husk pod requests for KVM
	// access (the device plugin slot, not privileged: true). Empty defaults to
	// mitos.run/kvm. Only used when EnableHuskPods is true.
	KVMResourceName string
	// DataDir is the forkd data directory on the node; the husk pod's snapshot
	// hostPath is rooted here (<DataDir>/templates/<id>/snapshot). Empty defaults
	// to /var/lib/mitos. Only used when EnableHuskPods is true.
	DataDir string
	// HuskMemoryHeadroom is the FIXED-FLOOR memory headroom added on top of a
	// husk pod's memory request to size its memory LIMIT (production-blocker #2,
	// cap 1). The limit must include headroom because the cgroup that the limit
	// caps holds MORE than the guest's configured RAM: the Firecracker VMM
	// itself, the husk-stub process, and copy-on-write dirty-page slack as the
	// restored VM faults in and writes pages. A limit equal to the request would
	// OOM-kill a VM running normally at its configured RAM (destroying the
	// activate latency); the headroom is what makes the limit transparent to a
	// legitimate VM while still capping a runaway. The effective headroom is
	// max(this floor, HuskMemoryHeadroomPercent% of the request) so a large VM
	// gets proportional slack and a small VM gets at least the floor. Zero
	// selects the default floor (defaultHuskMemoryHeadroom, 256Mi). Tunable via
	// the controller --husk-memory-headroom flag.
	HuskMemoryHeadroom resource.Quantity
	// HuskMemoryHeadroomPercent is the PROPORTIONAL memory headroom (percent of
	// the memory request) considered alongside HuskMemoryHeadroom; the larger of
	// the two is used. Zero selects the default (defaultHuskMemoryHeadroomPercent,
	// 25). Tunable via the controller --husk-memory-headroom-percent flag.
	HuskMemoryHeadroomPercent int
	// HuskTLSSecretName is the Secret holding the husk stub's mTLS server leaf
	// (tls.crt, tls.key), mounted into each husk pod so the stub can serve the
	// mTLS network control. Only used when EnableHuskPods is true.
	HuskTLSSecretName string
	// HuskCASecretName is the Secret holding the control plane CA (ca.crt),
	// mounted into each husk pod so the stub verifies the controller client cert.
	// Only used when EnableHuskPods is true.
	HuskCASecretName string
	// ControllerNamespace is the namespace EnsurePKI materialized the control
	// plane PKI Secrets in (the controller's own namespace, default "mitos").
	// reconcileHuskPods replicates mitos-ca + mitos-forkd-tls FROM here INTO the
	// pool namespace so husk pods, which run in the pool namespace, can mount
	// them. Empty disables replication (the husk pods then require the secrets
	// to already exist in their namespace). Only used when EnableHuskPods.
	ControllerNamespace string

	// KMS is the envelope-encryption Wrapper that wraps a template's at-rest DEK
	// (the controller never persists the plaintext DEK). It is REQUIRED when any
	// reconciled template is Encrypted; EnsureEncKey fails closed if it is nil.
	// Built from the controller --kek-file (local AES-256-GCM KEK in dev/CI; a
	// cloud KMS provider is a documented follow-up).
	KMS kms.Wrapper
}

// SandboxPool ownership: get/list/watch to reconcile, status to write warmed
// counts and conditions. SandboxTemplate is read-only (covered above). The
// husk pod warm-pool path (issue #18) creates and deletes Pods, so the
// reconciler needs create;delete on pods on top of the get;list;watch the forkd
// discovery already declares.
// The husk warm pool is bounded against voluntary disruption (node drain,
// eviction) by a PodDisruptionBudget the reconciler creates-or-updates per pool
// (issue #18, slice 4b), so it needs the policy/poddisruptionbudgets verbs.
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=create;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pool v1alpha1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var template v1alpha1.SandboxTemplate
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: pool.Namespace,
		Name:      pool.Spec.TemplateRef.Name,
	}, &template); err != nil {
		logger.Error(err, "template not found", "template", pool.Spec.TemplateRef.Name)
		return ctrl.Result{}, err
	}

	templateID := pool.Spec.TemplateRef.Name
	desired := pool.Spec.Replicas

	// Husk pod warm-pool path (issue #18, the pod-native default). When enabled,
	// the pool does TWO INDEPENDENT things: it maintains a warm pool of
	// pre-scheduled DORMANT husk pods, AND it builds the template snapshot on the
	// target nodes the pods activate against. These two are DECOUPLED: the warm
	// pool of husk pods is maintained to Replicas REGARDLESS of whether the
	// snapshot is built yet. The husk pods schedule dormant and cannot ACTIVATE
	// until the snapshot is present on their node, but the pool of pod objects
	// must exist and self-heal independent of the build (a deleted husk pod is
	// recreated even while the build is incomplete or failing).
	//
	// ORDERING: reconcileHuskPods runs FIRST and is NEVER gated by the build, so a
	// build that cannot complete (for example a node that cannot boot a VM to
	// snapshot) does not stall warm-pool maintenance or self-heal. The build then
	// runs best-effort: its result is reported in status and a failure only
	// requeues to keep trying; it never short-circuits before the warm pool is
	// maintained.
	//
	// PLACEMENT COUPLING: when a snapshot-holding node is known (NodesWithTemplate),
	// each husk pod is pinned to it via nodeAffinity; when no holder exists yet
	// (build incomplete), the husk pods fall back to the kvm nodeSelector and a
	// later reconcile tightens the affinity once the registry reports the holders.
	// The raw-forkd path below (flag off, behind --enable-raw-forkd) is the
	// fork-per-claim fallback and does NOT create husk pods.
	if r.EnableHuskPods {
		// Warm pool FIRST, unconditionally: maintain the husk pod count to
		// Replicas and self-heal a deleted slot. This is decoupled from the
		// snapshot build so a build that does not complete never blocks the warm
		// pool. A reconcileHuskPods error (an API failure listing/creating pods)
		// requeues; the build is then skipped this pass and retried on the requeue.
		warm, err := r.reconcileHuskPods(ctx, &pool, &template)
		if err != nil {
			logger.Error(err, "failed to reconcile husk pods")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		// Build the snapshot the husk pods activate against, BEST-EFFORT. A build
		// error is logged and reported in status, and we requeue to keep trying,
		// but it does NOT return before the warm pool was maintained above. On a
		// node that cannot snapshot (the documented nested-VMM boundary on kind)
		// the build never completes, yet the warm pool above is still maintained
		// and self-heals.
		buildErr := r.ensureTemplateBuilt(ctx, &pool, &template)
		if buildErr != nil {
			logger.Error(buildErr, "failed to build template snapshot for husk pool (warm pool still maintained)")
		}

		// Bound voluntary disruption of the warm pool: a PodDisruptionBudget so a
		// node drain evicts husk pods one at a time instead of collapsing the pool.
		// A PDB error is logged and we requeue; it does not block the warm-pool
		// status update (the pool is still functional, just unbounded on drain).
		if err := r.ensureHuskPDB(ctx, &pool); err != nil {
			logger.Error(err, "failed to ensure husk PDB")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		readySnapshots := r.readySnapshotCount(templateID)
		setPoolReadySnapshots(pool.Name, readySnapshots)
		pool.Status.ReadySnapshots = readySnapshots
		pool.Status.TotalSnapshots = readySnapshots
		pool.Status.NodeDistribution = r.nodeDistribution(templateID)
		if digest, ok := r.NodeRegistry.TemplateDigest(templateID); ok {
			pool.Status.TemplateDigest = digest
		}
		now := metav1.Now()
		pool.Status.LastSnapshotTime = &now
		setCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             conditionStatus(warm >= desired && readySnapshots > 0),
			LastTransitionTime: now,
			Reason:             "HuskPodsReady",
			Message:            fmt.Sprintf("%d/%d husk pods, %d snapshot node(s)", warm, desired, readySnapshots),
		})
		if err := r.Status().Update(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
		// Bounded periodic requeue so the warm pool re-converges to Replicas even
		// if a husk pod DELETE event is somehow missed (Owns(pods) normally
		// enqueues the pool on delete, but the requeue is the belt-and-suspenders
		// guarantee that self-heal is not event-dependent). When the snapshot is
		// not built yet (no holder, or the build errored) requeue sooner to keep
		// driving the build AND to tighten the husk pod nodeAffinity once a holder
		// appears; once everything is ready, fall back to the slower steady cadence.
		if buildErr != nil || readySnapshots < desired {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Raw-forkd path (the --enable-raw-forkd fallback): build the snapshot on the
	// target nodes and let each claim fork on a holder. No husk pods.
	if r.readySnapshotCount(templateID) < desired {
		logger.Info("snapshot deficit", "ready", r.readySnapshotCount(templateID), "desired", desired)
		if err := r.ensureTemplateBuilt(ctx, &pool, &template); err != nil {
			logger.Error(err, "failed to create snapshots")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	readySnapshots := r.readySnapshotCount(templateID)

	// Update status
	setPoolReadySnapshots(pool.Name, readySnapshots)
	pool.Status.ReadySnapshots = readySnapshots
	pool.Status.TotalSnapshots = readySnapshots
	pool.Status.NodeDistribution = r.nodeDistribution(templateID)
	if digest, ok := r.NodeRegistry.TemplateDigest(templateID); ok {
		pool.Status.TemplateDigest = digest
	}

	now := metav1.Now()
	pool.Status.LastSnapshotTime = &now
	setCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(readySnapshots >= desired),
		LastTransitionTime: now,
		Reason:             "SnapshotsReady",
		Message:            fmt.Sprintf("%d/%d snapshots ready", readySnapshots, desired),
	})

	if err := r.Status().Update(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// readySnapshotCount counts healthy nodes that hold the pool's template
// snapshot. One snapshot per node per template, so replicas are capped by
// node count.
func (r *SandboxPoolReconciler) readySnapshotCount(templateID string) int32 {
	return int32(len(r.NodeRegistry.NodesWithTemplate(templateID)))
}

// snapshotNodeNames returns the hostnames of the healthy nodes that hold the
// template snapshot, the set a husk pod's nodeAffinity is pinned to. A nil
// registry (some unit tests) returns nil, which leaves the husk pod on the kvm
// nodeSelector alone.
func (r *SandboxPoolReconciler) snapshotNodeNames(templateID string) []string {
	if r.NodeRegistry == nil {
		return nil
	}
	holders := r.NodeRegistry.NodesWithTemplate(templateID)
	names := make([]string, 0, len(holders))
	for _, n := range holders {
		names = append(names, n.Name)
	}
	return names
}

// huskTemplateDigest returns the recorded CAS manifest digest for the template,
// as reported by any healthy node holding it (forkd's GetCapacity feeds the
// NodeRegistry). The husk pod mounts the matching manifest and the stub verifies
// the snapshot against it before loading. A nil registry or no reported digest
// returns "", which makes the husk pod fall back to the stub's development
// escape hatch (the warm pool still activates, the stub logs it loudly).
func (r *SandboxPoolReconciler) huskTemplateDigest(templateID string) string {
	if r.NodeRegistry == nil {
		return ""
	}
	if d, ok := r.NodeRegistry.TemplateDigest(templateID); ok {
		return d
	}
	return ""
}

// ensureTemplateBuilt drives the template snapshot toward pool.Spec.Replicas
// holder nodes using the same build/distribute path as the raw-forkd pool
// (createSnapshotsOnNodes). It is the FIRST half of a husk-mode reconcile: the
// husk pods that follow mount <dataDir>/templates/<id>/snapshot on a holder
// node, so the snapshot must exist there first. A no-op when the deficit is
// already met. The encrypted-template key handling matches the raw path: the
// per-template key Secret is owned here and delivered over mTLS, never logged.
func (r *SandboxPoolReconciler) ensureTemplateBuilt(ctx context.Context, pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate) error {
	templateID := pool.Spec.TemplateRef.Name
	readySnapshots := r.readySnapshotCount(templateID)
	if readySnapshots >= pool.Spec.Replicas {
		return nil
	}
	deficit := pool.Spec.Replicas - readySnapshots

	var wrappedDEK []byte
	var kekID string
	if template.Spec.Encrypted {
		var keyErr error
		wrappedDEK, kekID, keyErr = EnsureEncKey(ctx, r.Client, r.KMS, pool.Namespace, templateID, template)
		if keyErr != nil {
			// The error names only the Secret and the non-secret KEK id, never key
			// bytes.
			return fmt.Errorf("ensure encryption key for template %s: %w", templateID, keyErr)
		}
	}
	if _, err := r.createSnapshotsOnNodes(ctx, templateID, template.Spec.Image, template.Spec.Init, template.Spec.Volumes, wrappedDEK, kekID, deficit); err != nil {
		return fmt.Errorf("build template snapshot %s: %w", templateID, err)
	}
	return nil
}

// createSnapshotsOnNodes ensures the template is present on up to deficit
// additional healthy nodes and returns how many were added (built + pulled).
//
// Distribution policy (build once, distribute by pull):
//   - Encrypted template: every deficit node BUILDS its own snapshot
//     (CreateTemplate). The CAS chunks of a plaintext-on-the-wire pull would
//     defeat at-rest encryption, so encrypted templates are not distributed by
//     pull; this is the documented carve-out.
//   - Plaintext template: ensure the template is BUILT on at least one node
//     (CreateTemplate on the first eligible node when no node holds it yet),
//     then for the remaining deficit nodes that lack it, PULL the snapshot from
//     a holder's CAS surface instead of rebuilding it. A pull is O(network) and
//     reuses the one expensive build, so the fleet converges far faster than N
//     independent boots.
//
// The pull token is the shared peer credential the controller is configured
// with; it is delivered to the deficit node over its mTLS gRPC and is never
// logged.
func (r *SandboxPoolReconciler) createSnapshotsOnNodes(ctx context.Context, templateID, image string, initCommands []string, templateVolumes []v1alpha1.SandboxVolume, wrappedDEK []byte, kekID string, deficit int32) (int32, error) {
	var added int32
	var errs []error

	// Whether distribution by pull applies: plaintext template, a peer token is
	// configured, and a holder reporting a content-addressed digest exists. An
	// encrypted template always builds per node (the carve-out above).
	distribute := len(wrappedDEK) == 0 && r.PeerToken != ""

	for _, node := range r.NodeRegistry.ListNodes() {
		if added >= deficit {
			break
		}
		if !node.isHealthy() || node.hasSnapshot(templateID) {
			continue
		}

		// Prefer a pull when distribution applies AND a holder exists. The build
		// on the first node (when no holder exists yet) falls through to
		// CreateTemplate below; once that build registers a digest, subsequent
		// deficit nodes in this same pass pull from it.
		if distribute {
			if holder, casURL, digest, ok := r.NodeRegistry.TemplateSource(templateID); ok && holder.Name != node.Name {
				if err := r.pullTemplateOnNode(ctx, node, templateID, digest, casURL, r.PeerToken); err != nil {
					errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
					continue
				}
				r.NodeRegistry.AddTemplateWithDigest(node.Name, templateID, digest)
				added++
				continue
			}
		}

		// Build path. Fail closed: an encrypted template's WRAPPED DEK travels in
		// CreateTemplate, so the node connection must be mTLS. Refuse to send the
		// wrapped DEK over an insecure channel (node.TLS nil and registry.TLS nil,
		// i.e. PKI bootstrap disabled); skip the node without setting it and record
		// the refusal. A plaintext template carries no DEK and is unaffected.
		if len(wrappedDEK) > 0 && !r.NodeRegistry.NodeMTLS(node.Name) {
			errs = append(errs, fmt.Errorf("node %s: refusing to deliver the wrapped DEK over an insecure gRPC channel: enable PKI bootstrap on the controller and mTLS on forkd, or disable template encryption", node.Name))
			continue
		}
		conn, err := r.NodeRegistry.GetConnection(node.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// CreateTemplate on the real engine boots a VM and snapshots it:
		// O(minutes). This blocks the pool reconcile worker; bounded here so a
		// hung node cannot stall pool reconciliation forever. Moving builds to
		// a background queue is roadmap work (snapshot distribution).
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		// The template's declared volumes are baked into the snapshot as
		// placeholder drives. No fork-policy override applies at build time, so
		// volumeMounts is called with no overrides; each fork's ForkRequest must
		// match this set by name.
		resp, err := forkdpb.NewForkDaemonClient(conn).CreateTemplate(cctx, &forkdpb.CreateTemplateRequest{
			TemplateId:   templateID,
			Image:        image,
			InitCommands: initCommands,
			Volumes:      volumeMounts(templateVolumes, nil),
			// EncryptionKey carries the WRAPPED DEK for an Encrypted template,
			// delivered over mTLS; KekId names the KEK that wrapped it (non-secret)
			// so the node selects the matching KEK to unwrap. Both empty for a
			// plaintext template. The wrapped DEK is never logged.
			EncryptionKey: wrappedDEK,
			KekId:         kekID,
		})
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}
		r.NodeRegistry.AddTemplateWithDigest(node.Name, templateID, resp.TemplateDigest)
		added++
	}
	if added == 0 && len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	return added, nil
}

func (r *SandboxPoolReconciler) nodeDistribution(templateID string) map[string]int32 {
	dist := make(map[string]int32)
	for _, n := range r.NodeRegistry.NodesWithTemplate(templateID) {
		// One snapshot per node in the current model; becomes a real count when
		// snapshot distribution lands (ROADMAP §3).
		dist[n.Name] = 1
	}
	return dist
}

func (r *SandboxPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Owns(pods): a husk pod is owner-referenced to its pool, so a husk pod
	// delete (a node drain, an eviction, an operator kubectl delete) enqueues the
	// owning pool. The deficit logic in reconcileHuskPods then recreates the
	// replacement, so the warm pool SELF-HEALS a lost dormant slot without
	// waiting for the periodic 30s requeue. Owns(pods) also covers the
	// owner-referenced husk PodDisruptionBudget for free via the same ownership
	// edge for pods; the PDB itself is reconciled on the pool's own events.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SandboxPool{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

func setCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == condition.Type {
			(*conditions)[i] = condition
			return
		}
	}
	*conditions = append(*conditions, condition)
}

func conditionStatus(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}
