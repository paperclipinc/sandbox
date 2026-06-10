package pki

import (
	"crypto/tls"
	"net"
	"testing"
)

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	ca, err := NewCA("agent-run")
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
	ca, _ := NewCA("agent-run")
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
	ca, _ := NewCA("agent-run")
	if _, err := ca.Issue("imposter.agent-run"); err == nil {
		t.Fatal("expected issuance restricted to known identities")
	}
}
