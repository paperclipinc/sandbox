package facade_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"

	runv1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/facade"
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
// reconciles to our mitos.run SandboxTemplate, mapping the first container's
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
// reconciles to our mitos.run SandboxPool at the requested replicas, pointing
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

	// The mirrored scale-subresource selector must match the husk pods of the
	// mapped pool (mitos.run/pool=<pool>,mitos.run/husk=true, the exact
	// keys/values buildHuskPod stamps), so an HPA reading pod-resource metrics
	// finds the real husk pods. The pool and warm pool share the same name.
	wantSelector := "mitos.run/pool=ext-warmpool,mitos.run/husk=true"
	eventually(t, "the facade mirrors a husk-pod-matching status.selector", func() bool {
		var cur extv1alpha1.SandboxWarmPool
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: "ext-warmpool", Namespace: "default"}, &cur); err != nil {
			return false
		}
		return cur.Status.Selector == wantSelector
	})
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
	updateWithRetry(t, types.NamespacedName{Name: "ext-warmpool-hpa", Namespace: "default"}, &cur, func() {
		cur.Spec.Replicas = 5
	})

	eventually(t, "our pool follows the upstream replica change to 5", func() bool {
		p, ok := getOurPool(t, "ext-warmpool-hpa")
		return ok && p.Spec.Replicas == 5
	})
}

