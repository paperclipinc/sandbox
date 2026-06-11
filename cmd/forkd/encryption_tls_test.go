package main

import (
	"strings"
	"testing"
)

// TestRequireTLSForEncryption proves the fail-closed guard: at-rest encryption
// may only run over an mTLS gRPC channel because the controller delivers the
// per-template key over the request. Encryption enabled without TLS is fatal;
// every other combination is allowed.
func TestRequireTLSForEncryption(t *testing.T) {
	cases := []struct {
		name          string
		enableEnc     bool
		tlsConfigured bool
		wantErr       bool
	}{
		{name: "encryption without TLS is fatal", enableEnc: true, tlsConfigured: false, wantErr: true},
		{name: "encryption with TLS is allowed", enableEnc: true, tlsConfigured: true, wantErr: false},
		{name: "no encryption without TLS is allowed", enableEnc: false, tlsConfigured: false, wantErr: false},
		{name: "no encryption with TLS is allowed", enableEnc: false, tlsConfigured: true, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireTLSForEncryption(tc.enableEnc, tc.tlsConfigured)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a fatal error, got nil")
				}
				// The error must point the operator at the mTLS remediation.
				if !strings.Contains(err.Error(), "mTLS") {
					t.Fatalf("error should mention mTLS remediation, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}
