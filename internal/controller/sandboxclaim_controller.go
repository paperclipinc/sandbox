package controller

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// tracer is the controller component tracer; no-op unless tracing is configured.
var tracer = observability.Tracer("agentrun-controller")

type SandboxClaimReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry
}

func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// controller.reconcileClaim spans the whole reconcile. Only claim
	// name/namespace and the pool name (config, no secrets) are attributes; the
	// fork RPC below is a child span carrying the trace to forkd over gRPC.
	ctx, span := tracer.Start(ctx, "controller.reconcileClaim", trace.WithAttributes(
		attribute.String("claim.name", req.Name),
		attribute.String("claim.namespace", req.Namespace),
	))
	defer span.End()

	var claim v1alpha1.SandboxClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	span.SetAttributes(attribute.String("pool", claim.Spec.PoolRef.Name))

	// A claim under deletion: reap its backing VM via the finalizer before the
	// API object is allowed to disappear.
	if !claim.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &claim)
	}

	// Already assigned; drive maxLifetime / idleTimeout reaping.
	if claim.Status.Phase == v1alpha1.SandboxReady {
		return r.reconcileLifetime(ctx, &claim)
	}

	// Terminal phases: don't retry.
	if claim.Status.Phase == v1alpha1.SandboxFailed || claim.Status.Phase == v1alpha1.SandboxTerminated {
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

	// Add the terminate finalizer before the claim acquires a backing VM, so
	// no Ready claim can ever be deleted without forkd reaping its sandbox.
	// This is a metadata Update, distinct from the status writes below.
	if controllerutil.AddFinalizer(&claim, FinalizerTerminate) {
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, err
		}
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
		recordClaimPending()
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Translate the template's volumes (with this claim's VolumeOverrides
	// applied) into the Fork RPC's VolumeMounts. The node prepares and attaches
	// the backing drives per policy; the controller only forwards the spec.
	volumes := volumeMounts(template.Spec.Volumes, claim.Spec.VolumeOverrides)

	// Resolve secrets
	env, secretVals, err := r.resolveSecrets(ctx, claim.Namespace, claim.Spec.Env, claim.Spec.Secrets)
	if err != nil {
		logger.Error(err, "secret resolution failed")
		recordClaimError(claim.Spec.PoolRef.Name, "secret")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
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
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// Call forkd on the selected node: this is the <2ms hot path
	result, err := r.forkOnNode(ctx, node, snapshotID, claim.Name, env, secretVals, template.Spec.Network, volumes, apiToken)
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
		recordClaimError(claim.Spec.PoolRef.Name, "fork")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
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
		recordClaimError(claim.Spec.PoolRef.Name, "token")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
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

// reconcileDelete reaps the claim's backing VM via forkd Terminate, then
// removes the finalizer so the API object can be garbage collected. A claim
// that never acquired a sandbox (no Node or SandboxID) skips straight to
// finalizer removal. terminateOnNode treats a NotFound sandbox and a
// vanished node as already-terminated, so a node that left the registry never
// hangs deletion.
func (r *SandboxClaimReconciler) reconcileDelete(ctx context.Context, claim *v1alpha1.SandboxClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(claim, FinalizerTerminate) {
		return ctrl.Result{}, nil
	}

	if claim.Status.Node != "" && claim.Status.SandboxID != "" {
		if err := terminateOnNode(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID); err != nil {
			logger.Error(err, "terminate backing sandbox on delete", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(claim, FinalizerTerminate)
	if err := r.Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileLifetime drives a Ready claim to the terminal Terminated phase when
// it exceeds maxLifetime (Spec.Timeout from StartedAt) or goes idle
// (Spec.IdleTimeout from the later of last-activity and StartedAt). Expiry
// terminates the backing VM directly via terminateOnNode and leaves the
// finalizer in place; the bounded, tolerant terminateOnNode keeps eventual
// delete safe. A claim already Terminated returns immediately (idempotent).
func (r *SandboxClaimReconciler) reconcileLifetime(ctx context.Context, claim *v1alpha1.SandboxClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if claim.Status.Phase == v1alpha1.SandboxTerminated {
		return ctrl.Result{}, nil
	}
	if claim.Status.StartedAt == nil {
		return ctrl.Result{}, nil
	}

	hasMaxLifetime := claim.Spec.Timeout != nil
	hasIdle := claim.Spec.IdleTimeout != nil
	if !hasMaxLifetime && !hasIdle {
		return ctrl.Result{}, nil
	}

	now := time.Now()
	startedAt := claim.Status.StartedAt.Time

	// maxLifetime takes precedence: it does not depend on a reachable forkd.
	if hasMaxLifetime {
		deadline := startedAt.Add(claim.Spec.Timeout.Duration)
		if !now.Before(deadline) {
			return r.terminateLifetime(ctx, claim, "MaxLifetimeExceeded",
				fmt.Sprintf("max lifetime %s exceeded", claim.Spec.Timeout.Duration))
		}
	}

	// Idle check needs last-activity from forkd. An unreachable node means we
	// cannot evaluate idle this pass; requeue and try again.
	requeue := time.Duration(0)
	if hasIdle {
		_, lastActivity, ok := sandboxActivity(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID)
		if !ok {
			logger.Info("cannot evaluate idle, node unreachable; requeueing", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		last := startedAt
		if lastActivity.After(last) {
			last = lastActivity
		}
		idleDeadline := last.Add(claim.Spec.IdleTimeout.Duration)
		if now.After(idleDeadline) {
			return r.terminateLifetime(ctx, claim, "IdleTimeout",
				fmt.Sprintf("idle for more than %s", claim.Spec.IdleTimeout.Duration))
		}
		requeue = time.Until(idleDeadline)
	}

	// Requeue at the nearest deadline.
	if hasMaxLifetime {
		untilMax := time.Until(startedAt.Add(claim.Spec.Timeout.Duration))
		if requeue == 0 || untilMax < requeue {
			requeue = untilMax
		}
	}
	if requeue <= 0 {
		requeue = 1 * time.Second
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// terminateLifetime reaps the claim's backing VM and stamps the terminal
// Terminated phase with a FinishedAt time and a Terminated condition. The
// finalizer stays in place; the bounded terminateOnNode keeps later delete
// safe.
func (r *SandboxClaimReconciler) terminateLifetime(ctx context.Context, claim *v1alpha1.SandboxClaim, reason, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if claim.Status.Node != "" && claim.Status.SandboxID != "" {
		if err := terminateOnNode(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID); err != nil {
			logger.Error(err, "terminate backing sandbox on lifetime expiry", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	claim.Status.Phase = v1alpha1.SandboxTerminated
	claim.Status.FinishedAt = &now
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Terminated",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("claim terminated by lifetime policy", "claim", claim.Name, "reason", reason)
	return ctrl.Result{}, nil
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
