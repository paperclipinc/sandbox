package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// tokenSecretSuffix is appended to the claim or fork sandbox name to form
// the name of the Secret carrying the sandbox API bearer token.
const tokenSecretSuffix = "-sandbox-token"

// mintAPIToken returns 32 bytes of crypto/rand entropy hex-encoded
// (64 chars). The value is a bearer credential: never log it and never put
// it in status, conditions, or events.
func mintAPIToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint api token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ensureSandboxTokenSecret creates or updates the Secret that hands the
// sandbox API bearer token to the sandbox's consumer. The Secret is
// controller-owned by owner (a SandboxClaim or SandboxFork), so it is
// garbage collected with it. Keys: token (the bearer value) and endpoint
// (the HTTP sandbox API address). The token value goes nowhere else.
func ensureSandboxTokenSecret(ctx context.Context, c client.Client, owner client.Object, name, token, endpoint string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.GetNamespace()},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		secret.Data = map[string][]byte{
			"token":    []byte(token),
			"endpoint": []byte(endpoint),
		}
		return controllerutil.SetControllerReference(owner, secret, c.Scheme())
	})
	if err != nil {
		// The wrapped error never carries the token value; secret names are
		// safe to log.
		return fmt.Errorf("ensure token secret %s/%s: %w", owner.GetNamespace(), name, err)
	}
	return nil
}
