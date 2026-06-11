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
	// agentrun.dev/kvm. Only used when EnableHuskPods is true.
	KVMResourceName string
	// DataDir is the forkd data directory on the node; the husk pod's snapshot
	// hostPath is rooted here (<DataDir>/templates/<id>/snapshot). Empty defaults
	// to /var/lib/agent-run. Only used when EnableHuskPods is true.
	DataDir string
	// HuskTLSSecretName is the Secret holding the husk stub's mTLS server leaf
	// (tls.crt, tls.key), mounted into each husk pod so the stub can serve the
	// mTLS network control. Only used when EnableHuskPods is true.
	HuskTLSSecretName string
	// HuskCASecretName is the Secret holding the control plane CA (ca.crt),
	// mounted into each husk pod so the stub verifies the controller client cert.
	// Only used when EnableHuskPods is true.
	HuskCASecretName string
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

	// Husk pod warm-pool path (issue #18, the pod-native default). When enabled,
	// the pool does BOTH: it FIRST ensures the template snapshot is built on the
	// target nodes (the same createSnapshotsOnNodes build path raw-forkd uses, so
	// <dataDir>/templates/<id>/snapshot exists on those nodes), THEN maintains a
	// warm pool of pre-scheduled husk pods pinned to the snapshot-holding nodes so
	// their read-only snapshot hostPath resolves.
	//
	// ORDERING + PLACEMENT COUPLING: the snapshot build runs before the husk pods
	// so reconcileHuskPods can read the snapshot-holding node set
	// (NodesWithTemplate) and pin each husk pod to it via nodeAffinity. The first
	// reconcile of a fresh pool may build the snapshot but find no holder yet (the
	// build registers the node in the same pass); the husk pods then fall back to
	// the kvm nodeSelector and a later reconcile tightens the affinity once the
	// registry reports the holders. The raw-forkd path below (flag off, behind
	// --enable-raw-forkd) is the fork-per-claim fallback and does NOT create husk
	// pods.
	if r.EnableHuskPods {
		// Build the snapshot on the target nodes first. A build error is logged
		// and we requeue; we still attempt the husk pods with whatever holders
		// exist so a transient build hiccup does not stall the warm pool forever.
		if err := r.ensureTemplateBuilt(ctx, &pool, &template); err != nil {
			logger.Error(err, "failed to build template snapshot for husk pool")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		warm, err := r.reconcileHuskPods(ctx, &pool, &template)
		if err != nil {
			logger.Error(err, "failed to reconcile husk pods")
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

	var encKey []byte
	if template.Spec.Encrypted {
		var keyErr error
		encKey, keyErr = EnsureEncKey(ctx, r.Client, pool.Namespace, templateID, template)
		if keyErr != nil {
			// The error names only the Secret, never key bytes.
			return fmt.Errorf("ensure encryption key for template %s: %w", templateID, keyErr)
		}
	}
	if _, err := r.createSnapshotsOnNodes(ctx, templateID, template.Spec.Image, template.Spec.Init, template.Spec.Volumes, encKey, deficit); err != nil {
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
