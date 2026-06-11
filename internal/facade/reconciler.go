// Package facade implements the agents.x-k8s.io conformance facade (issue #19).
//
// It presents sandboxes via the upstream SIG agent-sandbox API
// (agents.x-k8s.io/v1alpha1 Sandbox) and fulfils them on our fork engine by
// mapping each upstream Sandbox onto our husk-backed run path: a SandboxClaim
// in our agentrun.dev group, bound to one of our pools.
//
// Toolchain note (ADR 0001): we vendor the upstream Go types
// (sigs.k8s.io/agent-sandbox) directly. That module declares go 1.26, so the
// faithful path required bumping our toolchain from go 1.24 to go 1.26. Both
// golangci-lint runs (darwin + GOOS=linux) analyzed the go 1.26 module cleanly
// with golangci-lint v1.64.8, so we kept the vendor instead of re-declaring the
// CRD by hand. See docs/adr/0001-facade-and-naming.md.
//
// The facade is opt-in: it runs as a separate binary (cmd/facade) with its own
// manager and is not entangled with cmd/controller. Extras (pools, warm pools,
// templates) stay in our agentrun.dev group; the single bridge annotation
// agentrun.dev/pool links an upstream Sandbox to one of our pools.
package facade

import (
	"context"
	"fmt"

	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	runv1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
)

const (
	// PoolAnnotation is the single bridge annotation that links an upstream
	// agents.x-k8s.io Sandbox to one of our agentrun.dev pools (the warm-pool
	// source for the husk run path). When unset the facade falls back to its
	// configured default pool. Documented in docs/adr/0001-facade-and-naming.md.
	PoolAnnotation = "agentrun.dev/pool"

	// SandboxConditionType is the upstream Ready condition the facade mirrors
	// our SandboxClaim readiness into.
	SandboxConditionType = string(agentsv1alpha1.SandboxConditionReady)
)

// SandboxReconciler reconciles an upstream agents.x-k8s.io/v1alpha1 Sandbox
// onto our husk-backed run path. It owns exactly one of our SandboxClaim
// objects per Sandbox (same name + namespace, owner-referenced for GC), mirrors
// the claim's readiness into the Sandbox status, and terminates the claim when
// the Sandbox is deleted or scaled to replicas 0.
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// DefaultPool is the agentrun.dev pool a Sandbox binds to when it carries no
	// agentrun.dev/pool bridge annotation. Required: the facade cannot fulfil a
	// Sandbox without a pool to draw a husk from.
	DefaultPool string

	// ClusterDomain is the DNS domain used to derive the upstream
	// status.serviceFQDN (defaults to cluster.local upstream). Empty disables
	// the derived FQDN.
	ClusterDomain string
}

// desiredReplicas returns the effective replica count for a Sandbox, applying
// the upstream default of 1 when spec.replicas is unset.
func desiredReplicas(sb *agentsv1alpha1.Sandbox) int32 {
	if sb.Spec.Replicas == nil {
		return 1
	}
	return *sb.Spec.Replicas
}

// poolFor resolves the agentrun.dev pool a Sandbox binds to: the bridge
// annotation agentrun.dev/pool if present, else the configured default pool.
func (r *SandboxReconciler) poolFor(sb *agentsv1alpha1.Sandbox) string {
	if p := sb.Annotations[PoolAnnotation]; p != "" {
		return p
	}
	return r.DefaultPool
}

