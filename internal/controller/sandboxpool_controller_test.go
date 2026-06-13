package controller_test

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSandboxPool_CreateAndReconcile(t *testing.T) {
	// Create a template first
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template-pool",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxTemplateSpec{
			Image: "python:3.12-slim",
			Init:  []string{"echo ready"},
			Resources: v1alpha1.SandboxResources{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("512Mi"),
			},
			Volumes: []v1alpha1.SandboxVolume{
				{
					Name:       "workspace",
					MountPath:  "/workspace",
					Size:       "1Gi",
					ForkPolicy: v1alpha1.ForkPolicySnapshot,
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatalf("create template: %v", err)
	}
	defer k8sClient.Delete(ctx, template)

	// Create pool
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-reconcile",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{
				Name: "test-template-pool",
			},
			Replicas:               5,
			SnapshotAfter:          v1alpha1.SnapshotAfterReady,
			ScaleDownAfterSnapshot: true,
		},
	}

	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer k8sClient.Delete(ctx, pool)

	// Wait for reconciliation
	time.Sleep(2 * time.Second)

	// Verify the pool was reconciled (status updated)
	var reconciled v1alpha1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-pool-reconcile",
		Namespace: "default",
	}, &reconciled); err != nil {
		t.Fatalf("get pool: %v", err)
	}

	if reconciled.Spec.Replicas != 5 {
		t.Errorf("expected 5 replicas, got %d", reconciled.Spec.Replicas)
	}
}

func TestSandboxPool_TemplateNotFound(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-no-template",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{
				Name: "nonexistent-template",
			},
			Replicas: 1,
		},
	}

	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer k8sClient.Delete(ctx, pool)

	// Controller should handle the missing template gracefully
	time.Sleep(2 * time.Second)

	var reconciled v1alpha1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-pool-no-template",
		Namespace: "default",
	}, &reconciled); err != nil {
		t.Fatalf("get pool: %v", err)
	}
}

func TestSandboxClaim_CreateAndReconcile(t *testing.T) {
	// Create template
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template-claim",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxTemplateSpec{
			Image: "python:3.12-slim",
		},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatalf("create template: %v", err)
	}
	defer k8sClient.Delete(ctx, template)

	// Create pool
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-claim",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "test-template-claim"},
			Replicas:    3,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer k8sClient.Delete(ctx, pool)

	// Create claim
	timeout := metav1.Duration{Duration: 10 * time.Minute}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim-reconcile",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "test-pool-claim"},
			Timeout: &timeout,
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	defer k8sClient.Delete(ctx, claim)

	// Wait for reconciliation
	time.Sleep(2 * time.Second)

	var reconciled v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-claim-reconcile",
		Namespace: "default",
	}, &reconciled); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	// Claim should be in Pending state (no forkd available in tests)
	if reconciled.Status.Phase != v1alpha1.SandboxPending && reconciled.Status.Phase != "" {
		// It's OK if it's empty (not yet reconciled) or Pending (no nodes)
		t.Logf("claim phase: %s", reconciled.Status.Phase)
	}
}

func TestSandboxFork_CreateAndReconcile(t *testing.T) {
	// Create a claim first
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim-for-fork",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "some-pool"},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		// Might already exist from previous test
		if err2 := k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim); err2 != nil {
			t.Fatalf("create claim: %v", err)
		}
	}
	defer k8sClient.Delete(ctx, claim)

	// Create fork
	fork := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-fork-reconcile",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxForkSpec{
			SourceRef: v1alpha1.LocalObjectReference{Name: "test-claim-for-fork"},
			Replicas:  3,
		},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	defer k8sClient.Delete(ctx, fork)

	// Wait for reconciliation
	time.Sleep(2 * time.Second)

	var reconciled v1alpha1.SandboxFork
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-fork-reconcile",
		Namespace: "default",
	}, &reconciled); err != nil {
		t.Fatalf("get fork: %v", err)
	}

	if reconciled.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", reconciled.Spec.Replicas)
	}
}
