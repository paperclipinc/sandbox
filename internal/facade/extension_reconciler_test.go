package facade_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"

	runv1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/facade"
)

// newExtTemplate builds a minimal valid upstream extension SandboxTemplate: the
// podTemplate is required, so it carries a single container with the mapped
// fields (image, command, env).
func newExtTemplate(name string) *extv1alpha1.SandboxTemplate {
	return &extv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: extv1alpha1.SandboxTemplateSpec{
			PodTemplate: agentsv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "agent",
							Image:   "python:3.11-slim",
							Command: []string{"/bin/sh", "-c", "sleep 1"},
							Env:     []corev1.EnvVar{{Name: "K", Value: "v"}},
						},
					},
				},
			},
		},
	}
}

func getOurTemplate(t *testing.T, name string) (*runv1alpha1.SandboxTemplate, bool) {
	t.Helper()
	var tmpl runv1alpha1.SandboxTemplate
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &tmpl)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get our template %s: %v", name, err)
	}
	return &tmpl, true
}

func getOurPool(t *testing.T, name string) (*runv1alpha1.SandboxPool, bool) {
	t.Helper()
	var pool runv1alpha1.SandboxPool
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &pool)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get our pool %s: %v", name, err)
	}
	return &pool, true
}

// TestFacadeMapsExtSandboxTemplate: an upstream extension SandboxTemplate
// reconciles to our agentrun.dev SandboxTemplate, mapping the first container's
// image/command/env, stamping the bridge annotation, and owner-referenced for GC.
func TestFacadeMapsExtSandboxTemplate(t *testing.T) {
	src := newExtTemplate("ext-template")
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext template: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	var tmpl *runv1alpha1.SandboxTemplate
	eventually(t, "facade maps the ext SandboxTemplate to our template", func() bool {
		tt, ok := getOurTemplate(t, "ext-template")
		tmpl = tt
		return ok
	})

	if tmpl.Spec.Image != "python:3.11-slim" {
		t.Fatalf("our template image = %q, want python:3.11-slim", tmpl.Spec.Image)
	}
	if len(tmpl.Spec.Command) != 3 || tmpl.Spec.Command[0] != "/bin/sh" {
		t.Fatalf("our template command = %+v, want the upstream container command", tmpl.Spec.Command)
	}
	if len(tmpl.Spec.Env) != 1 || tmpl.Spec.Env[0].Name != "K" || tmpl.Spec.Env[0].Value != "v" {
		t.Fatalf("our template env = %+v, want K=v", tmpl.Spec.Env)
	}
	if tmpl.Annotations[facade.TemplateAnnotation] != "ext-template" {
		t.Fatalf("our template bridge annotation = %q, want ext-template", tmpl.Annotations[facade.TemplateAnnotation])
	}
	if len(tmpl.OwnerReferences) != 1 || tmpl.OwnerReferences[0].Kind != "SandboxTemplate" || tmpl.OwnerReferences[0].Name != "ext-template" {
		t.Fatalf("our template owner refs = %+v, want a single SandboxTemplate owner", tmpl.OwnerReferences)
	}
}

// newExtWarmPool builds an upstream extension SandboxWarmPool referencing a
// template by name at the requested replicas.
func newExtWarmPool(name, templateName string, replicas int32) *extv1alpha1.SandboxWarmPool {
	return &extv1alpha1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: extv1alpha1.SandboxWarmPoolSpec{
			Replicas:    replicas,
			TemplateRef: extv1alpha1.SandboxTemplateRef{Name: templateName},
		},
	}
}

// TestFacadeMapsExtSandboxWarmPool: an upstream extension SandboxWarmPool
// reconciles to our agentrun.dev SandboxPool at the requested replicas, pointing
// at our template, owner-referenced and bridge-annotated.
func TestFacadeMapsExtSandboxWarmPool(t *testing.T) {
	src := newExtWarmPool("ext-warmpool", "ext-wp-template", 3)
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext warm pool: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	var pool *runv1alpha1.SandboxPool
	eventually(t, "facade maps the ext SandboxWarmPool to our pool", func() bool {
		p, ok := getOurPool(t, "ext-warmpool")
		pool = p
		return ok
	})

	if pool.Spec.Replicas != 3 {
		t.Fatalf("our pool replicas = %d, want 3", pool.Spec.Replicas)
	}
	if pool.Spec.TemplateRef.Name != "ext-wp-template" {
		t.Fatalf("our pool templateRef = %q, want ext-wp-template", pool.Spec.TemplateRef.Name)
	}
	if pool.Annotations[facade.WarmPoolAnnotation] != "ext-warmpool" {
		t.Fatalf("our pool warmpool bridge annotation = %q, want ext-warmpool", pool.Annotations[facade.WarmPoolAnnotation])
	}
	if len(pool.OwnerReferences) != 1 || pool.OwnerReferences[0].Kind != "SandboxWarmPool" || pool.OwnerReferences[0].Name != "ext-warmpool" {
		t.Fatalf("our pool owner refs = %+v, want a single SandboxWarmPool owner", pool.OwnerReferences)
	}
}

// TestFacadeWarmPoolReplicasFollowUpstream: changing the upstream warm pool's
// spec.replicas (as an HPA would) updates our pool's replicas; the facade
// re-reads the replica count every reconcile.
func TestFacadeWarmPoolReplicasFollowUpstream(t *testing.T) {
	src := newExtWarmPool("ext-warmpool-hpa", "ext-hpa-template", 1)
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext warm pool: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	eventually(t, "our pool starts at 1 replica", func() bool {
		p, ok := getOurPool(t, "ext-warmpool-hpa")
		return ok && p.Spec.Replicas == 1
	})

	// Simulate an HPA scaling their warm pool to 5.
	var cur extv1alpha1.SandboxWarmPool
	if err := k8sClient.Get(testCtx, types.NamespacedName{Name: "ext-warmpool-hpa", Namespace: "default"}, &cur); err != nil {
		t.Fatalf("get ext warm pool: %v", err)
	}
	cur.Spec.Replicas = 5
	if err := k8sClient.Update(testCtx, &cur); err != nil {
		t.Fatalf("scale ext warm pool to 5: %v", err)
	}

	eventually(t, "our pool follows the upstream replica change to 5", func() bool {
		p, ok := getOurPool(t, "ext-warmpool-hpa")
		return ok && p.Spec.Replicas == 5
	})
}
