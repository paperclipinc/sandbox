package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestChunkFileTenMiB(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0xAB}, 10<<20) // 10 MiB
	p := writeFile(t, dir, "mem", data)

	chunks, err := chunkFile(p)
	if err != nil {
		t.Fatalf("chunkFile: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks for 10 MiB, got %d", len(chunks))
	}
	if chunks[0].Size != ChunkSize || chunks[1].Size != ChunkSize {
		t.Fatalf("expected first two chunks of ChunkSize, got %d and %d", chunks[0].Size, chunks[1].Size)
	}
	if chunks[2].Size != (10<<20)-2*ChunkSize {
		t.Fatalf("expected last chunk size %d, got %d", (10<<20)-2*ChunkSize, chunks[2].Size)
	}
}

func TestChunkFileIdenticalContentIdenticalDigests(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0x01, 0x02, 0x03}, 2<<20)
	p1 := writeFile(t, dir, "a", data)
	p2 := writeFile(t, dir, "b", data)

	c1, err := chunkFile(p1)
	if err != nil {
		t.Fatalf("chunkFile a: %v", err)
	}
	c2, err := chunkFile(p2)
	if err != nil {
		t.Fatalf("chunkFile b: %v", err)
	}
	if len(c1) != len(c2) {
		t.Fatalf("chunk counts differ: %d vs %d", len(c1), len(c2))
	}
	for i := range c1 {
		if c1[i].Digest != c2[i].Digest {
			t.Fatalf("chunk %d digest differs: %s vs %s", i, c1[i].Digest, c2[i].Digest)
		}
	}
}

func TestChunkFileEmpty(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "empty", nil)
	chunks, err := chunkFile(p)
	if err != nil {
		t.Fatalf("chunkFile: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for empty file, got %d", len(chunks))
	}
}

func TestDigestValidate(t *testing.T) {
	real := digestBytes([]byte("hello world"))
	if err := real.Validate(); err != nil {
		t.Fatalf("real sha256 digest rejected: %v", err)
	}

	bad := []Digest{
		"",
		"..",
		"../../etc/passwd",
		Digest(strings.ToUpper(string(real))),                 // uppercase hex
		Digest(string(real)[:63]),                             // 63 chars
		Digest(string(real) + "0"),                            // 65 chars
		"g000000000000000000000000000000000000000000000000000000000000000", // non-hex
	}
	for _, d := range bad {
		if err := d.Validate(); err == nil {
			t.Fatalf("Validate accepted invalid digest %q", string(d))
		}
	}
}

func TestDigestBytesStable(t *testing.T) {
	data := []byte("hello world")
	got := digestBytes(data)
	sum := sha256.Sum256(data)
	want := Digest(hex.EncodeToString(sum[:]))
	if got != want {
		t.Fatalf("digestBytes = %s, want %s", got, want)
	}
}
