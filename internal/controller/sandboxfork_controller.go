package controller

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/workspace"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type SandboxForkReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry
}

func (r *SandboxForkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var fork v1alpha1.SandboxFork
	if err := r.Get(ctx, req.NamespacedName, &fork); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A rejected fork is terminal: never reconcile it again.
	if meta.IsStatusConditionTrue(fork.Status.Conditions, "Rejected") {
		return ctrl.Result{}, nil
	}

	if fork.Status.ReadyForks >= fork.Spec.Replicas {
		return ctrl.Result{}, nil
	}

	// Find the source sandbox (a SandboxClaim)
	var source v1alpha1.SandboxClaim
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: fork.Namespace,
		Name:      fork.Spec.SourceRef.Name,
	}, &source); err != nil {
		logger.Error(err, "source sandbox not found", "source", fork.Spec.SourceRef.Name)
		return ctrl.Result{}, err
	}

	// Live-fork secret gate: duplicating guest memory duplicates any
	// delivered secrets into every fork. Default-deny without explicit
	// opt-in. Spec-level check: fires regardless of source readiness.
	if len(source.Spec.Secrets) > 0 {
		now := metav1.Now()
		if !fork.Spec.AllowSecretInheritance {
			setCondition(&fork.Status.Conditions, metav1.Condition{
				Type:               "Rejected",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "SecretInheritanceDenied",
				Message:            "source claim holds secrets; recreate the fork with spec.allowSecretInheritance=true to permit it (forks duplicate guest memory, including secret values)",
			})
			if err := r.Status().Update(ctx, &fork); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil // terminal: no requeue
		}
		// Audit trail for the explicit opt-in. Only write status when the
		// condition is not already recorded, so the status-update-triggered
		// re-reconcile does not loop on itself.
		if c := meta.FindStatusCondition(fork.Status.Conditions, "SecretInheritance"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ExplicitOptIn" {
			setCondition(&fork.Status.Conditions, metav1.Condition{
				Type:               "SecretInheritance",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "ExplicitOptIn",
				Message:            "fork inherits the source's in-memory secrets by explicit opt-in",
			})
			if err := r.Status().Update(ctx, &fork); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if source.Status.Phase != v1alpha1.SandboxReady {
		logger.Info("source sandbox not ready, waiting", "source", source.Name, "phase", source.Status.Phase)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Find the node running the source sandbox
	node, ok := r.NodeRegistry.GetNode(source.Status.Node)
	if !ok {
		return ctrl.Result{}, fmt.Errorf("node %s not found in registry", source.Status.Node)
	}

	// Find the template for volume fork policies
	var pool v1alpha1.SandboxPool
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: fork.Namespace,
		Name:      source.Spec.PoolRef.Name,
	}, &pool); err != nil {
		return ctrl.Result{}, err
	}

	var template v1alpha1.SandboxTemplate
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: pool.Namespace,
		Name:      pool.Spec.TemplateRef.Name,
	}, &template); err != nil {
		return ctrl.Result{}, err
	}

	// Create forks
	needed := fork.Spec.Replicas - fork.Status.ReadyForks
	for i := int32(0); i < needed; i++ {
		forkID := fmt.Sprintf("%s-fork-%d", fork.Name, fork.Status.TotalForks+i)

		// Prepare volumes per fork policies
		_, err := r.prepareVolumes(ctx, template.Spec.Volumes, forkID, fork.Spec.VolumeOverrides)
		if err != nil {
			logger.Error(err, "volume preparation failed", "fork", forkID)
			continue
		}

		// Call forkd.ForkRunning on the source node
		result, err := r.forkRunningOnNode(ctx, node, source.Status.SandboxID, forkID, fork.Spec.PauseSource)
		if err != nil {
			logger.Error(err, "fork failed", "fork", forkID)
			continue
		}

		fork.Status.Forks = append(fork.Status.Forks, v1alpha1.ForkInfo{
			Name:           forkID,
			SandboxID:      result.SandboxID,
			Endpoint:       result.Endpoint,
			Node:           node.Name,
			Phase:          v1alpha1.SandboxReady,
			ForkTimeMicros: int64(result.ForkTimeMs * 1000),
		})
		fork.Status.ReadyForks++
		fork.Status.TotalForks++

		logger.Info("fork created",
			"fork", forkID,
			"node", node.Name,
			"forkTime", fmt.Sprintf("%.2fms", result.ForkTimeMs),
		)
	}

	now := metav1.Now()
	fork.Status.CheckpointTime = &now
	setCondition(&fork.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(fork.Status.ReadyForks >= fork.Spec.Replicas),
		LastTransitionTime: now,
		Reason:             "ForksCreated",
		Message:            fmt.Sprintf("%d/%d forks ready", fork.Status.ReadyForks, fork.Spec.Replicas),
	})

	if err := r.Status().Update(ctx, &fork); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *SandboxForkReconciler) prepareVolumes(ctx context.Context, templateVols []v1alpha1.SandboxVolume, sandboxID string, overrides []v1alpha1.VolumeOverride) ([]*workspace.PreparedVolume, error) {
	overrideMap := make(map[string]v1alpha1.ForkPolicy)
	for _, o := range overrides {
		overrideMap[o.Name] = o.ForkPolicy
	}

	var prepared []*workspace.PreparedVolume
	for _, vol := range templateVols {
		policy := vol.ForkPolicy
		if override, ok := overrideMap[vol.Name]; ok {
			policy = override
		}

		handler := workspace.HandlerForPolicy(policy)
		pv, err := handler.Prepare(ctx, vol, sandboxID)
		if err != nil {
			return nil, fmt.Errorf("volume %s: %w", vol.Name, err)
		}
		prepared = append(prepared, pv)
	}
	return prepared, nil
}

type forkRunningResult struct {
	SandboxID    string
	Endpoint     string
	ForkTimeMs   float64
	CheckpointMs float64
}

func (r *SandboxForkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SandboxFork{}).
		Complete(r)
}
