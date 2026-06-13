// Package workspace holds the host-side hydrate/dehydrate helpers that move a
// sandbox's /workspace tree between a running guest and the content-addressed
// store (internal/cas). Dehydrate captures the guest workspace into a CAS
// manifest (a committed WorkspaceRevision's content); Hydrate restores a CAS
// manifest into a guest workspace. Both speak the bulk tar transfer
// (vsock.Client.TarDir / UntarDir) over a small VsockTransport seam so the
// controller and tests can drive them without a real VM.
//
// The transfer never captures guest secrets: secret values live only in the
// guest's in-memory configured env (the guest agent's configuredEnv), never on
// disk under /workspace, and the caller additionally passes an explicit
// excludePaths allowlist of paths to strip from the captured tree. A revision is
// content only.
package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/paperclipinc/mitos/internal/cas"
)

// WorkspacePath is the guest directory the workspace transfer captures and
// restores. It mirrors the guest agent's workspace allowlist root.
const WorkspacePath = "/workspace"

// VsockTransport is the slice of the guest agent transport the transfer helpers
// need: the bulk tar ops. The production implementation is *vsock.Client; tests
// and callers that wire their own path supply any value with these two methods.
type VsockTransport interface {
	TarDir(path string) ([]byte, error)
	UntarDir(path string, tar []byte) error
}

// Dehydrate captures the guest's /workspace into a CAS manifest and returns its
// digest. It tars the workspace over vsock, strips any excludePaths member
// (defense in depth against a secret or token file that should never enter a
// revision), filters the surviving members down to the capturePaths subtrees (a
// nil capturePaths captures the whole workspace, the slice-2 default), unpacks
// them to a temp dir, and stores them via store.PutSnapshot. The returned digest
// is the content identifier a committed WorkspaceRevision records. An unchanged
// tree dehydrates to the same digest (PutSnapshot is content-addressed and
// deterministic in the file set).
func Dehydrate(ctx context.Context, agent VsockTransport, store *cas.Store, excludePaths, capturePaths []string) (cas.Digest, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := agent.TarDir(WorkspacePath)
	if err != nil {
		return "", fmt.Errorf("tar guest workspace: %w", err)
	}

	tmp, err := os.MkdirTemp("", "ws-dehydrate-*")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup

	files, err := unpackTar(data, tmp, excludePaths)
	if err != nil {
		return "", fmt.Errorf("unpack workspace tar: %w", err)
	}
	files = FilterFiles(files, capturePaths)

	m, err := store.PutSnapshot(files, cas.Metadata{})
	if err != nil {
		return "", fmt.Errorf("store workspace snapshot: %w", err)
	}
	return m.Digest(), nil
}

// Hydrate restores a CAS manifest into the guest's /workspace. It materializes
// the manifest's files to a temp dir, tars them, and sends the tar to the guest
// over vsock (UntarDir), which sanitizes every member against traversal before
// writing. The manifest's flat logical file names are the workspace-relative
// paths Dehydrate captured, so the round trip is byte-identical.
func Hydrate(ctx context.Context, agent VsockTransport, store *cas.Store, manifest cas.Digest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("hydrate: %w", err)
	}

	tmp, err := os.MkdirTemp("", "ws-hydrate-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup

	// The manifest's logical file names are workspace-relative paths (possibly
	// nested), so materialize each entry to its sanitized path under tmp with the
	// parent directories created. store.Materialize writes flat names only;
	// MaterializeFileTo creates the per-file parent dir and verifies each chunk.
	m, err := store.GetManifest(manifest)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", manifest, err)
	}
	for _, fe := range m.Files {
		dst, err := safeJoin(tmp, filepath.Clean(fe.Name))
		if err != nil {
			return fmt.Errorf("hydrate manifest %s: %w", manifest, err)
		}
		if err := store.MaterializeFileTo(manifest, fe.Name, dst); err != nil {
			return fmt.Errorf("materialize %s from %s: %w", fe.Name, manifest, err)
		}
	}

	data, err := tarTree(tmp)
	if err != nil {
		return fmt.Errorf("tar materialized workspace: %w", err)
	}
	if err := agent.UntarDir(WorkspacePath, data); err != nil {
		return fmt.Errorf("untar into guest workspace: %w", err)
	}
	return nil
}

// unpackTar extracts the regular-file members of a tar into dstDir (sanitizing
// each member against traversal) and returns a name -> hostpath map suitable for
// cas.Store.PutSnapshot. The map keys are the workspace-relative member names, so
// the manifest's logical names equal the workspace paths and Hydrate restores
// them in place. A member whose name matches excludePaths (exactly, or as a
// prefix directory) is dropped so secrets never enter a revision.
func unpackTar(data []byte, dstDir string, excludePaths []string) (map[string]string, error) {
	excl := normalizeExcludes(excludePaths)
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // directories are implied by their files; skip non-regular
		}
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		if excluded(name, excl) {
			continue
		}
		target, err := safeJoin(dstDir, name)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := writeFileFrom(target, tr); err != nil {
			return nil, err
		}
		files[name] = target
	}
	return files, nil
}

// tarTree tars every regular file under root, relative to root, into an in-memory
// buffer. It is the inverse of cas materialize for the hydrate path: the manifest
// names equal the workspace-relative paths, so the produced tar restores them in
// place on the guest.
func tarTree(root string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	walkErr := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     filepath.ToSlash(rel),
			Mode:     int64(fi.Mode().Perm()),
			Size:     fi.Size(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path) //nolint:gosec // path walked from the controller-owned temp dir
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // read-only file
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		return f.Close()
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileFrom streams r into a new file at target.
func writeFileFrom(target string, r io.Reader) error {
	f, err := os.Create(target) //nolint:gosec // target validated by safeJoin
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close() //nolint:errcheck // already failing
		return err
	}
	return f.Close()
}

// normalizeExcludes cleans the exclude paths into workspace-relative slash form
// so they match the tar member names. An absolute exclude like
// "/workspace/.git-credentials" becomes ".git-credentials"; a leading
// "/workspace/" prefix is stripped. Empty entries are dropped.
func normalizeExcludes(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = filepath.ToSlash(filepath.Clean(p))
		p = strings.TrimPrefix(p, WorkspacePath+"/")
		p = strings.TrimPrefix(p, "/")
		if p == "" || p == "." {
			continue
		}
		out = append(out, p)
	}
	return out
}

// excluded reports whether name matches any exclude entry exactly or sits under
// an exclude directory.
func excluded(name string, excludes []string) bool {
	for _, e := range excludes {
		if name == e || strings.HasPrefix(name, e+"/") {
			return true
		}
	}
	return false
}

// safeJoin joins a workspace-relative member name onto dst, rejecting an absolute
// path or a ".." escape. It mirrors the guest agent's extraction barrier so the
// host side is defended even though the bytes here come from the guest tar.
func safeJoin(dst, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty tar member name")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("refusing absolute tar member %q", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing tar member %q: escapes target directory", name)
	}
	joined := filepath.Join(dst, clean)
	if joined != dst && !strings.HasPrefix(joined, dst+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing tar member %q: resolves outside target directory", name)
	}
	return joined, nil
}
