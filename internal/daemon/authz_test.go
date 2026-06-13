package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/pki"
	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// newAuthzPKI builds a CA plus the forkd server leaf and returns the
// CA and a running TLS gRPC server address with the identity
// interceptor installed.
func newAuthzPKI(t *testing.T) (*pki.CA, string) {
	t.Helper()
	ca, err := pki.NewCA("mitos")
	if err != nil {
		t.Fatal(err)
	}
	serverLeaf, err := ca.Issue(pki.ServerName)
	if err != nil {
		t.Fatal(err)
	}
	serverCfg, err := pki.ServerTLSConfig(serverLeaf.CertPEM, serverLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	return ca, startTLSServer(t, serverCfg)
}

func startTLSServer(t *testing.T, serverCfg *tls.Config) string {
	t.Helper()
	engine := fork.NewMockEngine()
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(serverCfg)),
		grpc.UnaryInterceptor(RequireControllerIdentity),
		grpc.StreamInterceptor(RequireControllerIdentityStream),
	)
	RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis) //nolint:errcheck
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func dialForkd(t *testing.T, addr string, creds credentials.TransportCredentials) forkdpb.ForkDaemonClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return forkdpb.NewForkDaemonClient(conn)
}

func TestAuthzControllerIdentityAllowed(t *testing.T) {
	ca, addr := newAuthzPKI(t)
	controllerLeaf, err := ca.Issue(pki.ControllerName)
	if err != nil {
		t.Fatal(err)
	}
	clientCfg, err := pki.ClientTLSConfig(controllerLeaf.CertPEM, controllerLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	client := dialForkd(t, addr, credentials.NewTLS(clientCfg))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.GetCapacity(ctx, &forkdpb.GetCapacityRequest{}); err != nil {
		t.Fatalf("GetCapacity with controller identity: %v", err)
	}
}

func TestAuthzPlainClientRejected(t *testing.T) {
	_, addr := newAuthzPKI(t)
	client := dialForkd(t, addr, insecure.NewCredentials())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.GetCapacity(ctx, &forkdpb.GetCapacityRequest{})
	if err == nil {
		t.Fatal("plaintext client reached forkd through a TLS-only server")
	}
}

func TestServerLeafCannotActAsClient(t *testing.T) {
	ca, addr := newAuthzPKI(t)
	// The forkd server leaf carries only the ServerAuth EKU; presented
	// as a client certificate it must fail the TLS handshake itself,
	// before any interceptor runs.
	serverLeaf, err := ca.Issue(pki.ServerName)
	if err != nil {
		t.Fatal(err)
	}
	clientCfg, err := pki.ClientTLSConfig(serverLeaf.CertPEM, serverLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	client := dialForkd(t, addr, credentials.NewTLS(clientCfg))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = client.GetCapacity(ctx, &forkdpb.GetCapacityRequest{})
	if err == nil {
		t.Fatal("server leaf was accepted as a client certificate")
	}
	if status.Code(err) == codes.PermissionDenied {
		t.Fatalf("err = %v; want a transport-level handshake failure, not PermissionDenied from the interceptor", err)
	}
}

// issueImposterLeaf signs a leaf with ClientAuth EKU and a DNS SAN
// outside the two control plane identities, directly against the CA
// key. This deliberately bypasses Issue()'s name restriction to
// simulate a mis-issued or stolen-CA-signed certificate.
func issueImposterLeaf(t *testing.T, ca *pki.CA) (certPEM, keyPEM []byte) {
	t.Helper()
	caCertBlock, _ := pem.Decode(ca.CertPEM())
	if caCertBlock == nil {
		t.Fatal("no PEM block in CA cert")
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKeyBlock, _ := pem.Decode(ca.KeyPEM())
	if caKeyBlock == nil {
		t.Fatal("no PEM block in CA key")
	}
	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "imposter.mitos"},
		DNSNames:              []string{"imposter.mitos"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestWrongIdentityClientCertIsDenied(t *testing.T) {
	ca, addr := newAuthzPKI(t)
	certPEM, keyPEM := issueImposterLeaf(t, ca)
	clientCfg, err := pki.ClientTLSConfig(certPEM, keyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	client := dialForkd(t, addr, credentials.NewTLS(clientCfg))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Unary path: RequireControllerIdentity.
	_, err = client.GetCapacity(ctx, &forkdpb.GetCapacityRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unary code = %v (err = %v), want PermissionDenied", status.Code(err), err)
	}

	// Streaming path: RequireControllerIdentityStream. The denial must
	// come from the interceptor, before the handler can answer.
	stream, err := client.ExecStream(ctx, &forkdpb.ExecStreamRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stream code = %v (err = %v), want PermissionDenied", status.Code(err), err)
	}
}
