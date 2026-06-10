package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Store is a content-addressed store rooted at a directory. Layout:
//
//	<root>/chunks/<digest[:2]>/<digest>   chunk data
//	<root>/manifests/<digest>             canonical manifest bytes
//	<root>/pins/<digest>                  pin marker for a manifest
//
// All writes are atomic (temp file in the same directory then rename) so a
// crash never leaves a partial chunk under its final digest name.
type Store struct {
	root string
}

// New creates (or opens) a store rooted at root, creating the directory
// skeleton if needed.
func New(root string) (*Store, error) {
	for _, sub := range []string{"chunks", "manifests", "pins"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return &Store{root: root}, nil
}

// chunkPath returns the on-disk path for a chunk digest.
func (s *Store) chunkPath(d Digest) string {
	return filepath.Join(s.root, "chunks", string(d)[:2], string(d))
}

// manifestPath returns the on-disk path for a manifest digest.
func (s *Store) manifestPath(d Digest) string {
	return filepath.Join(s.root, "manifests", string(d))
}

// HasChunk reports whether the chunk is present in the store. An invalid
// digest can never name a stored chunk, so it reports false rather than
// touching the filesystem with an attacker-controlled path.
func (s *Store) HasChunk(d Digest) bool {
	if d.Validate() != nil {
		return false
	}
	_, err := os.Stat(s.chunkPath(d))
	return err == nil
}

// HasManifest reports whether the manifest is present in the store. An invalid
// digest can never name a stored manifest, so it reports false rather than
// touching the filesystem with an attacker-controlled path.
func (s *Store) HasManifest(d Digest) bool {
	if d.Validate() != nil {
		return false
	}
	_, err := os.Stat(s.manifestPath(d))
	return err == nil
}

// GetManifest loads and decodes a stored manifest by its digest. The digest is
// validated first so a traversal string never reaches the filesystem.
func (s *Store) GetManifest(d Digest) (Manifest, error) {
	if err := d.Validate(); err != nil {
		return Manifest{}, err
	}
	data, err := os.ReadFile(s.manifestPath(d)) //nolint:gosec // digest validated above against the strict hex allowlist
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %s: %w", d, err)
	}
	return decodeManifest(data)
}

// PutSnapshot streams each file exactly once: every chunk is hashed and, if not
// already present, written atomically (the dedup skip) in the same pass that
// collects the chunk refs. The manifest is assembled from those refs and
// written last. Single-pass keeps I/O to one read of each file (it halves I/O
// on multi-GB images) while memory stays bounded to one ChunkSize buffer. The
// returned manifest's digest is the snapshot identifier.
func (s *Store) PutSnapshot(files map[string]string, vmmVersion string, createdUnix int64) (Manifest, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]FileEntry, 0, len(names))
	for _, name := range names {
		fe, err := s.putFileChunks(name, files[name])
		if err != nil {
			return Manifest{}, fmt.Errorf("store chunks for %s: %w", name, err)
		}
		entries = append(entries, fe)
	}

	m := Manifest{
		Files:       entries,
		VMMVersion:  vmmVersion,
		CreatedUnix: createdUnix,
	}
	if err := s.writeManifest(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// putFileChunks streams the file once, hashing each chunk, writing it if not
// already present (the dedup skip), and accumulating the ordered ChunkRefs into
// a FileEntry. Memory stays bounded to one ChunkSize block.
func (s *Store) putFileChunks(name, path string) (FileEntry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileEntry{}, fmt.Errorf("stat %s: %w", path, err)
	}
	f, err := os.Open(path) //nolint:gosec // internal snapshot file
	if err != nil {
		return FileEntry{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	var chunks []ChunkRef
	buf := make([]byte, ChunkSize)
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			block := buf[:n]
			d := digestBytes(block)
			if !s.HasChunk(d) {
				if err := s.writeChunk(d, block); err != nil {
					return FileEntry{}, err
				}
			}
			chunks = append(chunks, ChunkRef{Digest: d, Size: n})
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return FileEntry{}, fmt.Errorf("read %s: %w", path, rerr)
		}
	}
	return FileEntry{Name: name, Size: info.Size(), Chunks: chunks}, nil
}

// writeChunk writes a chunk atomically (temp + rename) under its digest path.
func (s *Store) writeChunk(d Digest, data []byte) error {
	dst := s.chunkPath(d)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir chunk shard: %w", err)
	}
	return atomicWrite(dst, data)
}

// PutChunk reads chunk bytes from r, verifies they hash to the claimed digest,
// and writes them atomically into the store. A mismatch returns an error and
// nothing is written: this is the integrity gate for chunks arriving over a
// transport, where the sender is not trusted. Chunks are bounded by ChunkSize,
// so reading the whole chunk into memory is safe.
func (s *Store) PutChunk(d Digest, r io.Reader) error {
	if err := d.Validate(); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(r, ChunkSize+1))
	if err != nil {
		return fmt.Errorf("read chunk %s: %w", d, err)
	}
	if len(data) > ChunkSize {
		return fmt.Errorf("chunk %s exceeds max size %d", d, ChunkSize)
	}
	got := digestBytes(data)
	if got != d {
		return fmt.Errorf("chunk %s failed verification: got digest %s", d, got)
	}
	return s.writeChunk(d, data)
}

