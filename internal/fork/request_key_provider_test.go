package fork

import (
	"bytes"
	"context"
	"testing"

	"github.com/paperclipinc/mitos/internal/kms"
)

func TestRequestKeyProviderUnwrapsViaKMS(t *testing.T) {
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	w, err := kms.NewLocalKEK(kek)
	if err != nil {
		t.Fatalf("NewLocalKEK: %v", err)
	}
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i)
	}
	wrapped, err := w.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	p := NewRequestKeyProvider(w)
	p.SetWrappedKey("tmpl", wrapped.Ciphertext, wrapped.KEKID)
	got, err := p.KeyFor("tmpl")
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("unwrapped DEK mismatch")
	}
	p.ForgetKey("tmpl")
	if _, err := p.KeyFor("tmpl"); err == nil {
		t.Fatal("expected fail-closed after ForgetKey")
	}
}

func TestRequestKeyProviderFailsClosedWithNoWrappedKey(t *testing.T) {
	w, _ := kms.NewLocalKEK(make([]byte, 32))
	p := NewRequestKeyProvider(w)
	if _, err := p.KeyFor("missing"); err == nil {
		t.Fatal("expected error when no wrapped key is stashed")
	}
}

func TestRequestKeyProviderRejectsWrongKEK(t *testing.T) {
	kekA := make([]byte, 32)
	kekB := make([]byte, 32)
	for i := range kekB {
		kekB[i] = 0xaa
	}
	wa, _ := kms.NewLocalKEK(kekA)
	wb, _ := kms.NewLocalKEK(kekB)
	wrapped, _ := wa.Wrap(context.Background(), make([]byte, 32))
	p := NewRequestKeyProvider(wb) // node holds the wrong KEK
	p.SetWrappedKey("tmpl", wrapped.Ciphertext, wrapped.KEKID)
	if _, err := p.KeyFor("tmpl"); err == nil {
		t.Fatal("expected unwrap error for KEK mismatch")
	}
}
