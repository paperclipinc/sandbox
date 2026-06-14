package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTokenFromFileTrimsNewline(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "token")
	if err := os.WriteFile(f, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadToken(f)
	if err != nil {
		t.Fatalf("loadToken: %v", err)
	}
	if got != "secret-token" {
		t.Fatalf("loadToken = %q, want secret-token", got)
	}
}

func TestLoadTokenFromEnv(t *testing.T) {
	t.Setenv("RENDEZVOUS_TOKEN", "env-secret")
	got, err := loadToken("")
	if err != nil {
		t.Fatalf("loadToken: %v", err)
	}
	if got != "env-secret" {
		t.Fatalf("loadToken = %q, want env-secret", got)
	}
}

func TestLoadTokenMissingErrorHasNoToken(t *testing.T) {
	t.Setenv("RENDEZVOUS_TOKEN", "")
	_, err := loadToken("")
	if err == nil {
		t.Fatal("loadToken with no source must error")
	}
	if strings.Contains(err.Error(), "env-secret") {
		t.Fatalf("error should not echo any token: %v", err)
	}
}
