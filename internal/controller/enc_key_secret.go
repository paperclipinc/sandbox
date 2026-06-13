package controller

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/paperclipinc/mitos/internal/kms"
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

// encKeyWrappedDataKey is the data key inside the enc-key Secret that holds the
// WRAPPED DEK (the per-template DEK encrypted under the KMS KEK). It is opaque
// ciphertext, never the plaintext key.
const encKeyWrappedDataKey = "wrapped-dek"

// encKeyKEKIDDataKey is the data key inside the enc-key Secret that holds the
// non-secret KEK id that wrapped the DEK, so the node can select the matching
// KEK to unwrap.
const encKeyKEKIDDataKey = "kek-id"

// encKeyLen is the at-rest DEK length in bytes (256-bit), matching storecrypt's
// LUKS key length.
const encKeyLen = 32

// encKeySecretName returns the name of the enc-key Secret for a template.
func encKeySecretName(templateID string) string {
	return templateID + encKeySecretSuffix
}

// EnsureEncKey returns the per-template at-rest WRAPPED DEK plus its KEK id,
// using envelope encryption. On first call it generates a fresh 256-bit DEK with
// crypto/rand, wraps it with the KMS KEK (w), zeroizes the plaintext DEK
// immediately, and persists ONLY the wrapped DEK and the (non-secret) KEK id in
// the backing Secret; subsequent calls read the wrapped DEK back idempotently.
// The plaintext DEK NEVER persists to etcd or disk and is held in controller
// memory only for the duration of the wrap. The controller never unwraps; it
// never sees the plaintext DEK after this returns. When owner is non-nil the
// Secret is owner-referenced to it so Kubernetes garbage collection
// crypto-shreds the Secret (the only stored copy of the wrapped DEK) when the
// owner is deleted. The plaintext DEK and the wrapped DEK are NEVER logged or
// put in an error message; only the Secret name and the KEK id (both non-secret)
// appear in errors.
func EnsureEncKey(ctx context.Context, c client.Client, w kms.Wrapper, ns, templateID string, owner client.Object) (wrappedDEK []byte, kekID string, err error) {
	name := encKeySecretName(templateID)

	var secret corev1.Secret
	gerr := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &secret)
	switch {
	case gerr == nil:
		wrapped := secret.Data[encKeyWrappedDataKey]
		if len(wrapped) == 0 {
			// Present but empty: the Secret exists without usable wrapped DEK.
			// Refuse rather than silently regenerate (regenerating would make any
			// already-encrypted snapshot unopenable). The error names only the
			// Secret, never key bytes.
			return nil, "", fmt.Errorf("enc-key secret %s/%s exists but holds no wrapped DEK; fix or delete it", ns, name)
		}
		return wrapped, string(secret.Data[encKeyKEKIDDataKey]), nil

	case apierrors.IsNotFound(gerr):
		if w == nil {
			return nil, "", fmt.Errorf("cannot create enc-key secret %s/%s: no KMS configured to wrap the DEK; set the controller --kek-file", ns, name)
		}
		dek := make([]byte, encKeyLen)
		if _, rerr := rand.Read(dek); rerr != nil {
			return nil, "", fmt.Errorf("generate DEK for template %s: %w", templateID, rerr)
		}
		wrapped, werr := w.Wrap(ctx, dek)
		// Zeroize the plaintext DEK immediately after wrapping: it must not linger
		// in controller memory and never reaches etcd or disk.
		for i := range dek {
			dek[i] = 0
		}
		if werr != nil {
			return nil, "", fmt.Errorf("wrap DEK for template %s (kek %s): %w", templateID, w.KEKID(), werr)
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Type:       corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				encKeyWrappedDataKey: wrapped.Ciphertext,
				encKeyKEKIDDataKey:   []byte(wrapped.KEKID),
			},
		}
		if owner != nil {
			if rerr := controllerutil.SetControllerReference(owner, &secret, c.Scheme()); rerr != nil {
				return nil, "", fmt.Errorf("set owner on enc-key secret %s/%s: %w", ns, name, rerr)
			}
		}
		if cerr := c.Create(ctx, &secret); cerr != nil {
			if apierrors.IsAlreadyExists(cerr) {
				// Lost the create race to a parallel reconcile; theirs wins. Re-read
				// to return the persisted wrapped DEK.
				var existing corev1.Secret
				if rgerr := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &existing); rgerr != nil {
					return nil, "", fmt.Errorf("re-read enc-key secret %s/%s after create conflict: %w", ns, name, rgerr)
				}
				ew := existing.Data[encKeyWrappedDataKey]
				if len(ew) == 0 {
					return nil, "", fmt.Errorf("enc-key secret %s/%s created concurrently but holds no wrapped DEK", ns, name)
				}
				return ew, string(existing.Data[encKeyKEKIDDataKey]), nil
			}
			return nil, "", fmt.Errorf("create enc-key secret %s/%s: %w", ns, name, cerr)
		}
		return wrapped.Ciphertext, wrapped.KEKID, nil

	default:
		return nil, "", fmt.Errorf("get enc-key secret %s/%s: %w", ns, name, gerr)
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