// PutManifest writes a manifest into the store under its own digest. It is the
// transport-side companion to PutChunk: after every referenced chunk is
// present, the manifest is stored so it becomes Materializable locally.
//
// Precondition: every chunk referenced by m must already be present in the
// store. PutManifest does not verify chunk presence; ensuring it is the
// caller's responsibility. Pull satisfies this by fetching all MissingChunks
// (each verified via PutChunk) before calling PutManifest.
func (s *Store) PutManifest(m Manifest) error {
	return s.writeManifest(m)
}

// writeManifest writes the canonical manifest atomically under its digest.
func (s *Store) writeManifest(m Manifest) error {
	return atomicWrite(s.manifestPath(m.Digest()), m.Canonical())
}

// atomicWrite writes data to a temp file in the same directory then renames it
// into place, so readers never observe a partial file.
func atomicWrite(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename temp to %s: %w", dst, err)
	}
	cleanup = false
	return nil
}

// MissingChunks returns the digests of chunks referenced by m that the store
// does not currently hold, for incremental pull. Duplicates are collapsed.
func (s *Store) MissingChunks(m Manifest) []Digest {
	seen := make(map[Digest]struct{})
	var missing []Digest
	for _, fe := range m.Files {
		for _, c := range fe.Chunks {
			if _, ok := seen[c.Digest]; ok {
				continue
			}
			seen[c.Digest] = struct{}{}
			if !s.HasChunk(c.Digest) {
				missing = append(missing, c.Digest)
			}
		}
	}
	return missing
}

// Materialize reconstructs every file in the manifest into dstDir, streaming
// and verifying each chunk's digest as it reads. A corrupted or missing chunk
// produces an error naming the offending chunk and file.
func (s *Store) Materialize(manifestDigest Digest, dstDir string) error {
	if err := manifestDigest.Validate(); err != nil {
		return err
	}
	m, err := s.GetManifest(manifestDigest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("mkdir dst: %w", err)
	}
	for _, fe := range m.Files {
		if err := s.materializeFile(fe, dstDir); err != nil {
			return err
		}
	}
	return nil
}

// materializeFile reconstructs a single file by concatenating its verified
// chunks, then updates each chunk's access time (mtime) for LRU tracking. On
// ANY error (chunk digest mismatch, missing chunk, copy or sync failure) the
// partially written destination file is removed before returning, so a verify
// failure never leaves corrupt bytes behind for a caller to consume.
func (s *Store) materializeFile(fe FileEntry, dstDir string) (err error) {
	dst := filepath.Join(dstDir, fe.Name)
	out, err := os.Create(dst) //nolint:gosec // dst derived from manifest name
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close() //nolint:errcheck // explicit Sync+Close below

	defer func() {
		if err != nil {
			_ = os.Remove(dst) //nolint:errcheck // best-effort partial-output cleanup
		}
	}()

	for _, c := range fe.Chunks {
		if err = s.copyVerifiedChunk(out, c, fe.Name); err != nil {
			return err
		}
		s.touchChunk(c.Digest)
	}

	if err = out.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err = out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

// copyVerifiedChunk reads a chunk into out while verifying its digest.
func (s *Store) copyVerifiedChunk(out io.Writer, c ChunkRef, fileName string) error {
	cp := s.chunkPath(c.Digest)
	in, err := os.Open(cp) //nolint:gosec // path derived from digest
	if err != nil {
		return fmt.Errorf("missing chunk %s for file %s: %w", c.Digest, fileName, err)
	}
	defer in.Close() //nolint:errcheck // read-only file

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		return fmt.Errorf("read chunk %s for file %s: %w", c.Digest, fileName, err)
	}
	got := Digest(hex.EncodeToString(h.Sum(nil)))
	if got != c.Digest {
		return fmt.Errorf("chunk %s for file %s failed verification: got digest %s", c.Digest, fileName, got)
	}
	return nil
}

func decodeManifest(data []byte) (Manifest, error) {
	type chunkJSON struct {
		Digest Digest `json:"digest"`
		Size   int    `json:"size"`
	}
	type fileJSON struct {
		Chunks []chunkJSON `json:"chunks"`
		Name   string      `json:"name"`
		Size   int64       `json:"size"`
	}
	type manifestJSON struct {
		CreatedUnix int64      `json:"createdUnix"`
		Files       []fileJSON `json:"files"`
		VMMVersion  string     `json:"vmmVersion"`
	}
	if len(data) == 0 {
		return Manifest{}, errEmptyManifest
	}
	var mj manifestJSON
	if err := json.Unmarshal(data, &mj); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	m := Manifest{
		VMMVersion:  mj.VMMVersion,
		CreatedUnix: mj.CreatedUnix,
	}
	for _, fj := range mj.Files {
		fe := FileEntry{Name: fj.Name, Size: fj.Size}
		for _, cj := range fj.Chunks {
			fe.Chunks = append(fe.Chunks, ChunkRef(cj))
		}
		m.Files = append(m.Files, fe)
	}
	return m, nil
}

var errEmptyManifest = errors.New("empty manifest data")

// touchChunk updates a chunk's access time (mtime) to now. mtime is the
// crash-safe LRU signal used by EvictToFit. Failures are non-fatal: a missed
// touch only makes a chunk look slightly less recently used.
func (s *Store) touchChunk(d Digest) {
	now := time.Now()
	_ = os.Chtimes(s.chunkPath(d), now, now) //nolint:errcheck // best-effort LRU update
}
