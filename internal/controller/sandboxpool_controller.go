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
}

// SandboxPool ownership: get/list/watch to reconcile, status to write warmed
// counts and conditions. SandboxTemplate is read-only (covered above).
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxpools/status,verbs=get;update;patch
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
	readySnapshots := r.readySnapshotCount(templateID)
	desired := pool.Spec.Replicas

	if readySnapshots < desired {
		deficit := desired - readySnapshots
		logger.Info("snapshot deficit", "ready", readySnapshots, "desired", desired, "creating", deficit)
		created, err := r.createSnapshotsOnNodes(ctx, templateID, template.Spec.Image, template.Spec.Init, template.Spec.Volumes, deficit)
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

// createSnapshotsOnNodes asks up to deficit healthy nodes that lack the
// template to build it. Returns how many builds were started.
func (r *SandboxPoolReconciler) createSnapshotsOnNodes(ctx context.Context, templateID, image string, initCommands []string, templateVolumes []v1alpha1.SandboxVolume, deficit int32) (int32, error) {
	var created int32
	var errs []error
	for _, node := range r.NodeRegistry.ListNodes() {
		if created >= deficit {
			break
		}
		if !node.isHealthy() || node.hasSnapshot(templateID) {
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
		})
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}
		r.NodeRegistry.AddTemplateWithDigest(node.Name, templateID, resp.TemplateDigest)
		created++
	}
	if created == 0 && len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	return created, nil
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
