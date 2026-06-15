package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/pki"
)

// TestGRPCServerOptionsFailClosed proves the gRPC control surface fails CLOSED
// when no TLS is configured: with the TLS flags absent and --allow-insecure-grpc
// off, grpcServerOptions refuses (returns an error) rather than serving an
// unauthenticated control surface. The insecure path is reachable only with the
// explicit opt-in flag, and a full TLS triple yields a secure server.
func TestGRPCServerOptionsFailClosed(t *testing.T) {
	cert, key, ca := writeTestTLSTriple(t)

	cases := []struct {
		name          string
		cert          string
		key           string
		caPath        string
		allowInsecure bool
		wantErr       bool
		errSubstr     string
	}{
		{
			name:    "no TLS, no opt-in refuses",
			wantErr: true,
			// The error must name the missing TLS flags and the opt-in escape hatch.
			errSubstr: "--allow-insecure-grpc",
		},
		{
			name:          "no TLS with explicit opt-in is allowed (dev)",
			allowInsecure: true,
			wantErr:       false,
		},
		{
			name:    "full TLS triple is allowed",
			cert:    cert,
			key:     key,
			caPath:  ca,
			wantErr: false,
		},
		{
			name:    "partial TLS is a config error even with opt-in",
			cert:    cert,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := grpcServerOptions(tc.cert, tc.key, tc.caPath, tc.allowInsecure)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error (fail closed), got nil")
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error %q should mention %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

// writeTestTLSTriple mints a CA and a forkd server leaf, writing the cert, key,
// and CA PEM to temp files and returning their paths.
func writeTestTLSTriple(t *testing.T) (certPath, keyPath, caPath string) {
	t.Helper()
	ca, err := pki.NewCA("mitos-test")
	if err != nil {
		t.Fatalf("new CA: %v", err)
	}
	leaf, err := ca.Issue("forkd.mitos")
	if err != nil {
		t.Fatalf("issue leaf: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	caPath = filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(certPath, leaf.CertPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, leaf.KeyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(caPath, ca.CertPEM(), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return certPath, keyPath, caPath
}
