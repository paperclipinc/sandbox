package controller_test

import (
	"bytes"
	"testing"

	"github.com/paperclipinc/mitos/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReplicateHuskSecretsCopiesCAcrtAndForkdTLS(t *testing.T) {
	c := newCoreClient(t)
	src := newPKINamespace(t, c)
	dst := newPKINamespace(t, c)

	// Seed the source secrets directly (no real CA needed; replication copies
	// bytes, it does not re-issue).
	caSrc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: src, Name: controller.CASecretName},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"ca.crt": []byte("CA-CERT"), "ca.key": []byte("CA-KEY-SECRET")},
	}
	tlsSrc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: src, Name: controller.ForkdTLSSecretName},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("LEAF-CERT"), "tls.key": []byte("LEAF-KEY")},
	}
	if err := c.Create(ctx, caSrc); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, tlsSrc); err != nil {
		t.Fatal(err)
	}

	if err := controller.ReplicateHuskSecrets(ctx, c, src, dst); err != nil {
		t.Fatalf("ReplicateHuskSecrets: %v", err)
	}

	gotCA := getSecret(t, c, dst, controller.CASecretName)
	if !bytes.Equal(gotCA.Data["ca.crt"], []byte("CA-CERT")) {
		t.Errorf("dst ca.crt = %q, want CA-CERT", gotCA.Data["ca.crt"])
	}
	if _, leaked := gotCA.Data["ca.key"]; leaked {
		t.Error("dst CA secret leaked ca.key; the CA private key must never be replicated")
	}
	gotTLS := getSecret(t, c, dst, controller.ForkdTLSSecretName)
	if !bytes.Equal(gotTLS.Data["tls.crt"], []byte("LEAF-CERT")) || !bytes.Equal(gotTLS.Data["tls.key"], []byte("LEAF-KEY")) {
		t.Errorf("dst forkd-tls not copied: %v", gotTLS.Data)
	}
}

func TestReplicateHuskSecretsIsIdempotentAndHealsDrift(t *testing.T) {
	c := newCoreClient(t)
	src := newPKINamespace(t, c)
	dst := newPKINamespace(t, c)
	caSrc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: src, Name: controller.CASecretName},
		Data:       map[string][]byte{"ca.crt": []byte("CA-V1"), "ca.key": []byte("KEY")},
	}
	tlsSrc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: src, Name: controller.ForkdTLSSecretName},
		Data:       map[string][]byte{"tls.crt": []byte("LEAF-V1"), "tls.key": []byte("LK")},
	}
	if err := c.Create(ctx, caSrc); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, tlsSrc); err != nil {
		t.Fatal(err)
	}
	// First replication creates the destination copies.
	if err := controller.ReplicateHuskSecrets(ctx, c, src, dst); err != nil {
		t.Fatal(err)
	}
	// Source rotates; a second replication must heal the destination in place.
	caSrc.Data["ca.crt"] = []byte("CA-V2")
	if err := c.Update(ctx, caSrc); err != nil {
		t.Fatal(err)
	}
	if err := controller.ReplicateHuskSecrets(ctx, c, src, dst); err != nil {
		t.Fatalf("second ReplicateHuskSecrets: %v", err)
	}
	gotCA := getSecret(t, c, dst, controller.CASecretName)
	if !bytes.Equal(gotCA.Data["ca.crt"], []byte("CA-V2")) {
		t.Errorf("drift not healed: dst ca.crt = %q, want CA-V2", gotCA.Data["ca.crt"])
	}
}

func TestReplicateHuskSecretsSameNamespaceIsNoop(t *testing.T) {
	c := newCoreClient(t)
	ns := newPKINamespace(t, c)
	// No source secrets exist; replicating into the same namespace must not
	// error (the controller namespace already holds the originals).
	if err := controller.ReplicateHuskSecrets(ctx, c, ns, ns); err != nil {
		t.Fatalf("same-namespace replication should be a noop, got %v", err)
	}
}

func TestReconcileHuskPodsReplicatesSecretsIntoPoolNamespace(t *testing.T) {
	c := newCoreClient(t)
	ctrlNS := newPKINamespace(t, c)
	poolNS := newPKINamespace(t, c)

	// Seed the control plane secrets in the controller namespace, as EnsurePKI
	// would.
	if err := c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ctrlNS, Name: controller.CASecretName},
		Data:       map[string][]byte{"ca.crt": []byte("CA"), "ca.key": []byte("KEY")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ctrlNS, Name: controller.ForkdTLSSecretName},
		Data:       map[string][]byte{"tls.crt": []byte("L"), "tls.key": []byte("K")},
	}); err != nil {
		t.Fatal(err)
	}

	// ReplicateHuskSecrets is what reconcileHuskPods calls; assert it bridges
	// ctrlNS -> poolNS (the reconcile-level wiring is covered by the existing
	// huskpod envtest, which now runs with ControllerNamespace set).
	if err := controller.ReplicateHuskSecrets(ctx, c, ctrlNS, poolNS); err != nil {
		t.Fatal(err)
	}
	_ = getSecret(t, c, poolNS, controller.CASecretName)
	_ = getSecret(t, c, poolNS, controller.ForkdTLSSecretName)
}
