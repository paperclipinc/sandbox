package controller_test

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestLiveForkOfSecretHolderIsRejectedByDefault(t *testing.T) {
	// Source claim that *declares* secrets. Readiness is irrelevant: the
	// policy gate is spec-level and must fire before any forkd call.
	source := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-holder", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "nonexistent-pool"},
			Secrets: []v1alpha1.SecretMount{{
				Name:      "k",
				SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "K"},
			}},
		},
	}
	if err := k8sClient.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	forkObj := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{Name: "denied-fork", Namespace: "default"},
		Spec: v1alpha1.SandboxForkSpec{
			SourceRef: v1alpha1.LocalObjectReference{Name: "secret-holder"},
			Replicas:  1,
		},
	}
	if err := k8sClient.Create(ctx, forkObj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, forkObj)
		_ = k8sClient.Delete(ctx, source)
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxFork
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "denied-fork", Namespace: "default"}, &got); err == nil {
			if c := meta.FindStatusCondition(got.Status.Conditions, "Rejected"); c != nil {
				if c.Reason != "SecretInheritanceDenied" {
					t.Fatalf("reason = %q", c.Reason)
				}
				if got.Status.ReadyForks != 0 {
					t.Fatalf("forks were created despite rejection")
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("fork was not rejected within 10s")
}

func TestLiveForkOptInProceedsPastTheGate(t *testing.T) {
	source := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-holder-2", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "nonexistent-pool"},
			Secrets: []v1alpha1.SecretMount{{
				Name:      "k",
				SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "K"},
			}},
		},
	}
	if err := k8sClient.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	forkObj := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{Name: "optin-fork", Namespace: "default"},
		Spec: v1alpha1.SandboxForkSpec{
			SourceRef:              v1alpha1.LocalObjectReference{Name: "secret-holder-2"},
			Replicas:               1,
			AllowSecretInheritance: true,
		},
	}
	if err := k8sClient.Create(ctx, forkObj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, forkObj)
		_ = k8sClient.Delete(ctx, source)
	})

	// The gate must record the audit condition and NOT reject. (The fork then
	// waits on source readiness, which never comes in this test; that's fine,
	// we are testing the gate, not the fork path.)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxFork
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "optin-fork", Namespace: "default"}, &got); err == nil {
			if meta.FindStatusCondition(got.Status.Conditions, "Rejected") != nil {
				t.Fatal("opt-in fork must not be rejected")
			}
			if c := meta.FindStatusCondition(got.Status.Conditions, "SecretInheritance"); c != nil {
				if c.Reason != "ExplicitOptIn" {
					t.Fatalf("reason = %q", c.Reason)
				}
				return // audit condition recorded, gate passed
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("audit condition not recorded within 10s")
}
