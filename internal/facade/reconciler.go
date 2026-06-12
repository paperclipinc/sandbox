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
	"net"

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

// Reconcile drives the Sandbox -> husk run-path lifecycle. The upstream
// pause/resume contract is the spec.replicas 0<->1 toggle (upstream v0.4.6 has
// no stateful hibernate field; their controller deletes the pod on 0 and
// cold-creates it on 1). We map it onto the husk warm pool:
//
//   - replicas >= 1 (default 1) and not deleting (create OR resume): ensure our
//     SandboxClaim. On resume this re-activates a dormant warm husk pod via the
//     same fast path as the initial create. Mirror the claim readiness, and on
//     Ready set the serving observables (serviceFQDN, podIPs).
//   - replicas 0 (pause): RELEASE the run path to the warm pool by deleting our
//     SandboxClaim, so the bound husk pod returns dormant to the pool. Clear the
//     serving observables (Status.Replicas 0, Ready False, serviceFQDN + podIPs
//     cleared, no serving endpoint).
//   - deletion: the owner reference garbage-collects our SandboxClaim; we just
//     observe and return.
//
// The mapping is idempotent and stable under 1->0->1->0 toggling: pause is a
// no-op when the claim is already released, resume re-creates the same named
// claim, and the status writes are conditional (no write when nothing changed).
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
		return ctrl.Result{}, r.mirror(ctx, &sb, statusUpdate{
			status:  metav1.ConditionFalse,
			reason:  "NoPool",
			message: fmt.Sprintf("no %s annotation and no --default-pool configured; set the bridge annotation or the facade default pool", PoolAnnotation),
		})
	}

	// replicas 0 (pause): release the run path to the warm pool. Delete our
	// SandboxClaim so the bound husk pod returns dormant to the pool, and clear
	// the serving observables (serviceFQDN + podIPs).
	if desiredReplicas(&sb) == 0 {
		if err := r.deleteClaim(ctx, &sb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.mirror(ctx, &sb, statusUpdate{
			status:  metav1.ConditionFalse,
			reason:  "Paused",
			message: "replicas is 0 (paused); the husk run-path object is released to the warm pool",
		})
	}

	// replicas >= 1 (create or resume): ensure our SandboxClaim exists. On a
	// resume after a pause this re-activates a dormant warm husk pod via the same
	// fast path as create. Mirror the claim readiness.
	claim, err := r.ensureClaim(ctx, &sb, pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	if claim.Status.Phase == runv1alpha1.SandboxReady {
		logger.V(1).Info("sandbox ready", "sandbox", req.NamespacedName, "claim", claim.Name, "endpoint", claim.Status.Endpoint)
		return ctrl.Result{}, r.mirror(ctx, &sb, statusUpdate{
			status:   metav1.ConditionTrue,
			reason:   "ClaimReady",
			message:  fmt.Sprintf("husk run-path object %q is Ready", claim.Name),
			replicas: 1,
			endpoint: claim.Status.Endpoint,
		})
	}

	return ctrl.Result{}, r.mirror(ctx, &sb, statusUpdate{
		status:  metav1.ConditionFalse,
		reason:  "Claim" + string(claim.Status.Phase),
		message: fmt.Sprintf("husk run-path object %q is in phase %q", claim.Name, claim.Status.Phase),
	})
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

// statusUpdate is the set of upstream Sandbox status facts the facade mirrors in
// one reconcile: the Ready condition, the actual replica count, and (when
// running with a serving endpoint) the serviceFQDN + podIPs. The serving
// observables are populated only when status is True with an endpoint; on every
// other path (pause, pending, error) they are CLEARED so a paused or not-ready
// Sandbox never advertises a stale endpoint.
type statusUpdate struct {
	status   metav1.ConditionStatus
	reason   string
	message  string
	replicas int32
	// endpoint is the husk run-path endpoint (host:port) when Ready, used to
	// derive podIPs. Empty on every not-serving path.
	endpoint string
}

// mirror writes one statusUpdate onto the upstream Sandbox status subresource.
// It is idempotent (no write when nothing changed) and is the single place the
// serving observables (serviceFQDN, podIPs) are set or cleared, so pause always
// clears them and resume always re-populates them.
func (r *SandboxReconciler) mirror(ctx context.Context, sb *agentsv1alpha1.Sandbox, u statusUpdate) error {
	cond := metav1.Condition{
		Type:               SandboxConditionType,
		Status:             u.status,
		Reason:             u.reason,
		Message:            u.message,
		ObservedGeneration: sb.Generation,
	}

	before := sb.DeepCopy()
	changed := apimeta.SetStatusCondition(&sb.Status.Conditions, cond)
	sb.Status.Replicas = u.replicas

	// Serving observables: set on Ready-with-endpoint, cleared otherwise.
	if u.status == metav1.ConditionTrue && u.endpoint != "" {
		sb.Status.ServiceFQDN = r.serviceFQDN(sb)
		sb.Status.PodIPs = podIPsFromEndpoint(u.endpoint)
	} else {
		sb.Status.ServiceFQDN = ""
		sb.Status.PodIPs = nil
	}

	if !changed &&
		before.Status.Replicas == sb.Status.Replicas &&
		before.Status.ServiceFQDN == sb.Status.ServiceFQDN &&
		equalStrings(before.Status.PodIPs, sb.Status.PodIPs) {
		return nil
	}
	if err := r.Status().Update(ctx, sb); err != nil {
		return fmt.Errorf("mirror status into sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// podIPsFromEndpoint derives the upstream status.podIPs from the husk run-path
// endpoint (host:port). The host portion is the serving pod IP; a bare host
// without a port is taken as-is. Returns nil when no IP can be parsed.
func podIPsFromEndpoint(endpoint string) []string {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	if host == "" {
		return nil
	}
	return []string{host}
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
