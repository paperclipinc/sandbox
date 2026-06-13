package controller_test

import (
	"crypto/tls"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/pki"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// newTestMTLSPair builds a fresh CA plus the forkd server and controller
// client TLS configs, exactly what EnsurePKI materializes from Secrets.
func newTestMTLSPair(t *testing.T) (serverTLS, clientTLS *tls.Config) {
	t.Helper()
	ca, err := pki.NewCA("mitos-test")
	if err != nil {
		t.Fatal(err)
	}
	serverLeaf, err := ca.Issue(pki.ServerName)
	if err != nil {
		t.Fatal(err)
	}
	clientLeaf, err := ca.Issue(pki.ControllerName)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err = pki.ServerTLSConfig(serverLeaf.CertPEM, serverLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err = pki.ClientTLSConfig(clientLeaf.CertPEM, clientLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	return serverTLS, clientTLS
}

// TestClaimReachesReadyOverMTLS proves the full claim path works when the
// fake forkd requires mTLS and only this node carries a TLS dialing config;
// the suite's other fakes keep dialing insecure (node-level TLS, not
// registry-level, so mixed fleets stay testable).
func TestClaimReachesReadyOverMTLS(t *testing.T) {
	serverTLS, clientTLS := newTestMTLSPair(t)

	stop, err := controller.StartFakeForkdNodeTLS(testRegistry, "e2e-tls-node-1", serverTLS, clientTLS, "e2e-tls-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-tls-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-tls-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "e2e-tls-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-tls-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "e2e-tls-pool"},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "e2e-tls-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxReady {
				if got.Status.Node != "e2e-tls-node-1" {
					t.Fatalf("node = %q, want e2e-tls-node-1", got.Status.Node)
				}
				return
			}
			if got.Status.Phase == v1alpha1.SandboxFailed {
				t.Fatalf("claim failed: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("claim did not become Ready over mTLS within 15s")
}
