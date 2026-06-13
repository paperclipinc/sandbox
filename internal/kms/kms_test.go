package kms

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

var _ Wrapper = (*LocalKEK)(nil)

func TestLoadLocalKEKFromFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kek")
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	if err := os.WriteFile(path, kek, 0o600); err != nil {
		t.Fatalf("write kek: %v", err)
	}
	w, err := LoadLocalKEKFromFile(path)
	if err != nil {
		t.Fatalf("LoadLocalKEKFromFile: %v", err)
	}
	wrapped, err := w.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if _, err := w.Unwrap(context.Background(), wrapped); err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
}

func TestLoadLocalKEKFromFileWrongLength(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kek")
	if err := os.WriteFile(path, make([]byte, 10), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadLocalKEKFromFile(path); err == nil {
		t.Fatal("expected error for 10-byte KEK file")
	}
}
