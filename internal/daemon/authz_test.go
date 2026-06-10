package daemon

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/paperclipinc/sandbox/internal/fork"
	"github.com/paperclipinc/sandbox/internal/pki"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
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
	ca, err := pki.NewCA("agent-run")
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

func TestAuthzServerIdentityAsClientDenied(t *testing.T) {
	ca, addr := newAuthzPKI(t)
	// pki.Issue only issues the two known names; the server name leaf
	// stands in for any valid-but-wrong identity presented as a client
	// certificate.
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
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v (err = %v), want PermissionDenied", status.Code(err), err)
	}
}
