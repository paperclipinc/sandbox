//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// workspaceRoot is the only directory tree the bulk tar transfer is allowed to
// touch. TarDir and UntarDir refuse any path that is not workspaceRoot or a path
// under it: the guest never tars / and never extracts outside /workspace, so the
// host workspace transfer can never reach the guest's secret/token state (which
// lives in the in-memory configured env, never on disk under /workspace) or
// elsewhere on the rootfs.
const workspaceRoot = "/workspace"

// pathAllowed reports whether p is workspaceRoot or a descendant of it, after
// cleaning. This is the allowlist gate for both tar (read) and untar (write).
func pathAllowed(p string) bool {
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	if clean == workspaceRoot {
		return true
	}
	return strings.HasPrefix(clean, workspaceRoot+string(os.PathSeparator))
}

// handleTarDir tars the requested directory and returns the tar bytes. The path
// must be inside the workspace allowlist; the resulting tar is bounded by
// vsock.MaxTarBytes so a runaway workspace cannot force an unbounded response.
func handleTarDir(req *vsock.TarDirRequest) vsock.Response {
	if !pathAllowed(req.Path) {
		return vsock.Response{OK: false, Error: fmt.Sprintf("tar_dir: path %q is outside the workspace transfer allowlist", req.Path)}
	}
	data, err := tarDir(req.Path)
	if err != nil {
		return vsock.Response{OK: false, Error: fmt.Sprintf("tar_dir: %v", err)}
	}
	if len(data) > vsock.MaxTarBytes {
		return vsock.Response{OK: false, Error: fmt.Sprintf("tar_dir: tar size %d exceeds max %d", len(data), vsock.MaxTarBytes)}
	}
	return vsock.Response{OK: true, TarDir: &vsock.TarDirResponse{Tar: data}}
}

// tarDir walks dir and writes a tar of its regular files and directories,
// relative to dir, into a bounded in-memory buffer. Symlinks and other
// non-regular entries are skipped: a symlink could otherwise re-introduce an
// escape on extraction, and the workspace transfer only needs file content. A
// missing dir yields an empty tar (an empty workspace dehydrates cleanly).
func tarDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := tw.Close(); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // do not emit the root itself; members are relative
		}
		// Only regular files and directories cross the transfer. Symlinks,
		// devices, sockets, and fifos are skipped so extraction can never be
		// tricked into writing outside the target via a restored symlink.
		mode := fi.Mode()
		switch {
		case mode.IsDir():
			hdr := &tar.Header{
				Name:     filepath.ToSlash(rel) + "/",
				Mode:     int64(mode.Perm()),
				Typeflag: tar.TypeDir,
			}
			return tw.WriteHeader(hdr)
		case mode.IsRegular():
			hdr := &tar.Header{
				Name:     filepath.ToSlash(rel),
				Mode:     int64(mode.Perm()),
				Size:     fi.Size(),
				Typeflag: tar.TypeReg,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path) //nolint:gosec // path is under the workspace allowlist, walked from dir
			if err != nil {
				return err
			}
			defer f.Close() //nolint:errcheck // read-only file
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
			return f.Close()
		default:
			return nil // skip non-regular, non-dir entries
		}
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// handleUntarDir extracts the request tar into the requested directory. The
// target must be inside the workspace allowlist, the tar is bounded by
// vsock.MaxTarBytes, and every member name is sanitized against traversal before
// any write.
func handleUntarDir(req *vsock.UntarDirRequest) vsock.Response {
	if !pathAllowed(req.Path) {
		return vsock.Response{OK: false, Error: fmt.Sprintf("untar_dir: path %q is outside the workspace transfer allowlist", req.Path)}
	}
	if len(req.Tar) > vsock.MaxTarBytes {
		return vsock.Response{OK: false, Error: fmt.Sprintf("untar_dir: tar size %d exceeds max %d", len(req.Tar), vsock.MaxTarBytes)}
	}
	if err := untarDir(req.Path, req.Tar); err != nil {
		return vsock.Response{OK: false, Error: fmt.Sprintf("untar_dir: %v", err)}
	}
	return vsock.Response{OK: true}
}

// untarDir extracts tar into dst. Each member is run through safeJoin, which
// rejects an absolute path or any ".." escape outside dst, so a malicious tar
// (e.g. a "../../etc/passwd" member) is refused and nothing is written for it.
// Only regular files and directories are materialized; any other type is
// rejected (a symlink member could otherwise point outside the target).
func untarDir(dst string, data []byte) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeRegular(target, tr, fs.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("refusing tar member %q with unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

// writeRegular streams a single tar member to target. Reads are bounded by
// vsock.MaxTarBytes so a header that lies about size cannot drive an unbounded
// write.
func writeRegular(target string, r io.Reader, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) //nolint:gosec // target validated by safeJoin to be under dst
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(r, vsock.MaxTarBytes)); err != nil {
		_ = f.Close() //nolint:errcheck // already failing
		return err
	}
	return f.Close()
}

// safeJoin joins a tar member name onto dst and rejects any name that is
// absolute or that, after cleaning, escapes dst via "..". It is the traversal
// barrier for extraction. The returned path is guaranteed to be dst or a
// descendant of dst.
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
	// Defense in depth: the joined path must still be within dst.
	if joined != dst && !strings.HasPrefix(joined, dst+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing tar member %q: resolves outside target directory", name)
	}
	return joined, nil
}
