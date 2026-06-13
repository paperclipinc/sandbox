package controller_test

import (
	"bytes"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/kms"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func encKeySecretName(templateID string) string { return templateID + "-enc-key" }

// waitFor polls cond until it returns true or the deadline passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

// TestEncryptedPoolCreatesKeySecretAndDelivers proves that a pool whose template
// is Encrypted causes the controller to create the <template>-enc-key Secret and
// deliver a non-empty EncryptionKey to forkd's CreateTemplate.
func TestEncryptedPoolCreatesKeySecretAndDelivers(t *testing.T) {
	// The key delivery guard requires an mTLS node, so this happy-path test runs
	// the fake forkd over mTLS.
	serverTLS, clientTLS := newTestMTLSPair(t)
	stop, rec, err := controller.StartFakeForkdNodeEncRecordingTLS(testRegistry, "enc-node-1", serverTLS, clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim", Encrypted: true},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "enc-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	// The key Secret is created holding ONLY the wrapped DEK plus the KEK id, and
	// NOT a raw plaintext key (the old "key" data key must be absent).
	var keySecret corev1.Secret
	ok := waitFor(15*time.Second, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Name: encKeySecretName("enc-tmpl"), Namespace: "default"}, &keySecret) == nil
	})
	if !ok {
		t.Fatal("enc-key Secret was not created for the encrypted template")
	}
	if len(keySecret.Data["wrapped-dek"]) == 0 {
		t.Fatal("enc-key Secret holds no wrapped-dek")
	}
	if _, hasRaw := keySecret.Data["key"]; hasRaw {
		t.Fatal("enc-key Secret holds a raw plaintext key; envelope custody must store only the wrapped DEK")
	}
	if string(keySecret.Data["kek-id"]) != testKMS.KEKID() {
		t.Fatalf("kek-id = %q, want %q", string(keySecret.Data["kek-id"]), testKMS.KEKID())
	}
	// The wrapped DEK must unwrap to a 32-byte DEK via the test KMS.
	dek, uerr := testKMS.Unwrap(ctx, kms.WrappedKey{KEKID: string(keySecret.Data["kek-id"]), Ciphertext: keySecret.Data["wrapped-dek"]})
	if uerr != nil {
		t.Fatalf("unwrap stored wrapped DEK: %v", uerr)
	}
	if len(dek) != 32 {
		t.Fatalf("unwrapped DEK length = %d, want 32", len(dek))
	}
	// Owner-referenced to the template so k8s GC crypto-shreds it on delete.
	if len(keySecret.OwnerReferences) == 0 || keySecret.OwnerReferences[0].Kind != "SandboxTemplate" {
		t.Fatalf("enc-key Secret is not owner-referenced to the template: %+v", keySecret.OwnerReferences)
	}

	// forkd's CreateTemplate received the WRAPPED DEK (non-empty) and the KEK id.
	if !waitFor(15*time.Second, func() bool {
		seen, n := rec.CreateTemplateKeyLen()
		return seen && n > 0
	}) {
		seen, n := rec.CreateTemplateKeyLen()
		t.Fatalf("CreateTemplate did not receive a wrapped DEK (seen=%v len=%d)", seen, n)
	}
	if got := rec.CreateTemplateKekID(); got != testKMS.KEKID() {
		t.Fatalf("CreateTemplate KekId = %q, want %q", got, testKMS.KEKID())
	}

	// Neither the wrapped DEK nor the (unwrapped) DEK value must appear in the
	// controller logs.
	logs := logBuf.Bytes()
	if bytes.Contains(logs, keySecret.Data["wrapped-dek"]) {
		t.Fatal("the wrapped DEK leaked into the controller logs")
	}
	if bytes.Contains(logs, dek) {
		t.Fatal("the plaintext DEK leaked into the controller logs")
	}
}

// TestEncryptedClaimDeliversKeyOnFork proves that a claim against an encrypted
// template delivers the key in the Fork RPC.
func TestEncryptedClaimDeliversKeyOnFork(t *testing.T) {
	// The key delivery guard requires an mTLS node, so this happy-path test runs
	// the fake forkd over mTLS.
	serverTLS, clientTLS := newTestMTLSPair(t)
	stop, rec, err := controller.StartFakeForkdNodeEncRecordingTLS(testRegistry, "enc-node-2", serverTLS, clientTLS, "enc-tmpl-claim")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-tmpl-claim", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim", Encrypted: true},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-pool-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "enc-tmpl-claim"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-claim", Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "enc-pool-claim"}},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	if !waitFor(20*time.Second, func() bool {
		seen, n := rec.ForkKeyLen()
		return seen && n > 0
	}) {
		seen, n := rec.ForkKeyLen()
		t.Fatalf("Fork did not receive a wrapped DEK (seen=%v len=%d)", seen, n)
	}
	if got := rec.ForkKekID(); got != testKMS.KEKID() {
		t.Fatalf("Fork KekId = %q, want %q", got, testKMS.KEKID())
	}
}

