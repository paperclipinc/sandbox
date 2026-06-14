package workspace

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/storecrypt"
)

// ChunkStore is the narrow content-addressed surface the workspace transfer
// helpers need: assemble a manifest from a set of plaintext files (dedup and
// content addressing happen here), and reconstruct a named file from a manifest
// to an explicit path. The plaintext *cas.Store satisfies it directly; the
// EncryptedStore satisfies it while encrypting every chunk and manifest at rest.
//
// Keeping this interface plaintext-addressed is what preserves the two
// invariants the plan requires across the encryption boundary: the manifest
// digest (a revision's content identifier) is computed over PLAINTEXT, so an
// encrypted dehydrate of a tree yields the SAME digest as a plaintext dehydrate
// of that tree (content-addressed dedup is preserved), and a hydrate reconstructs
// byte-identical plaintext.
type ChunkStore interface {
	// PutSnapshot stores the plaintext files (encrypting at rest if the backend
	// encrypts) and returns the content-addressed manifest. The returned
	// manifest's Digest is the revision content identifier.
	PutSnapshot(files map[string]string, meta cas.Metadata) (cas.Manifest, error)
	// GetManifest loads the manifest for a digest.
	GetManifest(d cas.Digest) (cas.Manifest, error)
	// MaterializeFileTo reconstructs the named plaintext file from a manifest to
	// dstPath, verifying integrity (and decrypting if the backend encrypts).
	MaterializeFileTo(manifest cas.Digest, name, dstPath string) error
}

// plainStore adapts a *cas.Store to the ChunkStore interface (it already has the
// exact method set; this keeps the type-assertion explicit at the call site).
type plainStore struct{ *cas.Store }

var _ ChunkStore = plainStore{}

// EncryptedStore is a content-addressed chunk store whose chunks and manifests
// are encrypted at rest with AES-256-GCM under a data-encryption key (DEK). The
// DEK scope is per-template (keyed by templateID, see
// internal/controller/enc_key_secret.go), NOT per-workspace: a single template's
// workspaces share the template's DEK. The nonce scheme is digest-keyed and so
// stays safe under a shared key (each distinct plaintext chunk gets a distinct,
// deterministic nonce; see nonceFor), but isolation granularity is the template,
// not the individual workspace. Per-workspace crypto-shred is a future option if
// finer-grained key destruction is wanted.
//
// It mirrors the envelope pattern in internal/fork: the DEK is a secret VALUE
// supplied by the KMS-backed key custody path (the controller wraps a per-template
// DEK with the KEK; the node unwraps it via internal/kms and hands the plaintext
// DEK here), and it is NEVER logged, never placed in an error, and never written
// to disk.
//
// At-rest layout mirrors *cas.Store so dedup is preserved by plaintext digest:
//
//	<root>/chunks/<plainDigest[:2]>/<plainDigest>   AES-256-GCM(chunk)
//	<root>/manifests/<manifestDigest>               AES-256-GCM(canonical manifest)
//
// A chunk's at-rest name is its PLAINTEXT digest, and its GCM nonce is derived
// deterministically from that digest (a keyed HMAC over the digest), so a chunk
// re-encrypted from identical plaintext produces byte-identical ciphertext: the
// at-rest dedup skip ("file already exists at this digest path") still holds and
// re-dehydrating an unchanged tree writes zero new chunks.
type EncryptedStore struct {
	*envelope
	root string
}

var _ blobCASManifest = (*EncryptedStore)(nil)

// envelope holds the AES-256-GCM cipher and the deterministic-nonce derivation
// keyed by the DEK. It is the reusable seal/open core shared by the filesystem
// EncryptedStore and the EncryptedS3Store, so both encrypt at rest identically
// (same nonce scheme, same digest-as-AAD integrity gate) over different blob
// backends.
type envelope struct {
	aead cipher.AEAD
	// nonceKey is a separate HMAC key derived from the DEK, used only to derive
	// the deterministic per-chunk nonce from the plaintext digest. Deriving it
	// from the DEK (rather than reusing the DEK as the HMAC key) keeps the GCM
	// key and the nonce-derivation key domain-separated.
	nonceKey []byte
}

