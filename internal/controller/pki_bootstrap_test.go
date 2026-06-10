package controller_test

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/paperclipinc/sandbox/internal/controller"
	"github.com/paperclipinc/sandbox/internal/pki"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newCoreClient builds a direct envtest client whose scheme knows core/v1,
// independent of the suite's shared scheme.
func newCoreClient(t *testing.T) client.Client {
	t.Helper()
	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newPKINamespace creates a fresh namespace so each test sees a clean slate.
func newPKINamespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pki-test-"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	return ns.Name
}

func getSecret(t *testing.T, c client.Client, namespace, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &s); err != nil {
		t.Fatalf("get secret %s/%s: %v", namespace, name, err)
	}
	return &s
}

// verifyLeaf checks that certPEM chains to caPEM with the given SAN and EKU.
func verifyLeaf(t *testing.T, certPEM, caPEM []byte, dnsName string, eku x509.ExtKeyUsage) {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("stored ca.crt contains no valid certificates")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("leaf tls.crt is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   dnsName,
		KeyUsages: []x509.ExtKeyUsage{eku},
	}); err != nil {
		t.Fatalf("leaf does not verify against stored CA for %s: %v", dnsName, err)
	}
}

func TestEnsurePKICreatesAllSecrets(t *testing.T) {
	c := newCoreClient(t)
	ns := newPKINamespace(t, c)

	tlsConf, err := controller.EnsurePKI(ctx, c, ns)
	if err != nil {
		t.Fatalf("EnsurePKI: %v", err)
	}
	if tlsConf == nil {
		t.Fatal("EnsurePKI returned nil tls.Config")
	}
	if tlsConf.ServerName != pki.ServerName {
		t.Fatalf("client config ServerName = %q, want %q", tlsConf.ServerName, pki.ServerName)
	}
	if len(tlsConf.Certificates) != 1 {
		t.Fatalf("client config has %d certificates, want 1", len(tlsConf.Certificates))
	}

	caSecret := getSecret(t, c, ns, controller.CASecretName)
	for _, key := range []string{"ca.crt", "ca.key"} {
		if len(caSecret.Data[key]) == 0 {
			t.Fatalf("CA secret missing key %s", key)
		}
	}

	forkdSecret := getSecret(t, c, ns, controller.ForkdTLSSecretName)
	ctrlSecret := getSecret(t, c, ns, controller.ControllerTLSSecretName)
	for name, s := range map[string]*corev1.Secret{
		controller.ForkdTLSSecretName:      forkdSecret,
		controller.ControllerTLSSecretName: ctrlSecret,
	} {
		for _, key := range []string{"tls.crt", "tls.key"} {
			if len(s.Data[key]) == 0 {
				t.Fatalf("secret %s missing key %s", name, key)
			}
		}
	}

	verifyLeaf(t, forkdSecret.Data["tls.crt"], caSecret.Data["ca.crt"], pki.ServerName, x509.ExtKeyUsageServerAuth)
	verifyLeaf(t, ctrlSecret.Data["tls.crt"], caSecret.Data["ca.crt"], pki.ControllerName, x509.ExtKeyUsageClientAuth)
}

func TestEnsurePKISecondCallChangesNothing(t *testing.T) {
	c := newCoreClient(t)
	ns := newPKINamespace(t, c)

	if _, err := controller.EnsurePKI(ctx, c, ns); err != nil {
		t.Fatalf("first EnsurePKI: %v", err)
	}
	before := map[string]string{}
	for _, name := range []string{controller.CASecretName, controller.ForkdTLSSecretName, controller.ControllerTLSSecretName} {
		before[name] = getSecret(t, c, ns, name).ResourceVersion
	}

	if _, err := controller.EnsurePKI(ctx, c, ns); err != nil {
		t.Fatalf("second EnsurePKI: %v", err)
	}
	for name, rv := range before {
		if got := getSecret(t, c, ns, name).ResourceVersion; got != rv {
			t.Fatalf("secret %s resourceVersion changed on second call: %s -> %s", name, rv, got)
		}
	}
}

func TestEnsurePKIHealsDeletedControllerLeaf(t *testing.T) {
	c := newCoreClient(t)
	ns := newPKINamespace(t, c)

	if _, err := controller.EnsurePKI(ctx, c, ns); err != nil {
		t.Fatalf("first EnsurePKI: %v", err)
	}
	caSecret := getSecret(t, c, ns, controller.CASecretName)
	caRV := caSecret.ResourceVersion
	caCert := caSecret.Data["ca.crt"]

	if err := c.Delete(ctx, getSecret(t, c, ns, controller.ControllerTLSSecretName)); err != nil {
		t.Fatalf("delete controller leaf secret: %v", err)
	}

	tlsConf, err := controller.EnsurePKI(ctx, c, ns)
	if err != nil {
		t.Fatalf("EnsurePKI after leaf deletion: %v", err)
	}
	if tlsConf == nil {
		t.Fatal("EnsurePKI returned nil tls.Config after healing")
	}

	healed := getSecret(t, c, ns, controller.ControllerTLSSecretName)
	verifyLeaf(t, healed.Data["tls.crt"], caCert, pki.ControllerName, x509.ExtKeyUsageClientAuth)

	if got := getSecret(t, c, ns, controller.CASecretName).ResourceVersion; got != caRV {
		t.Fatalf("CA secret was rewritten while healing a leaf: resourceVersion %s -> %s", caRV, got)
	}
}
