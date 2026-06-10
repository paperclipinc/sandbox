package cas

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// recordingTransport wraps a Store and records how many chunks were fetched, so
// tests can assert that Pull transfers only the delta.
type recordingTransport struct {
	store    *Store
	getChunk int
	fetched  map[Digest]struct{}
}

func newRecordingTransport(store *Store) *recordingTransport {
	return &recordingTransport{store: store, fetched: make(map[Digest]struct{})}
}

func (r *recordingTransport) HasChunks(_ context.Context, digests []Digest) (map[Digest]bool, error) {
	present := make(map[Digest]bool, len(digests))
	for _, d := range digests {
		present[d] = r.store.HasChunk(d)
	}
	return present, nil
}

func (r *recordingTransport) GetChunk(_ context.Context, d Digest) (io.ReadCloser, error) {
	r.getChunk++
	r.fetched[d] = struct{}{}
	data, err := readChunk(r.store, d)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (r *recordingTransport) GetManifest(_ context.Context, d Digest) (Manifest, error) {
	return r.store.GetManifest(d)
}

func readChunk(s *Store, d Digest) ([]byte, error) {
	return readFileBytes(s.chunkPath(d))
}

func TestPullInProcessDeltaOnly(t *testing.T) {
	srcA := t.TempDir()
	memData := bytes.Repeat([]byte{0x11}, 10<<20)
	diskData := bytes.Repeat([]byte{0x22}, 6<<20)
	mem := writeFile(t, srcA, "mem", memData)
	disk := writeFile(t, srcA, "disk", diskData)

	storeA := newStore(t)
	storeB := newStore(t)

	mA, err := storeA.PutSnapshot(map[string]string{"mem": mem, "disk": disk}, "v1", 1000)
	if err != nil {
		t.Fatalf("PutSnapshot A: %v", err)
	}

	remote := newRecordingTransport(storeA)
	if err := Pull(context.Background(), storeB, remote, mA.Digest()); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	wantChunks := len(storeB.MissingChunksOf(mA))
	if wantChunks != 0 {
		t.Fatalf("after Pull B still missing %d chunks", wantChunks)
	}
	allChunks := uniqueChunkCount(mA)
	if remote.getChunk != allChunks {
		t.Fatalf("first Pull fetched %d chunks, want all %d", remote.getChunk, allChunks)
	}

	dst := t.TempDir()
	if err := storeB.Materialize(mA.Digest(), dst); err != nil {
		t.Fatalf("Materialize from B: %v", err)
	}
	assertByteIdentical(t, filepath.Join(dst, "mem"), memData)
	assertByteIdentical(t, filepath.Join(dst, "disk"), diskData)

	// Second snapshot overlaps the first: same mem, new disk. Only the new
	// disk chunks should transfer.
	newDiskData := bytes.Repeat([]byte{0x33}, 6<<20)
	newDisk := writeFile(t, srcA, "disk2", newDiskData)
	mA2, err := storeA.PutSnapshot(map[string]string{"mem": mem, "disk": newDisk}, "v1", 1001)
	if err != nil {
		t.Fatalf("PutSnapshot A2: %v", err)
	}

	wantDelta := chunksNotIn(mA2, mA) // chunks unique to disk2
	if wantDelta == 0 || wantDelta == uniqueChunkCount(mA2) {
		t.Fatalf("test setup invalid: delta %d of %d total", wantDelta, uniqueChunkCount(mA2))
	}
	remote2 := newRecordingTransport(storeA)
	if err := Pull(context.Background(), storeB, remote2, mA2.Digest()); err != nil {
		t.Fatalf("Pull 2: %v", err)
	}
	if remote2.getChunk != wantDelta {
		t.Fatalf("second Pull fetched %d chunks, want delta %d", remote2.getChunk, wantDelta)
	}

	dst2 := t.TempDir()
	if err := storeB.Materialize(mA2.Digest(), dst2); err != nil {
		t.Fatalf("Materialize 2: %v", err)
	}
	assertByteIdentical(t, filepath.Join(dst2, "disk"), newDiskData)
}

func TestPullOverHTTPDeltaOnlyAndByteIdentical(t *testing.T) {
	srcA := t.TempDir()
	memData := bytes.Repeat([]byte{0x44}, 9<<20)
	diskData := bytes.Repeat([]byte{0x55}, 5<<20)
	mem := writeFile(t, srcA, "mem", memData)
	disk := writeFile(t, srcA, "disk", diskData)

	storeA := newStore(t)
	storeB := newStore(t)

	mA, err := storeA.PutSnapshot(map[string]string{"mem": mem, "disk": disk}, "v1", 2000)
	if err != nil {
		t.Fatalf("PutSnapshot A: %v", err)
	}

	srv := httptest.NewServer(NewHTTPHandler(storeA))
	defer srv.Close()
	transport := NewHTTPTransport(srv.URL, srv.Client())

	if err := Pull(context.Background(), storeB, transport, mA.Digest()); err != nil {
		t.Fatalf("Pull over HTTP: %v", err)
	}

	dst := t.TempDir()
	if err := storeB.Materialize(mA.Digest(), dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	assertByteIdentical(t, filepath.Join(dst, "mem"), memData)
	assertByteIdentical(t, filepath.Join(dst, "disk"), diskData)

	// Delta-only over HTTP: second snapshot shares mem.
	newDiskData := bytes.Repeat([]byte{0x66}, 5<<20)
	newDisk := writeFile(t, srcA, "disk2", newDiskData)
	mA2, err := storeA.PutSnapshot(map[string]string{"mem": mem, "disk": newDisk}, "v1", 2001)
	if err != nil {
		t.Fatalf("PutSnapshot A2: %v", err)
	}

	before := uniqueChunkCount(mA2)
	missingBefore := len(storeB.MissingChunksOf(mA2))
	if missingBefore >= before {
		t.Fatalf("expected mem chunks already present, missing %d of %d", missingBefore, before)
	}
	if err := Pull(context.Background(), storeB, transport, mA2.Digest()); err != nil {
		t.Fatalf("Pull 2 over HTTP: %v", err)
	}
	if remaining := len(storeB.MissingChunksOf(mA2)); remaining != 0 {
		t.Fatalf("after delta Pull still missing %d chunks", remaining)
	}
}

// tamperTransport serves a wrong-byte chunk for one targeted digest.
type tamperTransport struct {
	inner  Transport
	target Digest
}

func (tt *tamperTransport) HasChunks(ctx context.Context, digests []Digest) (map[Digest]bool, error) {
	return tt.inner.HasChunks(ctx, digests)
}

func (tt *tamperTransport) GetManifest(ctx context.Context, d Digest) (Manifest, error) {
	return tt.inner.GetManifest(ctx, d)
}

func (tt *tamperTransport) GetChunk(ctx context.Context, d Digest) (io.ReadCloser, error) {
	if d == tt.target {
		return io.NopCloser(bytes.NewReader([]byte("corrupted bytes"))), nil
	}
	return tt.inner.GetChunk(ctx, d)
}

func TestPullRejectsTamperedChunk(t *testing.T) {
	src := t.TempDir()
	memData := bytes.Repeat([]byte{0x77}, 8<<20)
	mem := writeFile(t, src, "mem", memData)

	storeA := newStore(t)
	storeB := newStore(t)
	mA, err := storeA.PutSnapshot(map[string]string{"mem": mem}, "v1", 3000)
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	target := mA.Files[0].Chunks[0].Digest
	remote := &tamperTransport{inner: newRecordingTransport(storeA), target: target}

	err = Pull(context.Background(), storeB, remote, mA.Digest())
	if err == nil {
		t.Fatal("expected Pull to fail on tampered chunk")
	}
	if storeB.HasChunk(target) {
		t.Fatal("tampered chunk must not be stored")
	}
}

func TestPullRejectsTamperedChunkOverHTTP(t *testing.T) {
	src := t.TempDir()
	memData := bytes.Repeat([]byte{0x88}, 8<<20)
	mem := writeFile(t, src, "mem", memData)

	storeA := newStore(t)
	storeB := newStore(t)
	mA, err := storeA.PutSnapshot(map[string]string{"mem": mem}, "v1", 4000)
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	target := mA.Files[0].Chunks[0].Digest

	mux := http.NewServeMux()
	real := NewHTTPHandler(storeA)
	mux.HandleFunc("GET /cas/chunk/"+string(target), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("garbage")) //nolint:errcheck // test handler
	})
	mux.Handle("/", real)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, srv.Client())
	if err := Pull(context.Background(), storeB, transport, mA.Digest()); err == nil {
		t.Fatal("expected Pull over HTTP to fail on tampered chunk")
	}
	if storeB.HasChunk(target) {
		t.Fatal("tampered chunk must not be stored")
	}
}

