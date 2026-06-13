package fork

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/firecracker"
)

// newPullEngine builds a minimal Engine that can pull templates without KVM. The
// data dir starts empty (no template); env/metadata mirror the same-host build
// so a pulled snapshot re-derives to the holder's digest and the compat gate
// passes. client is the HTTP client PullTemplate dials the holder with.
func newPullEngine(t *testing.T, client *http.Client) *Engine {
	t.Helper()
	dataDir := t.TempDir()
	store := newTestStore(t, dataDir)
	return &Engine{
		dataDir:            dataDir,
		casStore:           store,
		pullClient:         client,
		env:                goodEnv(),
		unverifiedWarned:   make(map[string]struct{}),
		incompatibleWarned: make(map[string]struct{}),
		templateDigests:    make(map[string]cas.Digest),
	}
}

// startHolderCAS lays down a template on a holder engine's disk, records its
// digest with same-host metadata, and serves its CAS gated by token over an
// httptest server. It returns the CAS base URL, the recorded digest, and the
// holder's store (for assertions).
func startHolderCAS(t *testing.T, id, token string) (casURL string, digest cas.Digest) {
	t.Helper()
	dataDir := writeFakeTemplate(t, id)
	store := newTestStore(t, dataDir)
	holder := &Engine{
		dataDir:         dataDir,
		casStore:        store,
		env:             goodEnv(),
		templateDigests: make(map[string]cas.Digest),
	}
	d, err := recordTemplateDigest(store, dataDir, id, holder.manifestMetadata(defaultPullConfig()))
	if err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	gated := cas.RequirePullToken(token, cas.NewHTTPHandler(store))
	srv := httptest.NewServer(gated)
	t.Cleanup(srv.Close)
	return srv.URL + "/cas", d
}

// defaultPullConfig is the firecracker.DefaultVMConfig the verify path passes to
// manifestMetadata, so the holder stamps the same ConfigHash the puller
// re-derives. Both holder and puller leave kernelPath empty, which keeps
// ConfigHash reproducible across them.
func defaultPullConfig() firecracker.VMConfig {
	return firecracker.DefaultVMConfig()
}

func TestPullTemplateMaterializesVerifiesAndRecords(t *testing.T) {
	const id, token = "py", "peer-secret"
	casURL, digest := startHolderCAS(t, id, token)

	e := newPullEngine(t, http.DefaultClient)
	if err := e.PullTemplate(context.Background(), id, string(digest), casURL, token); err != nil {
		t.Fatalf("PullTemplate: %v", err)
	}

	// Layout: snapshot/{mem,vmstate} + rootfs.ext4 materialized into the template
	// dir, byte-identical to the holder's fake template.
	dir := filepath.Join(e.dataDir, "templates", id)
	assertFileBytes(t, filepath.Join(dir, "snapshot", "mem"), bytes.Repeat([]byte{0xAB}, 9<<20))
	assertFileBytes(t, filepath.Join(dir, "snapshot", "vmstate"), bytes.Repeat([]byte{0xCD}, 1<<20))
	assertFileBytes(t, filepath.Join(dir, "rootfs.ext4"), bytes.Repeat([]byte{0xEF}, 5<<20))

	if !isVerified(e.dataDir, id) {
		t.Fatal("verified marker not written after a good pull")
	}
	if got := e.GetCapacityDigest(id); got != digest {
		t.Fatalf("recorded digest %q, want %q", got, digest)
	}
}

func TestPullTemplateRejectsWrongToken(t *testing.T) {
	const id, token = "py", "peer-secret"
	casURL, digest := startHolderCAS(t, id, token)

	e := newPullEngine(t, http.DefaultClient)
	err := e.PullTemplate(context.Background(), id, string(digest), casURL, "wrong-token")
	if err == nil {
		t.Fatal("expected PullTemplate to fail with a wrong token")
	}
	assertNoTemplate(t, e, id)
}