// mkOurPool creates one of our SandboxPools bound to a template name, so the
// claim reconciler's default/named resolution has a pool to bind to.
func mkOurPool(t *testing.T, name, templateName string) *runv1alpha1.SandboxPool {
	t.Helper()
	pool := &runv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: runv1alpha1.SandboxPoolSpec{
			TemplateRef: runv1alpha1.LocalObjectReference{Name: templateName},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(testCtx, pool); err != nil {
		t.Fatalf("create our pool %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, pool) })
	return pool
}

// newExtClaim builds an upstream extension SandboxClaim referencing a template
// with an optional warmpool policy.
func newExtClaim(name, templateName string, policy *extv1alpha1.WarmPoolPolicy) *extv1alpha1.SandboxClaim {
	return &extv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: extv1alpha1.SandboxClaimSpec{
			TemplateRef: extv1alpha1.SandboxTemplateRef{Name: templateName},
			WarmPool:    policy,
		},
	}
}

func getOurClaimT(t *testing.T, name string) (*runv1alpha1.SandboxClaim, bool) {
	t.Helper()
	return getClaim(t, name)
}

// TestFacadeClaimDefaultBindsMatchingPool: an upstream SandboxClaim with the
// default warmpool policy binds our claim from any of our pools whose templateRef
// matches the resolved template.
func TestFacadeClaimDefaultBindsMatchingPool(t *testing.T) {
	mkOurPool(t, "claim-default-pool", "claim-default-template")
	policy := extv1alpha1.WarmPoolPolicyDefault
	src := newExtClaim("ext-claim-default", "claim-default-template", &policy)
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	var claim *runv1alpha1.SandboxClaim
	eventually(t, "default policy binds our claim from a matching pool", func() bool {
		c, ok := getOurClaimT(t, "ext-claim-default")
		claim = c
		return ok && c.Spec.PoolRef.Name == "claim-default-pool"
	})
	if claim.Annotations[facade.WarmPoolPolicyAnnotation] != "default" {
		t.Fatalf("claim warmpool-policy annotation = %q, want default", claim.Annotations[facade.WarmPoolPolicyAnnotation])
	}
	if !hasControllerOwner2(claim, "ext-claim-default") {
		t.Fatalf("claim missing controller owner reference to the upstream SandboxClaim: %+v", claim.OwnerReferences)
	}
}

// TestFacadeClaimNamedBindsNamedPool: an upstream SandboxClaim with a named
// warmpool policy binds our claim from that specific named pool (the bridge).
func TestFacadeClaimNamedBindsNamedPool(t *testing.T) {
	// Two pools for the same template; the named policy must pick the named one,
	// not the default match.
	mkOurPool(t, "claim-other-pool", "claim-named-template")
	mkOurPool(t, "claim-fast-pool", "claim-named-template")
	named := extv1alpha1.WarmPoolPolicy("claim-fast-pool")
	src := newExtClaim("ext-claim-named", "claim-named-template", &named)
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	var claim *runv1alpha1.SandboxClaim
	eventually(t, "named policy binds the named pool", func() bool {
		c, ok := getOurClaimT(t, "ext-claim-named")
		claim = c
		return ok && c.Spec.PoolRef.Name == "claim-fast-pool"
	})
	if claim.Annotations[facade.WarmPoolPolicyAnnotation] != "claim-fast-pool" {
		t.Fatalf("claim warmpool-policy annotation = %q, want claim-fast-pool", claim.Annotations[facade.WarmPoolPolicyAnnotation])
	}
}

// TestFacadeClaimNonePolicy: an upstream SandboxClaim with the none warmpool
// policy is still forked from the resolved template's pool (our engine has no
// pool-less path, a documented exception) and records the none policy.
func TestFacadeClaimNonePolicy(t *testing.T) {
	mkOurPool(t, "claim-none-pool", "claim-none-template")
	none := extv1alpha1.WarmPoolPolicyNone
	src := newExtClaim("ext-claim-none", "claim-none-template", &none)
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	var claim *runv1alpha1.SandboxClaim
	eventually(t, "none policy forks from the template pool (documented exception)", func() bool {
		c, ok := getOurClaimT(t, "ext-claim-none")
		claim = c
		return ok && c.Spec.PoolRef.Name == "claim-none-pool"
	})
	if claim.Annotations[facade.WarmPoolPolicyAnnotation] != "none" {
		t.Fatalf("claim warmpool-policy annotation = %q, want none", claim.Annotations[facade.WarmPoolPolicyAnnotation])
	}
}

// TestFacadeClaimMirrorsStatusAndGCs: when our claim reaches Ready, the facade
// mirrors a Ready=True condition + the bound sandbox name into the upstream
// claim status; deleting the upstream claim leaves our claim owner-referenced for
// GC.
func TestFacadeClaimMirrorsStatusAndGCs(t *testing.T) {
	mkOurPool(t, "claim-status-pool", "claim-status-template")
	policy := extv1alpha1.WarmPoolPolicyDefault
	src := newExtClaim("ext-claim-status", "claim-status-template", &policy)
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext claim: %v", err)
	}

	var claim *runv1alpha1.SandboxClaim
	eventually(t, "facade creates our claim", func() bool {
		c, ok := getOurClaimT(t, "ext-claim-status")
		claim = c
		return ok
	})

	// Drive our claim Ready (the test seam the real husk activation path sets).
	statusUpdateWithRetry(t, types.NamespacedName{Name: "ext-claim-status", Namespace: "default"}, claim, func() {
		claim.Status.Phase = runv1alpha1.SandboxReady
		claim.Status.Endpoint = "10.0.0.9:9091"
	})

	eventually(t, "upstream claim status mirrors Bound/Ready + sandbox name", func() bool {
		var got extv1alpha1.SandboxClaim
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: "ext-claim-status", Namespace: "default"}, &got); err != nil {
			return false
		}
		cond := apimetaFind(got.Status.Conditions, "Ready")
		return cond != nil && cond.Status == metav1.ConditionTrue && got.Status.SandboxStatus.Name == "ext-claim-status"
	})

	// Deletion: our claim stays owner-referenced for GC (envtest has no GC, assert
	// the linkage like the core delete test does).
	if err := k8sClient.Delete(testCtx, src); err != nil {
		t.Fatalf("delete upstream claim: %v", err)
	}
	got, ok := getOurClaimT(t, "ext-claim-status")
	if ok && !hasControllerOwner2(got, "ext-claim-status") {
		t.Fatalf("our claim missing controller owner reference to the upstream claim: %+v", got.OwnerReferences)
	}
	if ok {
		_ = k8sClient.Delete(testCtx, got)
	}
}

// hasControllerOwner2 reports whether our claim carries a controller owner
// reference to the upstream SandboxClaim of the given name.
func hasControllerOwner2(claim *runv1alpha1.SandboxClaim, name string) bool {
	for _, o := range claim.OwnerReferences {
		if o.Kind == "SandboxClaim" && o.Name == name && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// apimetaFind finds a status condition by type.
func apimetaFind(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
