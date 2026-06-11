package husk

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paperclipinc/sandbox/internal/pki"
)

// netTestPKI issues the husk server (forkd identity) and controller client
// leaves from a fresh CA, returning the server TLS config the husk control
// listener uses and the CA PEM the clients trust.
type netTestPKI struct {
	caPEM      []byte
	serverConf *tls.Config
	ctrlCert   tls.Certificate
}

func newNetTestPKI(t *testing.T) *netTestPKI {
	t.Helper()
	ca, err := pki.NewCA("husk-test")
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
	ctrlCert, err := tls.X509KeyPair(ctrlLeaf.CertPEM, ctrlLeaf.KeyPEM)
	if err != nil {
		t.Fatalf("controller keypair: %v", err)
	}
	return &netTestPKI{caPEM: ca.CertPEM(), serverConf: serverConf, ctrlCert: ctrlCert}
}

func certPool(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("append CA to pool")
	}
	return pool
}

// clientConf builds a client TLS config trusting the CA and pinning the husk
// server identity, presenting the given client certificate.
func (p *netTestPKI) clientConf(t *testing.T, cert tls.Certificate) *tls.Config {
	t.Helper()
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      certPool(t, p.caPEM),
		ServerName:   pki.ServerName,
		MinVersion:   tls.VersionTLS13,
	}
}

// startServer serves ServeTLS on a fresh loopback listener and returns the dial
// address plus a stop function that cancels and waits for the server.
func startServer(t *testing.T, stub *Stub, p *netTestPKI) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ServeTLS(ctx, ln, stub, p.serverConf, AuthorizeControllerIdentity)
	}()
	return ln.Addr().String(), func() {
		cancel()
		<-done
	}
}

// activateClient runs the controller-side Activate exchange directly (mirroring
// internal/controller.ActivateHuskPod, kept here so the husk test does not
// import the controller). It dials over mTLS, sends the request, reads the
// result.
func activateClient(t *testing.T, addr string, clientConf *tls.Config, req ActivateRequest) (ActivateResult, error) {
	t.Helper()
	dialer := &tls.Dialer{Config: clientConf}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return ActivateResult{}, err
	}
	defer conn.Close()
	if err := WriteRequest(conn, req); err != nil {
		return ActivateResult{}, err
	}
	return ReadResult(conn)
}

func TestServeTLSControllerRoundTrip(t *testing.T) {
	p := newNetTestPKI(t)
	vm := &fakeVMM{}
	n := &fakeNotifier{}
	stub := newTestStubWithNotifier(t, vm, readyOK, n)
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	addr, stop := startServer(t, stub, p)
	defer stop()

	req := ActivateRequest{
		SnapshotDir: "/data/templates/tmpl-a/snapshot",
		Env:         map[string]string{"LANG": "C"},
		Secrets:     map[string]string{"API_KEY": "s3cr3t-value"},
	}
	res, err := activateClient(t, addr, p.clientConf(t, p.ctrlCert), req)
	if err != nil {
		t.Fatalf("activate over mTLS: %v", err)
	}
	if !res.OK {
		t.Fatalf("activate not OK: %q", res.Error)
	}
	// The stub received the secret-bearing request.
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.gotReq) != 1 {
		t.Fatalf("notifier saw %d requests, want 1", len(n.gotReq))
	}
	if n.gotReq[0].Secrets["API_KEY"] != "s3cr3t-value" {
		t.Fatalf("stub did not receive the secret")
	}
}

func TestServeTLSRejectsNoClientCert(t *testing.T) {
	p := newNetTestPKI(t)
	n := &fakeNotifier{}
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, n)
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	addr, stop := startServer(t, stub, p)
	defer stop()

	// No client certificate: the server requires and verifies one, so the
	// handshake fails. The activate request must never be processed.
	noCertConf := &tls.Config{
		RootCAs:    certPool(t, p.caPEM),
		ServerName: pki.ServerName,
		MinVersion: tls.VersionTLS13,
	}
	_, err := activateClient(t, addr, noCertConf, ActivateRequest{
		SnapshotDir: "/data/snap",
		Secrets:     map[string]string{"API_KEY": "leak-me"},
	})
	if err == nil {
		t.Fatalf("expected handshake failure for missing client cert")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.calls != 0 {
		t.Fatalf("stub processed an unauthenticated request (%d notifier calls)", n.calls)
	}
}

