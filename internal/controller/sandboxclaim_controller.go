package controller

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/workspace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type SandboxClaimReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry
}

func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var claim v1alpha1.SandboxClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Already assigned; nothing to do
	if claim.Status.Phase == v1alpha1.SandboxReady {
		return r.reconcileTimeout(ctx, &claim)
	}

	// Already failed; don't retry
	if claim.Status.Phase == v1alpha1.SandboxFailed {
		return ctrl.Result{}, nil
	}

	// Find the pool
	var pool v1alpha1.SandboxPool
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: claim.Namespace,
		Name:      claim.Spec.PoolRef.Name,
	}, &pool); err != nil {
		logger.Error(err, "pool not found", "pool", claim.Spec.PoolRef.Name)
		return ctrl.Result{}, err
	}

	// Find the template
	var template v1alpha1.SandboxTemplate
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: pool.Namespace,
		Name:      pool.Spec.TemplateRef.Name,
	}, &template); err != nil {
		return ctrl.Result{}, err
	}

	// Mark as restoring
	claim.Status.Phase = v1alpha1.SandboxRestoring
	if err := r.Status().Update(ctx, &claim); err != nil {
		return ctrl.Result{}, err
	}

	// Pick a node with a ready snapshot
	node, snapshotID, err := r.selectNode(ctx, &pool, claim.Spec.NodeName)
	if err != nil {
		logger.Error(err, "no node with ready snapshot")
		claim.Status.Phase = v1alpha1.SandboxPending
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Prepare volumes per fork policies
	volumes, err := r.prepareVolumes(ctx, template.Spec.Volumes, claim.Name, claim.Spec.VolumeOverrides)
	if err != nil {
		logger.Error(err, "volume preparation failed")
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}
	_ = volumes

	// Resolve secrets
	env, secretVals, err := r.resolveSecrets(ctx, claim.Namespace, claim.Spec.Env, claim.Spec.Secrets)
	if err != nil {
		logger.Error(err, "secret resolution failed")
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// Mint the sandbox API bearer token before forking; forkd registers it
	// at fork time. The value reaches exactly two places: the ForkRequest
	// and the owned token Secret below. Never status, conditions, events,
	// or logs.
	apiToken, err := mintAPIToken()
	if err != nil {
		logger.Error(err, "token minting failed")
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// Call forkd on the selected node: this is the <2ms hot path
	result, err := r.forkOnNode(ctx, node, snapshotID, claim.Name, env, secretVals, apiToken)
	if err != nil {
		// A NotFound from forkd usually means the snapshot is not built on
		// that node yet; transient while the pool reconciler catches up.
		if isNotFound(err) {
			logger.Info("snapshot not yet on node, retrying", "node", node.Name, "error", err.Error())
			claim.Status.Phase = v1alpha1.SandboxPending
			// Best-effort status write; the return below already requeues or surfaces the error.
			_ = r.Status().Update(ctx, &claim)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		logger.Error(err, "fork failed", "node", node.Name)
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// Hand the token to the claim's consumer via an owned Secret, BEFORE
	// the Ready status write: a Ready claim whose token Secret does not
	// exist would be unusable, and the Ready early-return above would never
	// retry the Secret. The token exists only in this Secret.
	if err := ensureSandboxTokenSecret(ctx, r.Client, &claim, claim.Name+tokenSecretSuffix, apiToken, result.Endpoint); err != nil {
		logger.Error(err, "token secret write failed")
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// Update status
	now := metav1.Now()
	claim.Status.Phase = v1alpha1.SandboxReady
	claim.Status.Endpoint = result.Endpoint
	claim.Status.Node = node.Name
	claim.Status.SandboxID = result.SandboxID
	claim.Status.ForkTimeMicros = int64(result.ForkTimeMs * 1000)
	claim.Status.StartedAt = &now
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "Forked",
		Message:            fmt.Sprintf("forked in %.2fms on node %s", result.ForkTimeMs, node.Name),
	})

	if err := r.Status().Update(ctx, &claim); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("sandbox claimed",
		"sandbox", claim.Name,
		"node", node.Name,
		"forkTime", fmt.Sprintf("%.2fms", result.ForkTimeMs),
	)

	return ctrl.Result{}, nil
}

func (r *SandboxClaimReconciler) reconcileTimeout(ctx context.Context, claim *v1alpha1.SandboxClaim) (ctrl.Result, error) {
	if claim.Spec.Timeout == nil || claim.Status.StartedAt == nil {
		return ctrl.Result{}, nil
	}

	deadline := claim.Status.StartedAt.Add(claim.Spec.Timeout.Duration)
	if time.Now().After(deadline) {
		claim.Status.Phase = v1alpha1.SandboxTerminating
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, claim)
		// Terminate via forkd
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: time.Until(deadline)}, nil
}

type forkResult struct {
	SandboxID  string
	Endpoint   string
	ForkTimeMs float64
}

func (r *SandboxClaimReconciler) selectNode(ctx context.Context, pool *v1alpha1.SandboxPool, preferredNode string) (*NodeInfo, string, error) {
	templateName := pool.Spec.TemplateRef.Name
	node, err := r.NodeRegistry.SelectNode(templateName, preferredNode)
	if err != nil {
		return nil, "", err
	}
	return node, templateName, nil
}

func (r *SandboxClaimReconciler) prepareVolumes(ctx context.Context, templateVols []v1alpha1.SandboxVolume, sandboxID string, overrides []v1alpha1.VolumeOverride) ([]*workspace.PreparedVolume, error) {
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

func (r *SandboxClaimReconciler) resolveSecrets(ctx context.Context, namespace string, env []corev1.EnvVar, secrets []v1alpha1.SecretMount) (envOut, secretsOut map[string]string, err error) {
	envOut = make(map[string]string)
	secretsOut = make(map[string]string)

	for _, e := range env {
		envOut[e.Name] = e.Value
	}

	coreClient := r.Client
	for _, s := range secrets {
		var secret corev1.Secret
		if err := coreClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      s.SecretRef.Name,
		}, &secret); err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", s.SecretRef.Name, err)
		}
		value, ok := secret.Data[s.SecretRef.Key]
		if !ok {
			return nil, nil, fmt.Errorf("key %s not found in secret %s", s.SecretRef.Key, s.SecretRef.Name)
		}
		envVar := s.EnvVar
		if envVar == "" {
			envVar = s.Name
		}
		secretsOut[envVar] = string(value)
	}

	return envOut, secretsOut, nil
}

func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SandboxClaim{}).
		Complete(r)
}
