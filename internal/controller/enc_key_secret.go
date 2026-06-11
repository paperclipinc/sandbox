package controller

import (
	"context"
	"crypto/rand"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// TEARDOWN BOUNDARY (PR2): the controller owns the key Secret and shreds it,
// but it does NOT today drive a forkd-side container shred on template removal.
// There is no SandboxTemplate reconciler and the pool reconciler never calls a
// DeleteTemplate RPC, so templates are not explicitly deleted on the node from
// the controller. The key Secret is owner-referenced to the SandboxTemplate, so
// Kubernetes garbage collection crypto-shreds the Secret when the template is
// deleted (and DeleteEncKey is available for an explicit shred). The remaining
// wiring, triggering the forkd container shred (engine.DeleteTemplate from PR1)
// on template GC, is deliberately out of scope here and tracked as follow-up;
// until it lands, the node-side encrypted container is reclaimed by node data
// dir lifecycle, not by a controller-driven shred.

// encKeySecretSuffix is appended to a template id to form the name of the
// Secret that holds that template's at-rest encryption key.
const encKeySecretSuffix = "-enc-key"

// encKeyDataKey is the data key inside the enc-key Secret that holds the raw
// 256-bit key bytes.
const encKeyDataKey = "key"

// encKeyLen is the at-rest encryption key length in bytes (256-bit), matching
// storecrypt's LUKS key length.
const encKeyLen = 32

// encKeySecretName returns the name of the enc-key Secret for a template.
func encKeySecretName(templateID string) string {
	return templateID + encKeySecretSuffix
}

// EnsureEncKey returns the per-template at-rest encryption key, creating the
// backing Secret with a fresh 256-bit crypto/rand key on first call and reading
// it back idempotently afterwards. The controller owns key custody: the key
// lives only in this Secret (etcd) and is delivered to the node over the mTLS
// RPC, never generated or persisted on the node. When owner is non-nil the
// Secret is owner-referenced to it so Kubernetes garbage collection
// crypto-shreds the Secret when the owner (the SandboxTemplate) is deleted. The
// key value is a secret: it is NEVER logged, never put in an error message, and
// never written to status, conditions, or events. Only the Secret name (safe)
// appears in errors.
func EnsureEncKey(ctx context.Context, c client.Client, ns, templateID string, owner client.Object) ([]byte, error) {
	name := encKeySecretName(templateID)

	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &secret)
	switch {
	case err == nil:
		key := secret.Data[encKeyDataKey]
		if len(key) == 0 {
			// Present but empty: the Secret exists without usable key material.
			// Refuse rather than silently regenerate (regenerating would make any
			// already-encrypted snapshot unopenable). The error names only the
			// Secret, never key bytes.
			return nil, fmt.Errorf("enc-key secret %s/%s exists but holds no key; fix or delete it", ns, name)
		}
		return key, nil

	case apierrors.IsNotFound(err):
		key := make([]byte, encKeyLen)
		if _, rerr := rand.Read(key); rerr != nil {
			return nil, fmt.Errorf("generate enc key for template %s: %w", templateID, rerr)
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{encKeyDataKey: key},
		}
		if owner != nil {
			if rerr := controllerutil.SetControllerReference(owner, &secret, c.Scheme()); rerr != nil {
				return nil, fmt.Errorf("set owner on enc-key secret %s/%s: %w", ns, name, rerr)
			}
		}
		if cerr := c.Create(ctx, &secret); cerr != nil {
			if apierrors.IsAlreadyExists(cerr) {
				// Lost the create race to a parallel reconcile; theirs wins. Re-read
				// to return the persisted key.
				var existing corev1.Secret
				if gerr := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &existing); gerr != nil {
					return nil, fmt.Errorf("re-read enc-key secret %s/%s after create conflict: %w", ns, name, gerr)
				}
				k := existing.Data[encKeyDataKey]
				if len(k) == 0 {
					return nil, fmt.Errorf("enc-key secret %s/%s created concurrently but holds no key", ns, name)
				}
				return k, nil
			}
			return nil, fmt.Errorf("create enc-key secret %s/%s: %w", ns, name, cerr)
		}
		return key, nil

	default:
		return nil, fmt.Errorf("get enc-key secret %s/%s: %w", ns, name, err)
	}
}

// DeleteEncKey crypto-shreds a template's at-rest encryption key by deleting its
// Secret. A missing Secret is not an error (idempotent teardown). This destroys
// the only escrowed copy of the key; the node never persists it, so once the
// Secret is gone and the node container is shredded the ciphertext is
// unrecoverable. The key value is never logged.
func DeleteEncKey(ctx context.Context, c client.Client, ns, templateID string) error {
	name := encKeySecretName(templateID)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := c.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete enc-key secret %s/%s: %w", ns, name, err)
	}
	return nil
}