func TestServeTLSRejectsWrongCA(t *testing.T) {
	p := newNetTestPKI(t)
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, &fakeNotifier{})
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	addr, stop := startServer(t, stub, p)
	defer stop()

	// A controller leaf from a DIFFERENT CA: the server's ClientCAs pool does
	// not trust it, so verification fails at the handshake.
	otherCA, err := pki.NewCA("attacker")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	otherLeaf, err := otherCA.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue rogue leaf: %v", err)
	}
	rogueCert, err := tls.X509KeyPair(otherLeaf.CertPEM, otherLeaf.KeyPEM)
	if err != nil {
		t.Fatalf("rogue keypair: %v", err)
	}
	_, err = activateClient(t, addr, p.clientConf(t, rogueCert), ActivateRequest{SnapshotDir: "/data/snap"})
	if err == nil {
		t.Fatalf("expected handshake failure for wrong-CA client cert")
	}
}

func TestAuthorizeControllerIdentity(t *testing.T) {
	// nil state and an empty verified chain both fail closed.
	if err := AuthorizeControllerIdentity(nil); err == nil {
		t.Fatalf("nil state must be rejected")
	}
	if err := AuthorizeControllerIdentity(&tls.ConnectionState{}); err == nil {
		t.Fatalf("empty verified chain must be rejected")
	}

	// A verified chain whose leaf SAN is the husk server, not the controller, is
	// rejected by the identity check (defense in depth behind the TLS EKU split).
	ca, err := pki.NewCA("husk-test")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	serverLeaf, err := ca.Issue(pki.ServerName)
	if err != nil {
		t.Fatalf("issue server leaf: %v", err)
	}
	block, _ := pem.Decode(serverLeaf.CertPEM)
	if block == nil {
		t.Fatalf("decode server cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	wrongPeer := &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	if err := AuthorizeControllerIdentity(wrongPeer); err == nil {
		t.Fatalf("server identity must be rejected as an activate peer")
	}

	// The controller leaf is accepted.
	ctrlLeaf, err := ca.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue controller leaf: %v", err)
	}
	cblock, _ := pem.Decode(ctrlLeaf.CertPEM)
	ccert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		t.Fatalf("parse controller cert: %v", err)
	}
	ctrlPeer := &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{ccert}}}
	if err := AuthorizeControllerIdentity(ctrlPeer); err != nil {
		t.Fatalf("controller identity must be accepted: %v", err)
	}
}

func TestServeTLSRefusesWithoutTLSOrAuthorize(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, &fakeNotifier{})
	if err := ServeTLS(context.Background(), ln, stub, nil, AuthorizeControllerIdentity); err == nil {
		t.Fatalf("ServeTLS must refuse a nil TLS config")
	}
	p := newNetTestPKI(t)
	if err := ServeTLS(context.Background(), ln, stub, p.serverConf, nil); err == nil {
		t.Fatalf("ServeTLS must refuse a nil authorize hook")
	}
}

// TestServeTLSNoSecretInLogs runs a full activate plus a rejected handshake
// while capturing os.Stderr, and asserts the secret value never appears in any
// log output.
func TestServeTLSNoSecretInLogs(t *testing.T) {
	p := newNetTestPKI(t)
	stub := newTestStubWithNotifier(t, &fakeVMM{}, readyOK, &fakeNotifier{})
	if err := stub.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	logged := captureStderr(t, func() {
		addr, stop := startServer(t, stub, p)
		defer stop()

		const secret = "TOPSECRET-do-not-log"
		if _, err := activateClient(t, addr, p.clientConf(t, p.ctrlCert), ActivateRequest{
			SnapshotDir: "/data/snap",
			Secrets:     map[string]string{"API_KEY": secret},
		}); err != nil {
			t.Fatalf("activate: %v", err)
		}
		// Drive a rejected (no client cert) connection to exercise the error log
		// path; the request payload must still never be logged.
		noCertConf := &tls.Config{RootCAs: certPool(t, p.caPEM), ServerName: pki.ServerName, MinVersion: tls.VersionTLS13}
		_, _ = activateClient(t, addr, noCertConf, ActivateRequest{Secrets: map[string]string{"API_KEY": secret}})
		// Give the server goroutine a moment to flush its rejection log.
		time.Sleep(50 * time.Millisecond)
	})

	if strings.Contains(logged, "TOPSECRET-do-not-log") {
		t.Fatalf("secret value leaked into logs:\n%s", logged)
	}
}

// captureStderr redirects os.Stderr around fn and returns everything written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	var buf safeBuf
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = bufCopy(&buf, r)
	}()

	fn()

	os.Stderr = orig
	_ = w.Close()
	wg.Wait()
	_ = r.Close()
	return buf.String()
}

func bufCopy(dst *safeBuf, r *os.File) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = dst.Write(buf[:n])
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}

// safeBuf is a goroutine-safe bytes.Buffer for capturing concurrent log writes.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
