package cas

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPinUnpinPersisted(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := digestBytes([]byte("manifest"))
	if err := store.Pin(d); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "pins", string(d))); err != nil {
		t.Fatalf("pin marker not written: %v", err)
	}
	if err := store.Unpin(d); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "pins", string(d))); !os.IsNotExist(err) {
		t.Fatalf("pin marker not removed: %v", err)
	}
}

func TestEvictToFitRespectsPinAndLRU(t *testing.T) {
	src := t.TempDir()
	store, err := New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Pinned snapshot: 2 distinct 4 MiB chunks (8 MiB).
	pinnedData := append(
		bytes.Repeat([]byte{0xA1}, ChunkSize),
		bytes.Repeat([]byte{0xA2}, ChunkSize)...,
	)
	pf := writeFile(t, src, "pinned", pinnedData)
	pinnedM, err := store.PutSnapshot(map[string]string{"mem": pf}, Metadata{VMMVersion: "v1", CreatedUnix: 1})
	if err != nil {
		t.Fatalf("PutSnapshot pinned: %v", err)
	}
	if err := store.Pin(pinnedM.Digest()); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	// Unpinned snapshot: 3 distinct 4 MiB chunks (12 MiB).
	unpinnedData := bytes.Join([][]byte{
		bytes.Repeat([]byte{0xB1}, ChunkSize),
		bytes.Repeat([]byte{0xB2}, ChunkSize),
		bytes.Repeat([]byte{0xB3}, ChunkSize),
	}, nil)
	uf := writeFile(t, src, "unpinned", unpinnedData)
	unpinnedM, err := store.PutSnapshot(map[string]string{"mem": uf}, Metadata{VMMVersion: "v1", CreatedUnix: 2})
	if err != nil {
		t.Fatalf("PutSnapshot unpinned: %v", err)
	}

	pinnedChunks := pinnedM.Files[0].Chunks
	unpinnedChunks := unpinnedM.Files[0].Chunks

	// Set deterministic mtimes so LRU order is unambiguous: oldest first among
	// the unpinned chunks. Pinned chunks get the oldest time of all to prove
	// pinning overrides LRU.
	base := time.Unix(1_000_000, 0)
	for _, c := range pinnedChunks {
		setMtime(t, store.chunkPath(c.Digest), base)
	}
	for i, c := range unpinnedChunks {
		setMtime(t, store.chunkPath(c.Digest), base.Add(time.Duration(i+1)*time.Hour))
	}

	// Total is 20 MiB. Budget of 14 MiB must evict 6 MiB. Pinned (8 MiB) is
	// off-limits, so eviction comes from the unpinned set: the two oldest
	// unpinned chunks (8 MiB) get removed to drop below budget.
	budget := int64(14 << 20)
	freed, err := store.EvictToFit(budget)
	if err != nil {
		t.Fatalf("EvictToFit: %v", err)
	}
	if freed != int64(2*ChunkSize) {
		t.Fatalf("expected to free %d bytes, freed %d", 2*ChunkSize, freed)
	}

	// Pinned chunks must survive.
	for _, c := range pinnedChunks {
		if !store.HasChunk(c.Digest) {
			t.Fatalf("pinned chunk %s was evicted", c.Digest)
		}
	}
	// The two oldest unpinned chunks must be gone.
	if store.HasChunk(unpinnedChunks[0].Digest) {
		t.Fatalf("oldest unpinned chunk %s should have been evicted", unpinnedChunks[0].Digest)
	}
	if store.HasChunk(unpinnedChunks[1].Digest) {
		t.Fatalf("second oldest unpinned chunk %s should have been evicted", unpinnedChunks[1].Digest)
	}
	// The newest unpinned chunk must remain.
	if !store.HasChunk(unpinnedChunks[2].Digest) {
		t.Fatalf("newest unpinned chunk %s should have survived", unpinnedChunks[2].Digest)
	}

	total, err := store.totalChunkBytes()
	if err != nil {
		t.Fatalf("totalChunkBytes: %v", err)
	}
	if total > budget {
		t.Fatalf("store still over budget: %d > %d", total, budget)
	}
}

func setMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}
