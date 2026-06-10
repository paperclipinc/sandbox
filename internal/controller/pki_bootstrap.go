package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/paperclipinc/sandbox/internal/pki"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Names of the Secrets that hold the control plane PKI material.
const (
	// CASecretName holds the CA certificate and key (keys ca.crt, ca.key).
	CASecretName = "agent-run-ca"
	// ForkdTLSSecretName holds the forkd server leaf (keys tls.crt, tls.key);
	// the daemonset mounts it together with the CA secret.
	ForkdTLSSecretName = "agent-run-forkd-tls"
	// ControllerTLSSecretName holds the controller client leaf (keys
	// tls.crt, tls.key); consumed in-process by EnsurePKI itself.
	ControllerTLSSecretName = "agent-run-controller-tls"
)

const caOrganization = "agent-run"

// EnsurePKI idempotently materializes the control plane PKI in the given
// namespace: the CA secret plus the forkd server and controller client leaf
// Secrets. An existing CA is never regenerated; missing or unusable leaf
// Secrets are healed by issuing fresh leaves from the stored CA. Create
// races with a parallel controller replica are tolerated by re-reading.
// Returns the controller's mTLS client config for dialing forkd.
func EnsurePKI(ctx context.Context, c client.Client, namespace string) (*tls.Config, error) {
	ca, err := ensureCA(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	if _, err := ensureLeaf(ctx, c, namespace, ForkdTLSSecretName, pki.ServerName, ca); err != nil {
		return nil, err
	}
	controllerLeaf, err := ensureLeaf(ctx, c, namespace, ControllerTLSSecretName, pki.ControllerName, ca)
	if err != nil {
		return nil, err
	}
	tlsConf, err := pki.ClientTLSConfig(controllerLeaf.CertPEM, controllerLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		return nil, fmt.Errorf("build controller client TLS config: %w", err)
	}
	return tlsConf, nil
}

// ensureCA returns the CA from the CA secret, creating both when absent.
// A present CA is authoritative and is never regenerated: rewriting it
// would orphan every leaf already mounted by running forkd pods.
func ensureCA(ctx context.Context, c client.Client, namespace string) (*pki.CA, error) {
	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: CASecretName}, &secret)
	if err == nil {
		return caFromSecret(&secret)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get CA secret %s/%s: %w", namespace, CASecretName, err)
	}

	ca, err := pki.NewCA(caOrganization)
	if err != nil {
		return nil, fmt.Errorf("generate control plane CA: %w", err)
	}
	secret = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: CASecretName},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt": ca.CertPEM(),
			"ca.key": ca.KeyPEM(),
		},
	}
	if err := c.Create(ctx, &secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost the create race to a parallel replica; theirs wins.
			var existing corev1.Secret
			if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: CASecretName}, &existing); err != nil {
				return nil, fmt.Errorf("re-read CA secret %s/%s after create conflict: %w", namespace, CASecretName, err)
			}
			return caFromSecret(&existing)
		}
		return nil, fmt.Errorf("create CA secret %s/%s: %w", namespace, CASecretName, err)
	}
	return ca, nil
}

func caFromSecret(secret *corev1.Secret) (*pki.CA, error) {
	certPEM, keyPEM := secret.Data["ca.crt"], secret.Data["ca.key"]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, fmt.Errorf("CA secret %s/%s exists but lacks ca.crt or ca.key; refusing to overwrite it, fix or delete the secret", secret.Namespace, secret.Name)
	}
	ca, err := pki.LoadCA(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load CA from secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return ca, nil
}

// ensureLeaf returns a usable leaf for dnsName from the named secret,
// issuing and persisting a fresh one when the secret is missing or its
// material does not verify against the CA.
func ensureLeaf(ctx context.Context, c client.Client, namespace, name, dnsName string, ca *pki.CA) (*pki.Leaf, error) {
	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret)
	switch {
	case err == nil:
		if leaf, ok := leafFromSecret(&secret, ca, dnsName); ok {
			return leaf, nil
		}
		// Present but unusable (wrong keys, tampered, or signed by an
		// older CA): heal in place from the stored CA.
		leaf, err := ca.Issue(dnsName)
		if err != nil {
			return nil, fmt.Errorf("issue %s leaf: %w", dnsName, err)
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["tls.crt"] = leaf.CertPEM
		secret.Data["tls.key"] = leaf.KeyPEM
		if err := c.Update(ctx, &secret); err != nil {
			return nil, fmt.Errorf("heal leaf secret %s/%s: %w", namespace, name, err)
		}
		return leaf, nil

	case apierrors.IsNotFound(err):
		leaf, err := ca.Issue(dnsName)
		if err != nil {
			return nil, fmt.Errorf("issue %s leaf: %w", dnsName, err)
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.crt": leaf.CertPEM,
				"tls.key": leaf.KeyPEM,
			},
		}
		if err := c.Create(ctx, &secret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost the create race; the winner issued from the same
				// stored CA, so its material must validate.
				var existing corev1.Secret
				if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing); err != nil {
					return nil, fmt.Errorf("re-read leaf secret %s/%s after create conflict: %w", namespace, name, err)
				}
				if leaf, ok := leafFromSecret(&existing, ca, dnsName); ok {
					return leaf, nil
				}
				return nil, fmt.Errorf("leaf secret %s/%s created concurrently but does not verify against the CA", namespace, name)
			}
			return nil, fmt.Errorf("create leaf secret %s/%s: %w", namespace, name, err)
		}
		return leaf, nil

	default:
		return nil, fmt.Errorf("get leaf secret %s/%s: %w", namespace, name, err)
	}
}

// leafFromSecret returns the stored leaf when it is a coherent keypair whose
// certificate chains to the CA with the expected SAN and the exact extended
// key usage Issue grants that identity.
func leafFromSecret(secret *corev1.Secret, ca *pki.CA, dnsName string) (*pki.Leaf, bool) {
	certPEM, keyPEM := secret.Data["tls.crt"], secret.Data["tls.key"]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, false
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		return nil, false
	}
	eku := x509.ExtKeyUsageServerAuth
	if dnsName == pki.ControllerName {
		eku = x509.ExtKeyUsageClientAuth
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   dnsName,
		KeyUsages: []x509.ExtKeyUsage{eku},
	}); err != nil {
		return nil, false
	}
	return &pki.Leaf{CertPEM: certPEM, KeyPEM: keyPEM}, true
}
