package cas

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileSHA(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256.Sum256(data)
}

func TestPutSnapshotAndMaterializeByteIdentical(t *testing.T) {
	src := t.TempDir()
	memData := bytes.Repeat([]byte{0x5A}, 10<<20)
	diskData := bytes.Repeat([]byte{0x3C}, 6<<20)
	mem := writeFile(t, src, "mem", memData)
	disk := writeFile(t, src, "disk", diskData)

	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, err := store.PutSnapshot(map[string]string{"mem": mem, "disk": disk}, "v1", 1000)
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	if !store.HasManifest(m.Digest()) {
		t.Fatalf("manifest not present after Put")
	}

	dst := t.TempDir()
	if err := store.Materialize(m.Digest(), dst); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if fileSHA(t, filepath.Join(dst, "mem")) != fileSHA(t, mem) {
		t.Fatalf("mem not byte-identical after Materialize")
	}
	if fileSHA(t, filepath.Join(dst, "disk")) != fileSHA(t, disk) {
		t.Fatalf("disk not byte-identical after Materialize")
	}
}

func TestPutSnapshotDedupAddsFewChunks(t *testing.T) {
	src := t.TempDir()
	base := bytes.Repeat([]byte{0x77}, 12<<20) // 3 full 4 MiB chunks
	a := writeFile(t, src, "a", base)

	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m1, err := store.PutSnapshot(map[string]string{"mem": a}, "v1", 1)
	if err != nil {
		t.Fatalf("PutSnapshot 1: %v", err)
	}
	uniqueAfter1 := countChunks(t, store)

	// Second file: same content but a few trailing bytes changed, so only the
	// last 4 MiB chunk differs; the first two chunks are shared.
	changed := append([]byte(nil), base...)
	for i := len(changed) - 8; i < len(changed); i++ {
		changed[i] ^= 0xFF
	}
	b := writeFile(t, src, "b", changed)
	m2, err := store.PutSnapshot(map[string]string{"mem": b}, "v1", 2)
	if err != nil {
		t.Fatalf("PutSnapshot 2: %v", err)
	}
	uniqueAfter2 := countChunks(t, store)

	added := uniqueAfter2 - uniqueAfter1
	if added != 1 {
		t.Fatalf("expected dedup to add exactly 1 new chunk, added %d", added)
	}
	if m1.Digest() == m2.Digest() {
		t.Fatalf("distinct snapshots produced identical manifest digest")
	}
}

func TestMaterializeCorruptedChunkFails(t *testing.T) {
	src := t.TempDir()
	mem := writeFile(t, src, "mem", bytes.Repeat([]byte{0x42}, 5<<20))

	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, err := store.PutSnapshot(map[string]string{"mem": mem}, "v1", 1)
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	// Tamper with the first chunk on disk.
	victim := m.Files[0].Chunks[0].Digest
	cp := store.chunkPath(victim)
	if err := os.WriteFile(cp, []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	err = store.Materialize(m.Digest(), t.TempDir())
	if err == nil {
		t.Fatalf("expected Materialize to fail on corrupted chunk")
	}
	if !strings.Contains(err.Error(), string(victim)) {
		t.Fatalf("error should name offending chunk %s, got: %v", victim, err)
	}
}

func TestMaterializeCorruptedChunkLeavesNoOutput(t *testing.T) {
	src := t.TempDir()
	mem := writeFile(t, src, "mem", bytes.Repeat([]byte{0x42}, 5<<20))

	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, err := store.PutSnapshot(map[string]string{"mem": mem}, "v1", 1)
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	// Corrupt the first chunk on disk so verification fails mid-file.
	victim := m.Files[0].Chunks[0].Digest
	if err := os.WriteFile(store.chunkPath(victim), []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	dst := t.TempDir()
	if err := store.Materialize(m.Digest(), dst); err == nil {
		t.Fatalf("expected Materialize to fail on corrupted chunk")
	}

	// The partial/corrupt destination file must not remain.
	if _, err := os.Stat(filepath.Join(dst, "mem")); !os.IsNotExist(err) {
		t.Fatalf("expected no destination file after failed Materialize, stat err: %v", err)
	}
}

func TestMissingChunks(t *testing.T) {
	src := t.TempDir()
	mem := writeFile(t, src, "mem", bytes.Repeat([]byte{0x42}, 5<<20))

	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, err := store.PutSnapshot(map[string]string{"mem": mem}, "v1", 1)
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	if got := store.MissingChunks(m); len(got) != 0 {
		t.Fatalf("expected no missing chunks, got %d", len(got))
	}

	// Build a manifest referencing a chunk that was never stored.
	m.Files = append(m.Files, FileEntry{
		Name:   "ghost",
		Size:   1,
		Chunks: []ChunkRef{{Digest: digestBytes([]byte("never stored")), Size: 1}},
	})
	miss := store.MissingChunks(m)
	if len(miss) != 1 {
		t.Fatalf("expected 1 missing chunk, got %d", len(miss))
	}
}

// TestGetManifestRejectsTraversalDigest asserts a traversal digest never
// reaches the filesystem: GetManifest returns an error rather than reading a
// file outside the store root.
func TestGetManifestRejectsTraversalDigest(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, bad := range []Digest{"../../x", "../../etc/passwd", "", "x"} {
		if _, err := store.GetManifest(bad); err == nil {
			t.Fatalf("GetManifest(%q) returned nil error; traversal not blocked", string(bad))
		}
	}
}

// TestHasRejectsInvalidDigest asserts the bool-returning lookups treat an
// invalid digest as not-present without touching an attacker-controlled path.
func TestHasRejectsInvalidDigest(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if store.HasChunk("../../etc/passwd") {
		t.Fatalf("HasChunk reported a traversal digest as present")
	}
	if store.HasManifest("../../etc/passwd") {
		t.Fatalf("HasManifest reported a traversal digest as present")
	}
}

// countChunks counts unique chunk files in the store.
func countChunks(t *testing.T, s *Store) int {
	t.Helper()
	count := 0
	root := filepath.Join(s.root, "chunks")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk chunks: %v", err)
	}
	return count
}
