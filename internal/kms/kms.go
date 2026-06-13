// Package kms provides envelope encryption for the at-rest data-encryption key
// (DEK). A DEK is wrapped (encrypted) by a key-encryption key (KEK) that lives
// behind a Wrapper. Only the WRAPPED DEK is persisted (in a Kubernetes Secret)
// and carried over the wire; the plaintext DEK exists in memory only while a
// cryptsetup operation needs it and is zeroized after. The KEK never leaves the
// Wrapper boundary: the local provider holds it in process memory; a future
// cloud KMS provider keeps it in the HSM and performs wrap/unwrap remotely.
//
// Secret discipline: the plaintext DEK and the KEK are secret VALUES. They are
// never logged, never placed in an error message, and never written to a host
// path. The KEK id (KEKID) is NOT secret: it is a stable, non-reversible label
// used to match a wrapped DEK to the KEK that produced it and is safe to log.
package kms

import "context"

// WrappedKey is a DEK encrypted under a KEK. KEKID identifies the KEK that
// produced it (safe to log); Ciphertext is the opaque wrapped DEK (never
// logged, even though it is not the plaintext).
type WrappedKey struct {
	KEKID      string
	Ciphertext []byte
}

// Wrapper performs envelope wrap/unwrap of a DEK under a KEK. Implementations
// must be safe for concurrent use. Wrap and Unwrap take a context so a cloud
// KMS provider can bound and cancel its remote call; the local provider ignores
// it. Errors should carry actionable remediation text (the API v2 LLM-legible
// error rule) and must never include the plaintext DEK or the KEK.
type Wrapper interface {
	Wrap(ctx context.Context, plaintextDEK []byte) (WrappedKey, error)
	Unwrap(ctx context.Context, w WrappedKey) ([]byte, error)
	KEKID() string
}
