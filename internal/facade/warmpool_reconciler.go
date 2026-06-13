package facade

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"

	runv1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
)

// SandboxWarmPoolReconciler maps an upstream
// extensions.agents.x-k8s.io/v1alpha1 SandboxWarmPool onto our mitos.run
// SandboxPool. It owns exactly one of our SandboxPool objects per upstream
// warm pool (same name + namespace, owner-referenced for GC), setting
// Spec.Replicas from their spec.replicas (re-read EVERY reconcile so an HPA that
// scales their warm pool is honored) and pointing the pool at our template
// resolved from their sandboxTemplateRef (the template reconciler creates our
// template under the same name). It mirrors our pool's ready/warm count back
// into their status (replicas + readyReplicas + selector).
//
// Their UpdateStrategy (Recreate / OnReplenish) is a documented justified
// exception: our husk warm pool self-heals dormant slots and rebuilds on a
// template-snapshot change; we do not expose the upstream per-pod rollout knob.
// Recorded in docs/facade-conformance.md (no silent divergence).
type SandboxWarmPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools/status,verbs=get

// Reconcile ensures our mitos.run SandboxPool mirrors the upstream warm pool
// at the requested replicas and mirrors our pool status back. Deletion is
// handled by the owner-reference garbage collector.
func (r *SandboxWarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var src extv1alpha1.SandboxWarmPool
	if err := r.Get(ctx, req.NamespacedName, &src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !src.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	pool, err := r.ensurePool(ctx, &src)
	if err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("mirrored upstream SandboxWarmPool", "warmpool", req.NamespacedName, "replicas", src.Spec.Replicas)

	return ctrl.Result{}, r.mirrorStatus(ctx, &src, pool)
}

// ensurePool creates or updates our SandboxPool for an upstream warm pool. The
// pool is named after the warm pool, lives in the same namespace, is
// owner-referenced to it, points at our template (their sandboxTemplateRef
// resolved by name, since the template reconciler mirrors under the same name),
// and carries the requested replica count. The replica count is read EVERY
// reconcile so an HPA-controlled change to their spec.replicas is propagated.
func (r *SandboxWarmPoolReconciler) ensurePool(ctx context.Context, src *extv1alpha1.SandboxWarmPool) (*runv1alpha1.SandboxPool, error) {
	pool := &runv1alpha1.SandboxPool{
		ObjectMeta: metaName(src.Name, src.Namespace),
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pool, func() error {
		if pool.Annotations == nil {
			pool.Annotations = map[string]string{}
		}
		pool.Annotations[WarmPoolAnnotation] = src.Name
		pool.Annotations[TemplateAnnotation] = src.Spec.TemplateRef.Name
		pool.Spec.TemplateRef = runv1alpha1.LocalObjectReference{Name: src.Spec.TemplateRef.Name}
		pool.Spec.Replicas = src.Spec.Replicas
		return controllerutil.SetControllerReference(src, pool, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure SandboxPool for upstream warm pool %s/%s: %w", src.Namespace, src.Name, err)
	}
	return pool, nil
}

// huskPoolLabel and huskLabel are the labels our controller stamps on every
// husk pod of a pool (controller.buildHuskPod): mitos.run/pool=<pool-name>
// plus mitos.run/husk=true. The mirrored scale selector below is built from
// exactly these so it matches the real husk pods; the values MUST track
// internal/controller/huskpod.go.
const (
	huskPoolLabel = "mitos.run/pool"
	huskLabel     = "mitos.run/husk"
)

// mirrorStatus writes our pool's warm-slot counts back into the upstream warm
// pool status (replicas + readyReplicas + the scale selector), idempotently (no
// write when nothing changed). Our pool reports ReadySnapshots/TotalSnapshots;
// we mirror ReadySnapshots into readyReplicas and the desired replica count into
// replicas, matching the upstream scale subresource contract.
func (r *SandboxWarmPoolReconciler) mirrorStatus(ctx context.Context, src *extv1alpha1.SandboxWarmPool, pool *runv1alpha1.SandboxPool) error {
	wantReplicas := pool.Spec.Replicas
	wantReady := pool.Status.ReadySnapshots
	// The scale-subresource selector must match the pool's husk pods, NOT a
	// mitos.run/warmpool label (no pod carries one). buildHuskPod labels each
	// husk pod mitos.run/pool=<pool-name>,mitos.run/husk=true, and our pool
	// shares the warm pool's name, so build the selector from those exact keys.
	// A wrong key matches zero pods and breaks HPA pod-resource-metric reads.
	wantSelector := fmt.Sprintf("%s=%s,%s=true", huskPoolLabel, pool.Name, huskLabel)

	if src.Status.Replicas == wantReplicas &&
		src.Status.ReadyReplicas == wantReady &&
		src.Status.Selector == wantSelector {
		return nil
	}
	src.Status.Replicas = wantReplicas
	src.Status.ReadyReplicas = wantReady
	src.Status.Selector = wantSelector
	if err := r.Status().Update(ctx, src); err != nil {
		return fmt.Errorf("mirror status into warm pool %s/%s: %w", src.Namespace, src.Name, err)
	}
	return nil
}

// SetupWithManager wires the reconciler to watch upstream SandboxWarmPools and
// own our SandboxPool objects so a pool status change re-queues the warm pool.
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extv1alpha1.SandboxWarmPool{}).
		Owns(&runv1alpha1.SandboxPool{}).
		Complete(r)
}