// MissingChunksOf exposes MissingChunks for tests in this package.
func (s *Store) MissingChunksOf(m Manifest) []Digest { return s.MissingChunks(m) }

// chunksNotIn counts chunks unique to a that do not appear in b.
func chunksNotIn(a, b Manifest) int {
	bSet := make(map[Digest]struct{})
	for _, fe := range b.Files {
		for _, c := range fe.Chunks {
			bSet[c.Digest] = struct{}{}
		}
	}
	delta := make(map[Digest]struct{})
	for _, fe := range a.Files {
		for _, c := range fe.Chunks {
			if _, ok := bSet[c.Digest]; !ok {
				delta[c.Digest] = struct{}{}
			}
		}
	}
	return len(delta)
}

func uniqueChunkCount(m Manifest) int {
	seen := make(map[Digest]struct{})
	for _, fe := range m.Files {
		for _, c := range fe.Chunks {
			seen[c.Digest] = struct{}{}
		}
	}
	return len(seen)
}

// TestHTTPHandlerRejectsInvalidDigest asserts the read surface returns 400 Bad
// Request for a malformed or traversal digest before touching the store, so an
// encoded "../../etc/passwd" in the request path cannot escape the store root.
func TestHTTPHandlerRejectsInvalidDigest(t *testing.T) {
	srv := httptest.NewServer(NewHTTPHandler(newStore(t)))
	defer srv.Close()

	cases := []string{
		"/cas/chunk/" + url.PathEscape("../../etc/passwd"),
		"/cas/chunk/not-a-digest",
		"/cas/manifest/" + url.PathEscape("../../etc/passwd"),
		"/cas/manifest/short",
	}
	for _, path := range cases {
		resp, err := http.Get(srv.URL + path) //nolint:noctx,bodyclose // test
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET %s: status %d, want 400", path, resp.StatusCode)
		}
	}

	// POST /cas/has must reject a body containing any invalid digest.
	resp, err := http.Post(srv.URL+"/cas/has", "application/json", //nolint:noctx,bodyclose // test
		bytes.NewReader([]byte(`["../../etc/passwd"]`)))
	if err != nil {
		t.Fatalf("POST /cas/has: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /cas/has with invalid digest: status %d, want 400", resp.StatusCode)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func assertByteIdentical(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file %s not byte-identical: got %d bytes want %d", path, len(got), len(want))
	}
}

func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // test helper
}
