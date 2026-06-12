package facade

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"

	runv1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
)

const (
	// TemplateAnnotation is the bridge annotation stamped on our agentrun.dev
	// SandboxTemplate that links it back to the upstream
	// extensions.agents.x-k8s.io SandboxTemplate it was created from. It records
	// the bridge in the same single-annotation style as PoolAnnotation
	// (docs/adr/0001-facade-and-naming.md): the value is the upstream
	// SandboxTemplate name.
	TemplateAnnotation = "agentrun.dev/template"

	// WarmPoolAnnotation is the bridge annotation stamped on our agentrun.dev
	// SandboxPool that links it back to the upstream
	// extensions.agents.x-k8s.io SandboxWarmPool it was created from. The value is
	// the upstream SandboxWarmPool name.
	WarmPoolAnnotation = "agentrun.dev/warmpool"
)

// SandboxTemplateReconciler maps an upstream
// extensions.agents.x-k8s.io/v1alpha1 SandboxTemplate onto our agentrun.dev
// SandboxTemplate. It owns exactly one of our SandboxTemplate objects per
// upstream template (same name + namespace, owner-referenced for GC), mapping
// the upstream podTemplate's first container (image, command, env) onto our
// template's fields. Unmapped upstream fields (volumeClaimTemplates,
// networkPolicy, securityContext, ports, multiple containers,
// envVarsInjectionPolicy, service) are documented justified exceptions in
// docs/facade-conformance.md (no silent divergence): the husk pool pins
// resources at build time and our engine is fork-from-snapshot, not pod-native.
type SandboxTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxtemplates,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures our agentrun.dev SandboxTemplate mirrors the upstream one.
// Deletion is handled by the owner-reference garbage collector: our template
// carries an owner reference to the upstream template, so deleting theirs GCs
// ours.
func (r *SandboxTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var src extv1alpha1.SandboxTemplate
	if err := r.Get(ctx, req.NamespacedName, &src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !src.DeletionTimestamp.IsZero() {
		// Owner-reference GC removes our template; nothing to do.
		return ctrl.Result{}, nil
	}

	if err := r.ensureTemplate(ctx, &src); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("mirrored upstream SandboxTemplate", "template", req.NamespacedName)
	return ctrl.Result{}, nil
}

// ensureTemplate creates or updates our SandboxTemplate for an upstream one.
// Our template is named after the upstream template, lives in the same
// namespace, and is owner-referenced to it (for GC + the watch back-link).
func (r *SandboxTemplateReconciler) ensureTemplate(ctx context.Context, src *extv1alpha1.SandboxTemplate) error {
	tmpl := &runv1alpha1.SandboxTemplate{
		ObjectMeta: metaName(src.Name, src.Namespace),
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, tmpl, func() error {
		if tmpl.Annotations == nil {
			tmpl.Annotations = map[string]string{}
		}
		tmpl.Annotations[TemplateAnnotation] = src.Name

		container := firstTemplateContainer(src)
		if container != nil {
			tmpl.Spec.Image = container.Image
			tmpl.Spec.Command = container.Command
			tmpl.Spec.Env = container.Env
		}
		return controllerutil.SetControllerReference(src, tmpl, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure SandboxTemplate for upstream %s/%s: %w", src.Namespace, src.Name, err)
	}
	return nil
}

// metaName builds an ObjectMeta naming an object in a given namespace; the
// bridged objects share name + namespace with their upstream source.
func metaName(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace}
}

// firstTemplateContainer returns the first container of the upstream template's
// podTemplate, or nil when the template carries none. Sandboxes are
// single-workload by construction; additional containers are a documented
// exception (docs/facade-conformance.md).
func firstTemplateContainer(src *extv1alpha1.SandboxTemplate) *corev1.Container {
	containers := src.Spec.PodTemplate.Spec.Containers
	if len(containers) == 0 {
		return nil
	}
	return &containers[0]
}

// SetupWithManager wires the reconciler to watch upstream SandboxTemplates and
// own our SandboxTemplate objects.
func (r *SandboxTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extv1alpha1.SandboxTemplate{}).
		Owns(&runv1alpha1.SandboxTemplate{}).
		Complete(r)
}
