package kms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// kekLen is the local KEK length in bytes (AES-256).
const kekLen = 32

// LocalKEK wraps a DEK with AES-256-GCM under a process-held KEK. It is the
// dev/CI provider: the KEK comes from a Kubernetes Secret mounted as a file,
// not from a cloud HSM. It is safe for concurrent use (the cipher.AEAD is
// stateless and a fresh nonce is drawn per Wrap).
type LocalKEK struct {
	aead  cipher.AEAD
	kekID string
}

// NewLocalKEK builds a provider from a 32-byte AES-256 KEK. The KEK is a secret
// value: it is never logged or placed in an error. The returned KEKID is a
// non-reversible fingerprint, safe to log.
func NewLocalKEK(kek []byte) (*LocalKEK, error) {
	if len(kek) != kekLen {
		return nil, fmt.Errorf("local KEK must be %d bytes (AES-256); got %d: supply a 32-byte key via --kek-file", kekLen, len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("init AES cipher for local KEK: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init GCM for local KEK: %w", err)
	}
	sum := sha256.Sum256(kek)
	return &LocalKEK{aead: aead, kekID: "local:" + hex.EncodeToString(sum[:8])}, nil
}

// LoadLocalKEKFromFile reads a 32-byte KEK from path. The file must hold exactly
// 32 raw bytes (e.g. a Kubernetes Secret key mounted as a file). The bytes are a
// secret value: this never logs them and zeroizes the read buffer after use.
func LoadLocalKEKFromFile(path string) (*LocalKEK, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied KEK file
	if err != nil {
		return nil, fmt.Errorf("read KEK file %s: %w", path, err)
	}
	w, werr := NewLocalKEK(b)
	for i := range b {
		b[i] = 0
	}
	if werr != nil {
		return nil, werr
	}
	return w, nil
}

// Wrap encrypts the plaintext DEK with AES-256-GCM under the KEK. The output is
// nonce || ciphertext+tag. A fresh random nonce is drawn per call. The plaintext
// DEK is never logged or placed in an error.
func (l *LocalKEK) Wrap(_ context.Context, plaintextDEK []byte) (WrappedKey, error) {
	nonce := make([]byte, l.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return WrappedKey{}, fmt.Errorf("draw GCM nonce: %w", err)
	}
	ct := l.aead.Seal(nonce, nonce, plaintextDEK, nil)
	return WrappedKey{KEKID: l.kekID, Ciphertext: ct}, nil
}

// Unwrap decrypts a wrapped DEK. It returns an error (never the plaintext) on a
// KEK mismatch, a short ciphertext, or a GCM authentication failure.
func (l *LocalKEK) Unwrap(_ context.Context, w WrappedKey) ([]byte, error) {
	if w.KEKID != l.kekID {
		return nil, fmt.Errorf("wrapped DEK was produced by KEK %q but this node holds KEK %q: deliver the matching KEK (check --kek-file) or rewrap the DEK", w.KEKID, l.kekID)
	}
	ns := l.aead.NonceSize()
	if len(w.Ciphertext) < ns {
		return nil, fmt.Errorf("wrapped DEK too short: %d bytes, want at least %d", len(w.Ciphertext), ns)
	}
	nonce, ct := w.Ciphertext[:ns], w.Ciphertext[ns:]
	dek, err := l.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK: GCM authentication failed (wrong KEK or corrupted wrapped DEK): %w", err)
	}
	return dek, nil
}

// KEKID returns the non-secret KEK fingerprint, safe to log.
func (l *LocalKEK) KEKID() string { return l.kekID }
