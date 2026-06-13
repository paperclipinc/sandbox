package daemon

import (
	"bytes"
	"context"
	"log"
	"net"
	"testing"

	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/kms"
	"github.com/paperclipinc/mitos/internal/volume"
	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// keyProbeEngine is a ForkEngine that, during CreateTemplate and Fork, reads the
// key the daemon stashed in the provider and records it, so a test can assert
// the request-delivered (wrapped) DEK was available to the engine, unwrapped via
// the KMS, during the call. It does no real work otherwise.
type keyProbeEngine struct {
	ForkEngine   // embedded so unused methods are present; calling them panics (none are exercised)
	prov         *fork.RequestKeyProvider
	createKey    []byte
	createKeyErr error
	forkKey      []byte
	forkKeyErr   error
}

func (e *keyProbeEngine) CreateTemplate(id, _ string, _ []string, _ []volume.Spec) error {
	k, err := e.prov.KeyFor(id)
	if err != nil {
		e.createKeyErr = err
		return nil
	}
	// Copy the bytes out; the engine zeroizes the per-use copy after the call.
	e.createKey = append([]byte(nil), k...)
	return nil
}

func (e *keyProbeEngine) Fork(snapshotID, sandboxID string, _ fork.ForkOpts) (*fork.ForkResult, error) {
	k, err := e.prov.KeyFor(snapshotID)
	if err != nil {
		e.forkKeyErr = err
	} else {
		e.forkKey = append([]byte(nil), k...)
	}
	return &fork.ForkResult{SandboxID: sandboxID, Endpoint: "vsock://test"}, nil
}

func (e *keyProbeEngine) GetCapacity() fork.Capacity {
	return fork.Capacity{TemplateDigests: map[string]string{}}
}

func newKeyTestClient(t *testing.T, engine ForkEngine, prov *fork.RequestKeyProvider) (forkdpb.ForkDaemonClient, *Server) {
	t.Helper()
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	srv.SetKeyProvider(prov)

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return forkdpb.NewForkDaemonClient(conn), srv
}

// secretDEK is a recognizable but non-real DEK the test scans the logs for.
var secretDEK = []byte("THIS-IS-A-SECRET-KEY-32-BYTES!!!")

// testWrapper builds a LocalKEK from a fixed KEK and wraps secretDEK once, so the
// tests can deliver the WRAPPED DEK plus its KEK id over the RPC.
func testWrapper(t *testing.T) (*kms.LocalKEK, kms.WrappedKey) {
	t.Helper()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	w, err := kms.NewLocalKEK(kek)
	if err != nil {
		t.Fatalf("NewLocalKEK: %v", err)
	}
	wrapped, err := w.Wrap(context.Background(), secretDEK)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	return w, wrapped
}

func TestCreateTemplateStashesWrappedKeyAndForgets(t *testing.T) {
	w, wrapped := testWrapper(t)
	prov := fork.NewRequestKeyProvider(w)
	engine := &keyProbeEngine{prov: prov}
	client, _ := newKeyTestClient(t, engine, prov)

	if _, err := client.CreateTemplate(context.Background(), &forkdpb.CreateTemplateRequest{
		TemplateId:    "tmpl1",
		Image:         "python:3.12-slim",
		EncryptionKey: wrapped.Ciphertext,
		KekId:         wrapped.KEKID,
	}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	if engine.createKeyErr != nil {
		t.Fatalf("engine had no key during CreateTemplate: %v", engine.createKeyErr)
	}
	if !bytes.Equal(engine.createKey, secretDEK) {
		t.Fatal("engine did not see the unwrapped DEK during CreateTemplate")
	}
	// After the call the daemon must have forgotten the wrapped DEK (hygiene).
	if _, err := prov.KeyFor("tmpl1"); err == nil {
		t.Fatal("daemon did not ForgetKey after CreateTemplate; wrapped DEK still present")
	}
}

func TestForkStashesWrappedKeyAndForgets(t *testing.T) {
	w, wrapped := testWrapper(t)
	prov := fork.NewRequestKeyProvider(w)
	engine := &keyProbeEngine{prov: prov}
	client, _ := newKeyTestClient(t, engine, prov)

	if _, err := client.Fork(context.Background(), &forkdpb.ForkRequest{
		SnapshotId:    "tmpl1",
		SandboxId:     "sb-1",
		EncryptionKey: wrapped.Ciphertext,
		KekId:         wrapped.KEKID,
	}); err != nil {
		t.Fatalf("Fork: %v", err)
	}

	if engine.forkKeyErr != nil {
		t.Fatalf("engine had no key during Fork: %v", engine.forkKeyErr)
	}
	if !bytes.Equal(engine.forkKey, secretDEK) {
		t.Fatal("engine did not see the unwrapped DEK during Fork")
	}
	if _, err := prov.KeyFor("tmpl1"); err == nil {
		t.Fatal("daemon did not ForgetKey after Fork; wrapped DEK still present")
	}
}

// TestKeyNeverLogged captures the daemon's log output across CreateTemplate and
// Fork and asserts neither the plaintext DEK nor the wrapped DEK ever appear in
// any log line.
func TestKeyNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	})

	w, wrapped := testWrapper(t)
	prov := fork.NewRequestKeyProvider(w)
	engine := &keyProbeEngine{prov: prov}
	client, _ := newKeyTestClient(t, engine, prov)

	if _, err := client.CreateTemplate(context.Background(), &forkdpb.CreateTemplateRequest{
		TemplateId:    "tmpl1",
		Image:         "python:3.12-slim",
		EncryptionKey: wrapped.Ciphertext,
		KekId:         wrapped.KEKID,
	}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if _, err := client.Fork(context.Background(), &forkdpb.ForkRequest{
		SnapshotId:    "tmpl1",
		SandboxId:     "sb-1",
		EncryptionKey: wrapped.Ciphertext,
		KekId:         wrapped.KEKID,
	}); err != nil {
		t.Fatalf("Fork: %v", err)
	}

	if bytes.Contains(buf.Bytes(), secretDEK) {
		t.Fatal("the plaintext DEK leaked into the daemon logs")
	}
	if bytes.Contains(buf.Bytes(), wrapped.Ciphertext) {
		t.Fatal("the wrapped DEK leaked into the daemon logs")
	}
}
