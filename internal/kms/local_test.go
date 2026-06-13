package kms

import (
	"bytes"
	"context"
	"crypto/rand"
	"strings"
	"testing"
)

func TestLocalKEKWrapUnwrapRoundTrip(t *testing.T) {
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand: %v", err)
	}
	w, err := NewLocalKEK(kek)
	if err != nil {
		t.Fatalf("NewLocalKEK: %v", err)
	}
	dek := []byte("0123456789abcdef0123456789abcdef") // 32-byte DEK
	wrapped, err := w.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if bytes.Contains(wrapped.Ciphertext, dek) {
		t.Fatal("wrapped DEK contains the plaintext DEK")
	}
	if wrapped.KEKID != w.KEKID() {
		t.Fatalf("KEKID mismatch: %q vs %q", wrapped.KEKID, w.KEKID())
	}
	got, err := w.Unwrap(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("round-trip DEK mismatch")
	}
}

func TestLocalKEKWrapUsesDistinctNonces(t *testing.T) {
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand: %v", err)
	}
	w, err := NewLocalKEK(kek)
	if err != nil {
		t.Fatalf("NewLocalKEK: %v", err)
	}
	dek := []byte("0123456789abcdef0123456789abcdef")
	// Wrapping the same DEK twice MUST yield different ciphertexts: a fresh
	// random GCM nonce per Wrap is mandatory (nonce reuse under one key breaks
	// GCM confidentiality and integrity). The nonce is prefixed to the
	// ciphertext, so distinct output proves distinct nonces.
	a, err := w.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap a: %v", err)
	}
	b, err := w.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap b: %v", err)
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("two Wrap calls produced identical ciphertext: nonce reuse")
	}
}

func TestLocalKEKRejectsWrongKEKLength(t *testing.T) {
	if _, err := NewLocalKEK(make([]byte, 16)); err == nil {
		t.Fatal("expected error for 16-byte KEK")
	}
}

func TestLocalKEKUnwrapTamperFails(t *testing.T) {
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	w, _ := NewLocalKEK(kek)
	wrapped, _ := w.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef"))
	wrapped.Ciphertext[len(wrapped.Ciphertext)-1] ^= 0xff
	if _, err := w.Unwrap(context.Background(), wrapped); err == nil {
		t.Fatal("expected GCM auth failure on tampered ciphertext")
	}
}

func TestLocalKEKIDIsStableAndNonSecret(t *testing.T) {
	kek := bytes.Repeat([]byte{7}, 32)
	w, _ := NewLocalKEK(kek)
	id := w.KEKID()
	if !strings.HasPrefix(id, "local:") {
		t.Fatalf("KEKID %q missing local: prefix", id)
	}
	if strings.Contains(id, string(kek)) {
		t.Fatal("KEKID leaks KEK bytes")
	}
	w2, _ := NewLocalKEK(kek)
	if w2.KEKID() != id {
		t.Fatal("KEKID not stable for the same KEK")
	}
}
