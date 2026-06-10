// Package cas implements a content-addressed store for VM memory and disk
// snapshots. Files are split into fixed-size chunks keyed by their sha256
// digest, deduplicated across snapshots, and reconstructed with per-chunk
// integrity verification.
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
)

// ChunkSize is the fixed chunk size used for content-addressed splitting.
// 4 MiB balances dedup granularity against per-chunk overhead for the
// multi-GB memory images this store is built for.
const ChunkSize = 4 << 20

// Digest is a lowercase hex sha256 string. Digests are safe to log.
type Digest string

// digestPattern matches a sha256 digest: exactly 64 lowercase hex characters.
var digestPattern = regexp.MustCompile("^[a-f0-9]{64}$")

// Validate reports whether d is a well-formed sha256 digest. A Digest can
// arrive from an untrusted source (an HTTP request path, a remote manifest),
// and is used to build on-disk paths; validating it against this strict
// allowlist is the barrier that blocks path traversal (e.g. "../../etc/passwd")
// and avoids the short-string slice panic in path construction. Callers that
// build a path or open a file from a Digest must Validate it first.
func (d Digest) Validate() error {
	if !digestPattern.MatchString(string(d)) {
		return fmt.Errorf("invalid digest %q: must be 64 lowercase hex chars", string(d))
	}
	return nil
}

// ChunkRef identifies a single chunk by its digest and byte length.
type ChunkRef struct {
	Digest Digest
	Size   int
}

// digestBytes returns the lowercase hex sha256 digest of b.
func digestBytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest(hex.EncodeToString(sum[:]))
}

// chunkFile splits the file at path into ChunkSize chunks, returning a
// ChunkRef per chunk in order. It streams the file in ChunkSize blocks so
// memory stays bounded regardless of file size. An empty file yields no
// chunks.
func chunkFile(path string) ([]ChunkRef, error) {
	f, err := os.Open(path) //nolint:gosec // path is an internal snapshot file
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	var chunks []ChunkRef
	buf := make([]byte, ChunkSize)
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			block := buf[:n]
			chunks = append(chunks, ChunkRef{
				Digest: digestBytes(block),
				Size:   n,
			})
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	return chunks, nil
}