// newEnvelope builds the AES-256-GCM envelope from a 32-byte DEK. The DEK is a
// secret value the caller may Zeroize after this returns: newEnvelope retains
// only the derived cipher and nonce key, not the caller's slice.
func newEnvelope(dek storecrypt.Key) (*envelope, error) {
	if len(dek) != 32 {
		return nil, fmt.Errorf("workspace DEK must be 32 bytes (AES-256); got %d", len(dek))
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("init AES cipher for workspace store: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init GCM for workspace store: %w", err)
	}
	// Domain-separate the nonce-derivation key from the GCM key.
	mac := hmac.New(sha256.New, dek)
	mac.Write([]byte("mitos/workspace/nonce-derivation/v1"))
	return &envelope{aead: aead, nonceKey: mac.Sum(nil)}, nil
}

// NewEncryptedStore opens (creating the skeleton) an encrypted chunk store at
// root, using dek (a 32-byte AES-256 DEK) for at-rest encryption. The DEK is a
// secret value: the store copies what it needs into the AES-GCM cipher and the
// nonce-derivation key and does not retain the caller's slice, so the caller may
// Zeroize the passed key after this returns.
func NewEncryptedStore(root string, dek storecrypt.Key) (*EncryptedStore, error) {
	env, err := newEnvelope(dek)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"chunks", "manifests"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return &EncryptedStore{envelope: env, root: root}, nil
}

// chunkRoot is the chunks directory, exposed for tests that assert ciphertext at
// rest.
func (e *EncryptedStore) chunkRoot() string { return filepath.Join(e.root, "chunks") }

// chunkPath returns the at-rest path for a plaintext chunk digest.
func (e *EncryptedStore) chunkPath(d cas.Digest) string {
	return filepath.Join(e.root, "chunks", string(d)[:2], string(d))
}

// manifestPath returns the at-rest path for a manifest digest.
func (e *EncryptedStore) manifestPath(d cas.Digest) string {
	return filepath.Join(e.root, "manifests", string(d))
}

// nonceFor derives the deterministic GCM nonce for a plaintext digest. It is a
// keyed HMAC truncated to the GCM nonce size, so it is unpredictable without the
// DEK yet stable for a given (DEK, plaintext) pair, which preserves at-rest
// dedup. A digest is unique per distinct plaintext chunk (sha256), so each
// distinct chunk gets a distinct nonce: there is no GCM nonce reuse across
// distinct plaintexts under one key.
func (e *envelope) nonceFor(d cas.Digest) []byte {
	mac := hmac.New(sha256.New, e.nonceKey)
	mac.Write([]byte(d))
	return mac.Sum(nil)[:e.aead.NonceSize()]
}

// seal encrypts plaintext addressed by its plaintext digest, returning
// nonce-prefixed ciphertext (nonce || ciphertext+tag). The nonce is
// deterministic in (DEK, digest) so the output is byte-identical for identical
// plaintext, preserving at-rest dedup.
func (e *envelope) seal(d cas.Digest, plaintext []byte) []byte {
	nonce := e.nonceFor(d)
	return e.aead.Seal(append([]byte(nil), nonce...), nonce, plaintext, []byte(d))
}

// open decrypts at-rest bytes for a plaintext digest and verifies the recovered
// plaintext hashes back to that digest. The GCM tag plus the digest re-check is
// the integrity gate: a wrong DEK fails the GCM Open (fail closed), and any
// tampering fails either GCM or the digest check.
func (e *envelope) open(d cas.Digest, atRest []byte) ([]byte, error) {
	ns := e.aead.NonceSize()
	if len(atRest) < ns {
		return nil, fmt.Errorf("encrypted chunk %s too short: %d bytes", d, len(atRest))
	}
	nonce, ct := atRest[:ns], atRest[ns:]
	pt, err := e.aead.Open(nil, nonce, ct, []byte(d))
	if err != nil {
		return nil, fmt.Errorf("decrypt chunk %s: GCM authentication failed (wrong workspace key or corrupted chunk): %w", d, err)
	}
	if got := chunkDigest(pt); got != d {
		return nil, fmt.Errorf("decrypted chunk %s failed digest verification: got %s", d, got)
	}
	return pt, nil
}

// chunkDigest returns the lowercase hex sha256 of b as a cas.Digest.
func chunkDigest(b []byte) cas.Digest {
	sum := sha256.Sum256(b)
	return cas.Digest(hex.EncodeToString(sum[:]))
}

