package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
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

	// EnableHuskPods selects the husk pod warm-pool path (issue #18, slice 1):
	// the pool maintains a warm pool of pre-scheduled husk pods running the
	// dormant-VMM stub instead of building node-local snapshots. Default false:
	// the existing raw-forkd createSnapshotsOnNodes path runs unchanged. The
	// pod-native default is a later migration slice.
	EnableHuskPods bool
	// HuskStubImage is the container image that runs cmd/husk-stub in a husk
	// pod. Only used when EnableHuskPods is true.
	HuskStubImage string
	// KVMResourceName is the extended resource a husk pod requests for KVM
	// access (the device plugin slot, not privileged: true). Empty defaults to
	// agentrun.dev/kvm. Only used when EnableHuskPods is true.
	KVMResourceName string
}

// SandboxPool ownership: get/list/watch to reconcile, status to write warmed
// counts and conditions. SandboxTemplate is read-only (covered above). The
// husk pod warm-pool path (issue #18) creates and deletes Pods, so the
// reconciler needs create;delete on pods on top of the get;list;watch the forkd
// discovery already declares.
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=create;delete
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

	// Husk pod warm-pool path (issue #18, slice 1). When enabled, the pool
	// maintains a warm pool of pre-scheduled husk pods instead of building
	// node-local snapshots; the snapshot-on-nodes deficit is skipped entirely.
	// readySnapshots is reused for the status count so the CRD reports the warm
	// pool size in the existing field. The default (flag off) leaves the
	// raw-forkd path below unchanged.
	if r.EnableHuskPods {
		warm, err := r.reconcileHuskPods(ctx, &pool, &template)
		if err != nil {
			logger.Error(err, "failed to reconcile husk pods")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		setPoolReadySnapshots(pool.Name, warm)
		pool.Status.ReadySnapshots = warm
		pool.Status.TotalSnapshots = warm
		now := metav1.Now()
		pool.Status.LastSnapshotTime = &now
		setCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             conditionStatus(warm >= desired),
			LastTransitionTime: now,
			Reason:             "HuskPodsReady",
			Message:            fmt.Sprintf("%d/%d husk pods", warm, desired),
		})
		if err := r.Status().Update(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	readySnapshots := r.readySnapshotCount(templateID)

	if readySnapshots < desired {
		deficit := desired - readySnapshots
		logger.Info("snapshot deficit", "ready", readySnapshots, "desired", desired, "creating", deficit)
		// When the template requests at-rest encryption, own and deliver its key.
		// EnsureEncKey creates-or-reads the per-template Secret (owner-referenced
		// to the template for GC); the key bytes go straight into the CreateTemplate
		// RPC over mTLS and are never logged. A plaintext template passes no key.
		var encKey []byte
		if template.Spec.Encrypted {
			var keyErr error
			encKey, keyErr = EnsureEncKey(ctx, r.Client, pool.Namespace, templateID, &template)
			if keyErr != nil {
				// The error names only the Secret, never key bytes.
				logger.Error(keyErr, "ensure encryption key for template", "template", templateID)
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
		}
		created, err := r.createSnapshotsOnNodes(ctx, templateID, template.Spec.Image, template.Spec.Init, template.Spec.Volumes, encKey, deficit)
		if err != nil {
			logger.Error(err, "failed to create snapshots")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		readySnapshots += created
	}

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
func (r *SandboxPoolReconciler) createSnapshotsOnNodes(ctx context.Context, templateID, image string, initCommands []string, templateVolumes []v1alpha1.SandboxVolume, encKey []byte, deficit int32) (int32, error) {
	var added int32
	var errs []error

	// Whether distribution by pull applies: plaintext template, a peer token is
	// configured, and a holder reporting a content-addressed digest exists. An
	// encrypted template always builds per node (the carve-out above).
	distribute := len(encKey) == 0 && r.PeerToken != ""

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

		// Build path. Fail closed: an encrypted template's key travels in
		// CreateTemplate, so the node connection must be mTLS. Refuse to send the
		// key in cleartext over an insecure channel (node.TLS nil and registry.TLS
		// nil, i.e. PKI bootstrap disabled); skip the node without setting the key
		// and record the refusal. A plaintext template carries no key and is
		// unaffected.
		if len(encKey) > 0 && !r.NodeRegistry.NodeMTLS(node.Name) {
			errs = append(errs, fmt.Errorf("node %s: refusing to deliver the encryption key over an insecure gRPC channel: enable PKI bootstrap on the controller and mTLS on forkd, or disable template encryption", node.Name))
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
			// EncryptionKey is the at-rest key for an Encrypted template, delivered
			// over mTLS. Empty for a plaintext template. A secret value: never
			// logged.
			EncryptionKey: encKey,
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SandboxPool{}).
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