// Reconcile drives the Sandbox -> husk run-path lifecycle:
//   - replicas >= 1 (default 1) and not deleting: ensure our SandboxClaim, then
//     mirror its readiness into the Sandbox status.
//   - replicas 0: terminate our SandboxClaim (delete it).
//   - deletion: the owner reference garbage-collects our SandboxClaim; we just
//     observe and return.
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sb agentsv1alpha1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		// Not found: the Sandbox is gone; owner-ref GC removes our claim.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: the SandboxClaim carries an owner reference to the Sandbox, so
	// the apiserver garbage-collects it. Nothing for the facade to do.
	if !sb.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	pool := r.poolFor(&sb)
	if pool == "" {
		// No pool to bind to and no default configured: surface a not-ready
		// condition with actionable remediation rather than creating an
		// unbindable claim.
		return ctrl.Result{}, r.setReady(ctx, &sb, metav1.ConditionFalse, "NoPool",
			fmt.Sprintf("no %s annotation and no --default-pool configured; set the bridge annotation or the facade default pool", PoolAnnotation), 0)
	}

	// replicas 0: scaled to zero. Terminate our run-path object and report the
	// Sandbox not ready with zero replicas.
	if desiredReplicas(&sb) == 0 {
		if err := r.deleteClaim(ctx, &sb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setReady(ctx, &sb, metav1.ConditionFalse, "ScaledToZero",
			"replicas is 0; the husk run-path object is terminated", 0)
	}

	// replicas >= 1: ensure our SandboxClaim exists and mirror its readiness.
	claim, err := r.ensureClaim(ctx, &sb, pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	ready := claim.Status.Phase == runv1alpha1.SandboxReady
	if ready {
		logger.V(1).Info("sandbox ready", "sandbox", req.NamespacedName, "claim", claim.Name, "endpoint", claim.Status.Endpoint)
		return ctrl.Result{}, r.setReady(ctx, &sb, metav1.ConditionTrue, "ClaimReady",
			fmt.Sprintf("husk run-path object %q is Ready", claim.Name), 1)
	}

	return ctrl.Result{}, r.setReady(ctx, &sb, metav1.ConditionFalse, "Claim"+string(claim.Status.Phase),
		fmt.Sprintf("husk run-path object %q is in phase %q", claim.Name, claim.Status.Phase), 0)
}

// ensureClaim creates or returns our SandboxClaim for a Sandbox. The claim is
// named after the Sandbox, lives in the same namespace, is owner-referenced to
// the Sandbox (for GC + the watch back-link), and binds to the resolved pool.
//
// podTemplate mapping: the Sandbox spec.podTemplate.spec.containers[*].env is
// reconciled onto the claim's env (the husk run path applies env into the
// guest). Other podTemplate fields (images, resources, volumes, security
// context) are a documented conformance exception for a later slice; see
// docs/facade-conformance.md. The husk pool already pins the image + resources
// at pool build time, so the per-Sandbox podTemplate image is not yet honored.
func (r *SandboxReconciler) ensureClaim(ctx context.Context, sb *agentsv1alpha1.Sandbox, pool string) (*runv1alpha1.SandboxClaim, error) {
	claim := &runv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name,
			Namespace: sb.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, claim, func() error {
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[PoolAnnotation] = pool
		claim.Spec.PoolRef = runv1alpha1.LocalObjectReference{Name: pool}
		claim.Spec.Env = podTemplateEnv(sb)
		// Owner reference: GC our claim when the Sandbox is deleted, and set the
		// controller back-link so a claim status change re-queues the Sandbox.
		return controllerutil.SetControllerReference(sb, claim, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure SandboxClaim for sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return claim, nil
}

// podTemplateEnv extracts the union of container env vars from the upstream
// Sandbox podTemplate. The husk run path applies these into the guest. We take
// the first container's env as the canonical set (sandboxes are single-workload
// by construction); additional containers are a later-slice exception.
func podTemplateEnv(sb *agentsv1alpha1.Sandbox) []corev1.EnvVar {
	containers := sb.Spec.PodTemplate.Spec.Containers
	if len(containers) == 0 {
		return nil
	}
	return containers[0].Env
}

// deleteClaim terminates our SandboxClaim for a Sandbox (replicas 0 path). It
// is a no-op when the claim is already gone.
func (r *SandboxReconciler) deleteClaim(ctx context.Context, sb *agentsv1alpha1.Sandbox) error {
	claim := &runv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: sb.Name, Namespace: sb.Namespace},
	}
	if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("terminate SandboxClaim for sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// setReady mirrors readiness into the upstream Sandbox status: the Ready
// condition, the actual replica count, and the derived serviceFQDN. It patches
// the status subresource and is idempotent (no write when nothing changed).
func (r *SandboxReconciler) setReady(ctx context.Context, sb *agentsv1alpha1.Sandbox, status metav1.ConditionStatus, reason, message string, replicas int32) error {
	cond := metav1.Condition{
		Type:               SandboxConditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: sb.Generation,
	}

	before := sb.DeepCopy()
	changed := apimeta.SetStatusCondition(&sb.Status.Conditions, cond)
	sb.Status.Replicas = replicas
	if fqdn := r.serviceFQDN(sb); fqdn != "" {
		sb.Status.ServiceFQDN = fqdn
	}

	if !changed && before.Status.Replicas == sb.Status.Replicas && before.Status.ServiceFQDN == sb.Status.ServiceFQDN {
		return nil
	}
	if err := r.Status().Update(ctx, sb); err != nil {
		return fmt.Errorf("mirror status into sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// serviceFQDN derives the upstream status.serviceFQDN for a Sandbox using the
// configured cluster domain, matching the upstream headless-Service naming
// (<name>.<namespace>.svc.<cluster-domain>). Empty when no cluster domain is
// configured.
func (r *SandboxReconciler) serviceFQDN(sb *agentsv1alpha1.Sandbox) string {
	if r.ClusterDomain == "" {
		return ""
	}
	return fmt.Sprintf("%s.%s.svc.%s", sb.Name, sb.Namespace, r.ClusterDomain)
}

// SetupWithManager wires the reconciler to watch upstream Sandboxes and own our
// SandboxClaim objects so a claim status change re-queues the owning Sandbox.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Sandbox{}).
		Owns(&runv1alpha1.SandboxClaim{}).
		Complete(r)
}