// hasChunk reports whether the encrypted chunk for a plaintext digest is present.
func (e *EncryptedStore) hasChunk(d cas.Digest) bool {
	if d.Validate() != nil {
		return false
	}
	_, err := os.Stat(e.chunkPath(d))
	return err == nil
}

// PutSnapshot chunks each plaintext file, encrypts each distinct chunk at rest
// (skipping a chunk already present at its plaintext digest path: the dedup
// skip), and returns the plaintext-addressed manifest, which it also stores
// encrypted. The returned manifest digest equals what a plaintext *cas.Store
// would produce for the same files, so content addressing and dedup are
// preserved across the encryption boundary. It shares the content-addressing
// driver with the S3 backends so all backends behave identically.
func (e *EncryptedStore) PutSnapshot(files map[string]string, meta cas.Metadata) (cas.Manifest, error) {
	return putSnapshotBlobs(files, meta, e)
}

// GetManifest loads, decrypts, and verifies the manifest for a digest. A wrong
// key fails the GCM Open here (fail closed) before any chunk is touched.
func (e *EncryptedStore) GetManifest(d cas.Digest) (cas.Manifest, error) {
	if err := d.Validate(); err != nil {
		return cas.Manifest{}, err
	}
	atRest, err := os.ReadFile(e.manifestPath(d)) //nolint:gosec // digest validated above
	if err != nil {
		return cas.Manifest{}, fmt.Errorf("read encrypted manifest %s: %w", d, err)
	}
	pt, err := e.open(d, atRest)
	if err != nil {
		return cas.Manifest{}, err
	}
	m, err := cas.DecodeManifest(pt)
	if err != nil {
		return cas.Manifest{}, fmt.Errorf("decode manifest %s: %w", d, err)
	}
	return m, nil
}

// MaterializeFileTo reconstructs a single named plaintext file from the manifest
// to dstPath, decrypting and verifying each chunk. A wrong key fails the GCM Open
// per chunk (fail closed).
func (e *EncryptedStore) MaterializeFileTo(manifestDigest cas.Digest, name, dstPath string) error {
	return materializeBlobFile(manifestDigest, name, dstPath, e)
}

// --- blobCAS implementation backed by the encrypted filesystem layout -------

func (e *EncryptedStore) hasBlob(d cas.Digest) bool { return e.hasChunk(d) }

func (e *EncryptedStore) sealChunk(d cas.Digest, block []byte) []byte { return e.seal(d, block) }

func (e *EncryptedStore) sealManifest(d cas.Digest, canonical []byte) []byte {
	return e.seal(d, canonical)
}

func (e *EncryptedStore) openChunk(d cas.Digest, atRest []byte) ([]byte, error) {
	return e.open(d, atRest)
}

func (e *EncryptedStore) putChunkBlob(d cas.Digest, atRest []byte) error {
	return writeAtRest(e.chunkPath(d), atRest)
}

func (e *EncryptedStore) putManifestBlob(d cas.Digest, atRest []byte) error {
	return writeAtRest(e.manifestPath(d), atRest)
}

func (e *EncryptedStore) getChunkBlob(d cas.Digest) ([]byte, error) {
	return os.ReadFile(e.chunkPath(d)) //nolint:gosec // path derived from a validated digest
}

// readFull reads exactly len(buf) bytes from r, returning an error on a short
// read. It exists so the chunk reader reads precisely the chunk size
// BuildManifest recorded.
func readFull(r io.Reader, buf []byte) (int, error) {
	return io.ReadFull(r, buf)
}

// openForRead opens a snapshot file for reading.
func openForRead(p string) (*os.File, error) {
	f, err := os.Open(p) //nolint:gosec // internal snapshot file from the controller-owned temp dir
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	return f, nil
}

// createForWrite creates a destination file, making its parent dir.
func createForWrite(dst string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir for %s: %w", dst, err)
	}
	f, err := os.Create(dst) //nolint:gosec // dst is caller-validated (safeJoin)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", dst, err)
	}
	return f, nil
}

// removePartial best-effort removes a partially written destination on error.
func removePartial(dst string) {
	_ = os.Remove(dst) //nolint:errcheck // best-effort partial-output cleanup
}

// writeAtRest writes data atomically (temp + rename) under dst, creating the
// parent shard directory. Readers never observe a partial ciphertext file.
func writeAtRest(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir at-rest shard: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()        //nolint:errcheck // already failing
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("rename temp to %s: %w", dst, err)
	}
	return nil
}
