package controller

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type SandboxPoolReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry
}

// NodeRegistry tracks forkd instances across the cluster.
type NodeRegistry struct {
	// Maps node name to its gRPC endpoint and capacity.
	nodes map[string]*NodeInfo
}

type NodeInfo struct {
	Name             string
	Endpoint         string
	ActiveSandboxes  int32
	MaxSandboxes     int32
	MemoryTotal      int64
	MemoryUsed       int64
	TemplateIDs      []string
	SnapshotIDs      []string
	LastHeartbeat    time.Time
}

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

	// Count ready snapshots across nodes
	readySnapshots := r.countReadySnapshots(ctx, &pool)
	desired := pool.Spec.Replicas

	if readySnapshots < desired {
		deficit := desired - readySnapshots
		logger.Info("snapshot deficit", "ready", readySnapshots, "desired", desired, "creating", deficit)

		if err := r.createSnapshots(ctx, &pool, &template, deficit); err != nil {
			logger.Error(err, "failed to create snapshots")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
	}

	// Update status
	pool.Status.ReadySnapshots = readySnapshots
	pool.Status.TotalSnapshots = readySnapshots
	pool.Status.NodeDistribution = r.getNodeDistribution(ctx, &pool)

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

func (r *SandboxPoolReconciler) createSnapshots(ctx context.Context, pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate, count int32) error {
	// 1. Find nodes with capacity for new snapshots
	// 2. For each node, call forkd.CreateTemplate + forkd.CreateSnapshot via gRPC
	// 3. Distribute snapshots across nodes for availability
	return nil
}

func (r *SandboxPoolReconciler) countReadySnapshots(ctx context.Context, pool *v1alpha1.SandboxPool) int32 {
	// Query all forkd instances for snapshots matching this pool
	return 0
}

func (r *SandboxPoolReconciler) getNodeDistribution(ctx context.Context, pool *v1alpha1.SandboxPool) map[string]int32 {
	return nil
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