func TestPullTemplateFailsClosedOnWrongDigest(t *testing.T) {
	const id, token = "py", "peer-secret"
	casURL, _ := startHolderCAS(t, id, token)

	// A syntactically valid sha256 hex that does not name the served manifest.
	bogus := "0000000000000000000000000000000000000000000000000000000000000000"
	e := newPullEngine(t, http.DefaultClient)
	err := e.PullTemplate(context.Background(), id, bogus, casURL, token)
	if err == nil {
		t.Fatal("expected PullTemplate to fail on a manifest digest the holder does not serve")
	}
	assertNoTemplate(t, e, id)
}

func TestPullTemplateFailsClosedOnTamperedChunk(t *testing.T) {
	const id, token = "py", "peer-secret"
	dataDir := writeFakeTemplate(t, id)
	store := newTestStore(t, dataDir)
	holder := &Engine{dataDir: dataDir, casStore: store, env: goodEnv(), templateDigests: make(map[string]cas.Digest)}
	d, err := recordTemplateDigest(store, dataDir, id, holder.manifestMetadata(defaultPullConfig()))
	if err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	m, err := store.GetManifest(d)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	target := m.Files[0].Chunks[0].Digest

	// Serve the real gated CAS but return garbage for one chunk, so the puller's
	// PutChunk digest verify rejects it and the pull fails closed.
	real := cas.RequirePullToken(token, cas.NewHTTPHandler(store))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cas/chunk/"+string(target), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("garbage")) //nolint:errcheck // test handler
	})
	mux.Handle("/", real)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newPullEngine(t, http.DefaultClient)
	if err := e.PullTemplate(context.Background(), id, string(d), srv.URL+"/cas", token); err == nil {
		t.Fatal("expected PullTemplate to fail on a tampered chunk")
	}
	assertNoTemplate(t, e, id)
}

// TestPullTemplateRefusesIncompatibleSnapshot proves the same compat gate the
// fork path uses runs at pull time: a snapshot stamped with a different VMM
// version is refused and left no marker.
func TestPullTemplateRefusesIncompatibleSnapshot(t *testing.T) {
	const id, token = "py", "peer-secret"
	dataDir := writeFakeTemplate(t, id)
	store := newTestStore(t, dataDir)
	// Holder stamps an incompatible VMM version into the manifest.
	incompatibleEnv := goodEnv()
	incompatibleEnv.VMMVersion = "v1.10.0"
	holder := &Engine{dataDir: dataDir, casStore: store, env: incompatibleEnv, templateDigests: make(map[string]cas.Digest)}
	d, err := recordTemplateDigest(store, dataDir, id, holder.manifestMetadata(defaultPullConfig()))
	if err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	gated := cas.RequirePullToken(token, cas.NewHTTPHandler(store))
	srv := httptest.NewServer(gated)
	defer srv.Close()

	// Puller runs the good (v1.15.0) env, so the pulled snapshot is incompatible.
	e := newPullEngine(t, http.DefaultClient)
	if err := e.PullTemplate(context.Background(), id, string(d), srv.URL+"/cas", token); err == nil {
		t.Fatal("expected PullTemplate to refuse an incompatible snapshot")
	}
	assertNoTemplate(t, e, id)
}

// GetCapacityDigest returns the recorded digest for a template id (test helper).
func (e *Engine) GetCapacityDigest(id string) cas.Digest {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.templateDigests[id]
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // test
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: got %d bytes, want %d", path, len(got), len(want))
	}
}

func assertNoTemplate(t *testing.T, e *Engine, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.dataDir, "templates", id)); !os.IsNotExist(err) {
		t.Fatalf("partial template dir for %s must be removed on a failed pull (stat err: %v)", id, err)
	}
	if isVerified(e.dataDir, id) {
		t.Fatalf("verified marker must not exist after a failed pull of %s", id)
	}
	if e.GetCapacityDigest(id) != "" {
		t.Fatalf("digest must not be recorded after a failed pull of %s", id)
	}
}
