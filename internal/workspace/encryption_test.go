package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/kms"
	"github.com/paperclipinc/mitos/internal/storecrypt"
)

// newTestKMS returns a LocalKEK over a fixed 32-byte KEK so a test can wrap and
// unwrap DEKs deterministically. The KEK is test-only material, not a secret in
// the production sense; it never leaves the test process.
func newTestKMS(t *testing.T) kms.Wrapper {
	t.Helper()
	kek := bytes.Repeat([]byte{0x42}, 32)
	w, err := kms.NewLocalKEK(kek)
	if err != nil {
		t.Fatalf("NewLocalKEK: %v", err)
	}
	return w
}

// newWorkspaceDEK mints a wrapped DEK for a workspace via the KMS, mirroring the
// controller path that delivers a wrapped DEK to the node. The returned key
// material is the unwrapped DEK the EncryptedStore uses.
func newWorkspaceDEK(t *testing.T, w kms.Wrapper) (storecrypt.Key, kms.WrappedKey) {
	t.Helper()
	dek, err := storecrypt.NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	wrapped, err := w.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	return dek, wrapped
}

// countEncryptedChunks counts the at-rest chunk files in an EncryptedStore, so a
// test can assert that re-dehydrating an identical tree writes zero new chunks
// (dedup preserved).
func countEncryptedChunks(t *testing.T, es *EncryptedStore) int {
	t.Helper()
	n := 0
	err := filepath.Walk(es.chunkRoot(), func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk chunks: %v", err)
	}
	return n
}

func TestEncryptedDehydrateHydrateRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w := newTestKMS(t)
	dek, _ := newWorkspaceDEK(t, w)
	defer dek.Zeroize()

	es, err := NewEncryptedStore(root, dek)
	if err != nil {
		t.Fatalf("NewEncryptedStore: %v", err)
	}

	src := newFakeAgent(t)
	want := map[string]string{
		"main.go":           "package main",
		"sub/nested.txt":    "nested content",
		"sub/deep/data.bin": "\x00\x01binary\xff",
		"empty":             "",
	}
	for rel, content := range want {
		src.writeFile(t, rel, content)
	}

	digest, err := DehydrateTo(ctx, src, es, nil, nil)
	if err != nil {
		t.Fatalf("DehydrateTo: %v", err)
	}
	if err := digest.Validate(); err != nil {
		t.Fatalf("invalid digest: %v", err)
	}

	dst := newFakeAgent(t)
	if err := HydrateFrom(ctx, dst, es, digest); err != nil {
		t.Fatalf("HydrateFrom: %v", err)
	}

	got := dst.listFiles(t)
	if len(got) != len(want) {
		t.Fatalf("round trip file count = %d, want %d", len(got), len(want))
	}
	for rel, content := range want {
		if got[rel] != content {
			t.Errorf("round trip %s = %q, want %q", rel, got[rel], content)
		}
	}
}

func TestEncryptedDigestMatchesPlaintextDigest(t *testing.T) {
	// The revision content identifier (the manifest digest) is computed over
	// PLAINTEXT, so an encrypted dehydrate and a plaintext dehydrate of the SAME
	// tree produce the SAME digest. This is what keeps content-addressed dedup
	// intact across the encryption boundary.
	ctx := context.Background()

	tree := map[string]string{"a.txt": "alpha", "b/c.txt": "charlie"}

	plain := newStore(t)
	pa := newFakeAgent(t)
	for rel, c := range tree {
		pa.writeFile(t, rel, c)
	}
	plainDigest, err := Dehydrate(ctx, pa, plain, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	w := newTestKMS(t)
	dek, _ := newWorkspaceDEK(t, w)
	defer dek.Zeroize()
	es, err := NewEncryptedStore(t.TempDir(), dek)
	if err != nil {
		t.Fatal(err)
	}
	ea := newFakeAgent(t)
	for rel, c := range tree {
		ea.writeFile(t, rel, c)
	}
	encDigest, err := DehydrateTo(ctx, ea, es, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if plainDigest != encDigest {
		t.Fatalf("encrypted digest %s != plaintext digest %s; encryption broke content addressing", encDigest, plainDigest)
	}
}

func TestEncryptedDehydrateDedups(t *testing.T) {
	ctx := context.Background()
	w := newTestKMS(t)
	dek, _ := newWorkspaceDEK(t, w)
	defer dek.Zeroize()
	es, err := NewEncryptedStore(t.TempDir(), dek)
	if err != nil {
		t.Fatal(err)
	}

	a := newFakeAgent(t)
	a.writeFile(t, "a.txt", "alpha")
	a.writeFile(t, "b/c.txt", "charlie")
	d1, err := DehydrateTo(ctx, a, es, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	chunksAfterFirst := countEncryptedChunks(t, es)

	b := newFakeAgent(t)
	b.writeFile(t, "a.txt", "alpha")
	b.writeFile(t, "b/c.txt", "charlie")
	d2, err := DehydrateTo(ctx, b, es, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("unchanged tree produced different digests: %s != %s", d1, d2)
	}
	if got := countEncryptedChunks(t, es); got != chunksAfterFirst {
		t.Fatalf("re-dehydrating an identical tree wrote new chunks (%d -> %d); dedup broken", chunksAfterFirst, got)
	}
}

func TestEncryptedChunksAreCiphertextAtRest(t *testing.T) {
	ctx := context.Background()
	w := newTestKMS(t)
	dek, _ := newWorkspaceDEK(t, w)
	defer dek.Zeroize()
	es, err := NewEncryptedStore(t.TempDir(), dek)
	if err != nil {
		t.Fatal(err)
	}
	a := newFakeAgent(t)
	const marker = "PLAINTEXT-MARKER-shouldnotappear"
	a.writeFile(t, "secret.txt", marker)
	if _, err := DehydrateTo(ctx, a, es, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Scan every on-disk chunk: the plaintext marker must not appear.
	root := es.chunkRoot()
	walkErr := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // test reads its own temp store
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(marker)) {
			t.Fatalf("plaintext marker found in at-rest chunk %s; chunk not encrypted", p)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

func TestEncryptedHydrateWrongKeyFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w := newTestKMS(t)
	dek, _ := newWorkspaceDEK(t, w)

	es, err := NewEncryptedStore(root, dek)
	if err != nil {
		t.Fatal(err)
	}
	a := newFakeAgent(t)
	a.writeFile(t, "a.txt", "alpha")
	digest, err := DehydrateTo(ctx, a, es, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dek.Zeroize()

	wrong, err := storecrypt.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Zeroize()
	es2, err := NewEncryptedStore(root, wrong)
	if err != nil {
		t.Fatal(err)
	}
	dst := newFakeAgent(t)
	if err := HydrateFrom(ctx, dst, es2, digest); err == nil {
		t.Fatal("HydrateFrom with the wrong key succeeded; encryption did not fail closed")
	}
}
