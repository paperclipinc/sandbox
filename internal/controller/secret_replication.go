package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReplicateHuskSecrets copies the control plane PKI material husk pods mount
// from the controller namespace (src) into a pool namespace (dst). Husk pods
// run in the pool namespace (e.g. default), not the controller namespace, but
// EnsurePKI only materializes mitos-ca and mitos-forkd-tls in the controller
// namespace; this bridges that gap so `kubectl apply -k deploy/` needs no
// manual secret copy.
//
// The CA copy projects ONLY ca.crt: the CA private key (ca.key) must never
// leave the controller namespace, matching the husk pod's CA mount in
// huskpod.go (Items ca.crt only). The forkd leaf (tls.crt, tls.key) is copied
// whole because the husk stub serves the mTLS control with it.
//
// Replication is idempotent and heals drift: a destination copy whose data
// differs from the source is updated in place (so a CA rotation propagates).
// Copying into the source namespace itself is a noop (the originals are there).
func ReplicateHuskSecrets(ctx context.Context, c client.Client, src, dst string) error {
	if src == dst {
		return nil
	}
	if err := replicateControlPlaneSecret(ctx, c, src, dst, CASecretName, []string{"ca.crt"}); err != nil {
		return err
	}
	if err := replicateControlPlaneSecret(ctx, c, src, dst, ForkdTLSSecretName, []string{"tls.crt", "tls.key"}); err != nil {
		return err
	}
	return nil
}

// replicateControlPlaneSecret copies exactly the named keys of secret `name`
// from src to dst, creating the destination when absent and updating it when
// its projected data drifts. Keys not in `keys` are never copied (so the CA
// private key cannot leak). A missing source secret is an error: the caller
// runs this only after EnsurePKI has materialized the originals.
func replicateControlPlaneSecret(ctx context.Context, c client.Client, src, dst, name string, keys []string) error {
	var source corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: src, Name: name}, &source); err != nil {
		return fmt.Errorf("read source secret %s/%s for replication: %w", src, name, err)
	}

	projected := make(map[string][]byte, len(keys))
	for _, k := range keys {
		v, ok := source.Data[k]
		if !ok {
			return fmt.Errorf("source secret %s/%s lacks key %s; cannot replicate", src, name, k)
		}
		projected[k] = append([]byte(nil), v...)
	}

	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: dst, Name: name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		copySecret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: dst, Name: name},
			Type:       source.Type,
			Data:       projected,
		}
		if err := c.Create(ctx, &copySecret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost the create race to a parallel reconcile; the winner wrote
				// the same projected data from the same source, so treat as done.
				return nil
			}
			return fmt.Errorf("create replicated secret %s/%s: %w", dst, name, err)
		}
		return nil

	case err != nil:
		return fmt.Errorf("read destination secret %s/%s: %w", dst, name, err)

	default:
		if secretDataEqual(existing.Data, projected) {
			return nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		// Overwrite exactly the projected keys; leave any unrelated keys the
		// destination may carry untouched.
		for k, v := range projected {
			existing.Data[k] = v
		}
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("heal replicated secret %s/%s: %w", dst, name, err)
		}
		return nil
	}
}

// secretDataEqual reports whether dst already contains every key/value in want.
// It does not require dst to be a strict equal (dst may carry extra keys); it
// only checks the projected keys match, which is the replication contract.
func secretDataEqual(dst, want map[string][]byte) bool {
	for k, v := range want {
		got, ok := dst[k]
		if !ok || string(got) != string(v) {
			return false
		}
	}
	return true
}
