package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paperclipinc/sandbox/internal/husk"
	"github.com/paperclipinc/sandbox/internal/pki"
)

// huskClientPKI issues the husk server (forkd identity) and controller client
// leaves from a fresh CA.
type huskClientPKI struct {
	caPEM      []byte
	serverConf *tls.Config
	clientConf *tls.Config
}

func newHuskClientPKI(t *testing.T) *huskClientPKI {
	t.Helper()
	ca, err := pki.NewCA("husk-client-test")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	serverLeaf, err := ca.Issue(pki.ServerName)
	if err != nil {
		t.Fatalf("issue server leaf: %v", err)
	}
	ctrlLeaf, err := ca.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue controller leaf: %v", err)
	}
	serverConf, err := pki.ServerTLSConfig(serverLeaf.CertPEM, serverLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatalf("server TLS config: %v", err)
	}
	clientConf, err := pki.ClientTLSConfig(ctrlLeaf.CertPEM, ctrlLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatalf("client TLS config: %v", err)
	}
	return &huskClientPKI{caPEM: ca.CertPEM(), serverConf: serverConf, clientConf: clientConf}
}

func huskCertPool(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("append CA to pool")
	}
	return pool
}

// fakeHuskServer stands up an mTLS listener speaking the husk JSON control
// protocol directly (ReadRequest then WriteResult), so ActivateHuskPod's
// transport and auth are exercised without depending on the husk Stub's
// unexported VMM seam. It authorizes the verified peer with the same controller
// identity check the real husk.ServeTLS uses, and records the request so the
// test can assert the secret arrived over the wire.
type fakeHuskServer struct {
	addr string
	stop func()

	mu        sync.Mutex
	calls     int
	gotSecret string
}

func newFakeHuskServer(t *testing.T, p *huskClientPKI) *fakeHuskServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, p.serverConf)
	s := &fakeHuskServer{addr: ln.Addr().String()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, aerr := tlsLn.Accept()
			if aerr != nil {
				return
			}
			s.handle(conn)
		}
	}()
	s.stop = func() {
		_ = tlsLn.Close()
		<-done
	}
	return s
}

func (s *fakeHuskServer) handle(conn net.Conn) {
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return
	}
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		return
	}
	state := tlsConn.ConnectionState()
	if err := husk.AuthorizeControllerIdentity(&state); err != nil {
		return
	}
	req, err := husk.ReadRequest(conn)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.calls++
	s.gotSecret = req.Secrets["API_KEY"]
	s.mu.Unlock()
	_ = husk.WriteResult(conn, husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock", LatencyMs: 1.5})
}

func TestActivateHuskPodRoundTrip(t *testing.T) {
	p := newHuskClientPKI(t)
	s := newFakeHuskServer(t, p)
	defer s.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := ActivateHuskPod(ctx, s.addr, p.clientConf, husk.ActivateRequest{
		SnapshotDir: "/data/templates/tmpl-a/snapshot",
		Secrets:     map[string]string{"API_KEY": "s3cr3t-value"},
	})
	if err != nil {
		t.Fatalf("ActivateHuskPod: %v", err)
	}
	if !res.OK || res.VsockPath != "/run/husk/vsock.sock" {
		t.Fatalf("unexpected result: %+v", res)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gotSecret != "s3cr3t-value" {
		t.Fatalf("husk did not receive the secret over mTLS")
	}
}

func TestActivateHuskPodRefusesWithoutTLS(t *testing.T) {
	// A nil TLS config must be refused so secrets never ride an unauthenticated
	// channel; no dial is attempted.
	_, err := ActivateHuskPod(context.Background(), "127.0.0.1:1", nil, husk.ActivateRequest{
		Secrets: map[string]string{"API_KEY": "leak-me"},
	})
	if err == nil {
		t.Fatalf("ActivateHuskPod must refuse a nil TLS config")
	}
}

func TestActivateHuskPodRejectedByServerWithWrongCA(t *testing.T) {
	p := newHuskClientPKI(t)
	s := newFakeHuskServer(t, p)
	defer s.stop()

	// Client cert from a different CA: the husk server's ClientCAs pool does not
	// trust it, so the handshake fails and the activate never reaches the server.
	otherCA, err := pki.NewCA("attacker")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	rogueLeaf, err := otherCA.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue rogue leaf: %v", err)
	}
	rogueCert, err := tls.X509KeyPair(rogueLeaf.CertPEM, rogueLeaf.KeyPEM)
	if err != nil {
		t.Fatalf("rogue keypair: %v", err)
	}
	rogueConf := &tls.Config{
		Certificates: []tls.Certificate{rogueCert},
		RootCAs:      huskCertPool(t, p.caPEM),
		ServerName:   pki.ServerName,
		MinVersion:   tls.VersionTLS13,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = ActivateHuskPod(ctx, s.addr, rogueConf, husk.ActivateRequest{
		SnapshotDir: "/data/snap",
		Secrets:     map[string]string{"API_KEY": "leak-me"},
	})
	if err == nil {
		t.Fatalf("expected ActivateHuskPod to fail against an untrusted client cert")
	}
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls != 0 {
		t.Fatalf("husk processed an unauthenticated activate (%d calls)", s.calls)
	}
}

func TestActivateHuskPodNoSecretInLogs(t *testing.T) {
	p := newHuskClientPKI(t)
	s := newFakeHuskServer(t, p)
	defer s.stop()

	logged := captureControllerStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		const secret = "CLIENT-TOPSECRET"
		if _, err := ActivateHuskPod(ctx, s.addr, p.clientConf, husk.ActivateRequest{
			SnapshotDir: "/data/snap",
			Secrets:     map[string]string{"API_KEY": secret},
		}); err != nil {
			t.Fatalf("ActivateHuskPod: %v", err)
		}
	})
	if strings.Contains(logged, "CLIENT-TOPSECRET") {
		t.Fatalf("secret value leaked into logs:\n%s", logged)
	}
}

// captureControllerStderr redirects os.Stderr around fn and returns everything
// written.
func captureControllerStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	var buf bytes.Buffer
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tmp := make([]byte, 4096)
		for {
			nr, rerr := r.Read(tmp)
			if nr > 0 {
				mu.Lock()
				buf.Write(tmp[:nr])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()

	fn()

	os.Stderr = orig
	_ = w.Close()
	wg.Wait()
	_ = r.Close()
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}