// TestEncryptedPoolRefusesKeyOverInsecureNode proves the fail-closed delivery
// guard: an encrypted template targeting a node whose connection is insecure
// (NodeInfo.TLS nil, registry.TLS nil) is refused. The build does not run, so
// CreateTemplate never carries the key (the fake forkd records no key), and the
// pool never reaches Ready.
func TestEncryptedPoolRefusesKeyOverInsecureNode(t *testing.T) {
	// An insecure fake node (no serverTLS/clientTLS): dials to it are not mTLS.
	stop, rec, err := controller.StartFakeForkdNodeEncRecording(testRegistry, "enc-insecure-node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-insecure-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim", Encrypted: true},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-insecure-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "enc-insecure-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	// Give the pool reconciler ample time to run and be refused, then assert the
	// key was never delivered to the insecure node.
	time.Sleep(3 * time.Second)
	if seen, n := rec.CreateTemplateKeyLen(); seen {
		t.Fatalf("CreateTemplate was called on an insecure node (key delivered, seen=%v len=%d); the guard must refuse before the RPC", seen, n)
	}
}

// TestEncryptedClaimRefusesKeyOverInsecureNode proves the fork-path delivery
// guard: a claim against an encrypted template whose node connection is
// insecure fails and the Fork RPC never carries the key. The node is seeded
// with the snapshot so the claim reaches the fork call, where the guard refuses.
func TestEncryptedClaimRefusesKeyOverInsecureNode(t *testing.T) {
	stop, rec, err := controller.StartFakeForkdNodeEncRecording(testRegistry, "enc-insecure-node-2", "enc-insecure-claim-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-insecure-claim-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim", Encrypted: true},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-insecure-claim-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "enc-insecure-claim-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "enc-insecure-claim", Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "enc-insecure-claim-pool"}},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	// The claim must fail (the fork is refused over the insecure channel), and the
	// Fork RPC must never have carried the key.
	if !waitFor(15*time.Second, func() bool {
		var got v1alpha1.SandboxClaim
		if k8sClient.Get(ctx, types.NamespacedName{Name: "enc-insecure-claim", Namespace: "default"}, &got) != nil {
			return false
		}
		return got.Status.Phase == v1alpha1.SandboxFailed
	}) {
		t.Fatal("encrypted claim against an insecure node did not fail; the fork-path guard must refuse to deliver the key")
	}
	if seen, n := rec.ForkKeyLen(); seen {
		t.Fatalf("Fork was called on an insecure node (key delivered, seen=%v len=%d); the guard must refuse before the RPC", seen, n)
	}
}

// TestPlaintextPoolCreatesNoKeySecret proves a non-encrypted template creates no
// key Secret and delivers no key to forkd.
func TestPlaintextPoolCreatesNoKeySecret(t *testing.T) {
	stop, rec, err := controller.StartFakeForkdNodeEncRecording(testRegistry, "plain-node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "plain-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	// Wait for the template to be built on the node (no key).
	if !waitFor(15*time.Second, func() bool {
		seen, n := rec.CreateTemplateKeyLen()
		return seen && n == 0
	}) {
		seen, n := rec.CreateTemplateKeyLen()
		t.Fatalf("plaintext CreateTemplate carried a key (seen=%v len=%d)", seen, n)
	}
	// No key Secret was created.
	var keySecret corev1.Secret
	err = k8sClient.Get(ctx, types.NamespacedName{Name: encKeySecretName("plain-tmpl"), Namespace: "default"}, &keySecret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no enc-key Secret for a plaintext template, got err=%v", err)
	}
}

// TestDeleteEncKeyShredsSecret proves DeleteEncKey removes the key Secret
// (crypto-shred) and is idempotent.
func TestDeleteEncKeyShredsSecret(t *testing.T) {
	// EnsureEncKey creates the Secret; DeleteEncKey removes it.
	wrapped, kekID, err := controller.EnsureEncKey(ctx, k8sClient, testKMS, "default", "shred-tmpl", nil)
	if err != nil {
		t.Fatalf("EnsureEncKey: %v", err)
	}
	if len(wrapped) == 0 {
		t.Fatal("EnsureEncKey returned an empty wrapped DEK")
	}
	if kekID != testKMS.KEKID() {
		t.Fatalf("EnsureEncKey kekID = %q, want %q", kekID, testKMS.KEKID())
	}
	// The wrapped DEK unwraps to a 32-byte DEK.
	dek, uerr := testKMS.Unwrap(ctx, kms.WrappedKey{KEKID: kekID, Ciphertext: wrapped})
	if uerr != nil {
		t.Fatalf("unwrap returned wrapped DEK: %v", uerr)
	}
	if len(dek) != 32 {
		t.Fatalf("unwrapped DEK length = %d, want 32", len(dek))
	}
	// Idempotent read returns the same wrapped DEK.
	wrapped2, _, err := controller.EnsureEncKey(ctx, k8sClient, testKMS, "default", "shred-tmpl", nil)
	if err != nil {
		t.Fatalf("EnsureEncKey read: %v", err)
	}
	if !bytes.Equal(wrapped, wrapped2) {
		t.Fatal("EnsureEncKey returned a different wrapped DEK on the idempotent read")
	}

	if err := controller.DeleteEncKey(ctx, k8sClient, "default", "shred-tmpl"); err != nil {
		t.Fatalf("DeleteEncKey: %v", err)
	}
	var s corev1.Secret
	gerr := k8sClient.Get(ctx, types.NamespacedName{Name: encKeySecretName("shred-tmpl"), Namespace: "default"}, &s)
	if !apierrors.IsNotFound(gerr) {
		t.Fatalf("enc-key Secret still present after DeleteEncKey: %v", gerr)
	}
	// Idempotent: deleting a missing Secret is not an error.
	if err := controller.DeleteEncKey(ctx, k8sClient, "default", "shred-tmpl"); err != nil {
		t.Fatalf("DeleteEncKey on missing secret should be a no-op, got %v", err)
	}
}
