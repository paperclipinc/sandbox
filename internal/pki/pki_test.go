package pki

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	ca, err := NewCA("mitos")
	if err != nil {
		t.Fatal(err)
	}
	server, err := ca.Issue(ServerName)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ca.Issue(ControllerName)
	if err != nil {
		t.Fatal(err)
	}

	serverTLS, err := ServerTLSConfig(server.CertPEM, server.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := ClientTLSConfig(client.CertPEM, client.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}

	lis, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			done <- err
			return
		}
		done <- conn.(*tls.Conn).Handshake()
		conn.Close()
	}()

	conn, err := tls.Dial("tcp", lis.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("mTLS dial: %v", err)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	_ = net.Conn(conn)
}

func TestServerRejectsClientWithoutCert(t *testing.T) {
	ca, _ := NewCA("mitos")
	server, _ := ca.Issue(ServerName)
	serverTLS, _ := ServerTLSConfig(server.CertPEM, server.KeyPEM, ca.CertPEM())

	lis, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		_ = conn.(*tls.Conn).Handshake()
		conn.Close()
	}()

	conf := &tls.Config{InsecureSkipVerify: true} // no client cert
	conn, err := tls.Dial("tcp", lis.Addr().String(), conf)
	if err == nil {
		// In TLS 1.3 the client handshake completes before the server
		// evaluates the missing client certificate; the rejection
		// arrives as an alert on the first read.
		buf := make([]byte, 1)
		_, err = conn.Read(buf)
		conn.Close()
	}
	if err == nil {
		t.Fatal("server accepted a client without a certificate")
	}
}

func TestIssueRejectsUnknownName(t *testing.T) {
	ca, _ := NewCA("mitos")
	if _, err := ca.Issue("imposter.mitos"); err == nil {
		t.Fatal("expected issuance restricted to known identities")
	}
}

func parseLeafCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block in leaf cert")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestIssueSplitsExtKeyUsagePerIdentity(t *testing.T) {
	ca, err := NewCA("mitos")
	if err != nil {
		t.Fatal(err)
	}
	server, err := ca.Issue(ServerName)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := ca.Issue(ControllerName)
	if err != nil {
		t.Fatal(err)
	}

	serverCert := parseLeafCert(t, server.CertPEM)
	if len(serverCert.ExtKeyUsage) != 1 || serverCert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("server leaf ExtKeyUsage = %v, want exactly [ServerAuth]", serverCert.ExtKeyUsage)
	}

	controllerCert := parseLeafCert(t, controller.CertPEM)
	if len(controllerCert.ExtKeyUsage) != 1 || controllerCert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("controller leaf ExtKeyUsage = %v, want exactly [ClientAuth]", controllerCert.ExtKeyUsage)
	}
}

func TestPeerDNSNameIgnoresUnverifiedPeerCertificates(t *testing.T) {
	ca, err := NewCA("mitos")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := ca.Issue(ControllerName)
	if err != nil {
		t.Fatal(err)
	}
	leaf := parseLeafCert(t, controller.CertPEM)

	// A peer that presented a certificate which was never verified
	// against the CA must not be granted an identity.
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{leaf},
			},
		},
	})
	if name, ok := PeerDNSName(ctx); ok {
		t.Fatalf("PeerDNSName = %q from an unverified certificate, want no identity", name)
	}

	// The same certificate in VerifiedChains is an identity.
	ctx = peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{leaf},
				VerifiedChains:   [][]*x509.Certificate{{leaf}},
			},
		},
	})
	name, ok := PeerDNSName(ctx)
	if !ok || name != ControllerName {
		t.Fatalf("PeerDNSName = (%q, %v), want (%q, true)", name, ok, ControllerName)
	}
}

func TestKeyPEMSurvivesLoadCARoundTrip(t *testing.T) {
	ca, err := NewCA("mitos")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := ca.KeyPEM()
	if len(keyPEM) == 0 {
		t.Fatal("NewCA returned a CA with empty KeyPEM")
	}

	loaded, err := LoadCA(ca.CertPEM(), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.KeyPEM()) == 0 {
		t.Fatal("LoadCA returned a CA with empty KeyPEM")
	}
	if !bytes.Equal(loaded.KeyPEM(), keyPEM) {
		t.Fatal("KeyPEM changed across a LoadCA round trip")
	}
}
