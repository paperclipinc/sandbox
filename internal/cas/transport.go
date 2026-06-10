package cas

import (
	"context"
	"fmt"
	"io"
)

// Transport is the read side of a remote content-addressed store. It is the
// minimal surface a node needs to incrementally pull a snapshot from a peer:
// fetch a manifest, ask which chunks a peer holds, and fetch chunk bytes. The
// node-to-node orchestration (peer discovery, which node pulls from which) is
// out of scope here and tracked in #14.
type Transport interface {
	// HasChunks reports, for each requested digest, whether the remote holds
	// that chunk. The returned map has an entry for every requested digest.
	HasChunks(ctx context.Context, digests []Digest) (map[Digest]bool, error)
	// GetChunk streams the bytes of a single chunk. The caller closes the
	// reader. The bytes are not trusted until their digest is verified.
	GetChunk(ctx context.Context, d Digest) (io.ReadCloser, error)
	// GetManifest fetches and decodes a remote manifest by its digest.
	GetManifest(ctx context.Context, d Digest) (Manifest, error)
}

// Pull incrementally fetches a snapshot from remote into local. It fetches the
// manifest, computes the set of chunks local is missing, fetches only those
// from remote, verifies each chunk's digest on receipt before storing it, and
// finally writes the manifest locally. After Pull returns nil the manifest is
// Materializable from local. Only the delta is transferred: chunks local
// already holds (shared with earlier snapshots) are never fetched.
func Pull(ctx context.Context, local *Store, remote Transport, manifestDigest Digest) error {
	if err := manifestDigest.Validate(); err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	m, err := remote.GetManifest(ctx, manifestDigest)
	if err != nil {
		return fmt.Errorf("get remote manifest %s: %w", manifestDigest, err)
	}
	if got := m.Digest(); got != manifestDigest {
		return fmt.Errorf("remote manifest %s failed verification: got digest %s", manifestDigest, got)
	}

	for _, d := range local.MissingChunks(m) {
		if err := pullChunk(ctx, local, remote, d); err != nil {
			return err
		}
	}

	if err := local.PutManifest(m); err != nil {
		return fmt.Errorf("store manifest %s: %w", manifestDigest, err)
	}
	return nil
}

// pullChunk fetches one chunk and stores it, verifying its digest on receipt.
func pullChunk(ctx context.Context, local *Store, remote Transport, d Digest) error {
	rc, err := remote.GetChunk(ctx, d)
	if err != nil {
		return fmt.Errorf("get remote chunk %s: %w", d, err)
	}
	defer rc.Close() //nolint:errcheck // read-only stream
	if err := local.PutChunk(d, rc); err != nil {
		return fmt.Errorf("store pulled chunk %s: %w", d, err)
	}
	return nil
}
