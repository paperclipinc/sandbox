// Package ociroot pulls OCI images and flattens them into a directory tree
// and an ext4 rootfs image suitable for booting inside a microVM.
package ociroot

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// mountPoints are the directories the guest agent expects to exist so it can
// mount the standard pseudo filesystems and the workspace volume.
var mountPoints = []string{"proc", "sys", "dev", "tmp", "run", "workspace"}

// secureJoin joins entryName onto destDir and guarantees the result stays
// inside destDir. It rejects absolute paths and any cleaned path that escapes
// destDir via "..". Image tars are untrusted input, so this is the single
// chokepoint every extracted path must pass through.
func secureJoin(destDir, entryName string) (string, error) {
	if filepath.IsAbs(entryName) {
		return "", fmt.Errorf("ociroot: absolute path entry rejected: %q", entryName)
	}
	joined := filepath.Join(destDir, entryName)
	// Clean destDir for a stable prefix comparison.
	cleanDest := filepath.Clean(destDir)
	rel, err := filepath.Rel(cleanDest, joined)
	if err != nil {
		return "", fmt.Errorf("ociroot: cannot relativize entry %q: %w", entryName, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("ociroot: path traversal entry rejected: %q", entryName)
	}
	return joined, nil
}

// symlinkStaysInside reports whether a symlink at linkPath pointing to target
// would resolve to a location inside destDir. Relative targets are resolved
// against the link's own directory; absolute targets are rejected because they
// would point at the host filesystem when the rootfs is mounted elsewhere.
func symlinkStaysInside(destDir, linkPath, target string) bool {
	if filepath.IsAbs(target) {
		return false
	}
	resolved := filepath.Join(filepath.Dir(linkPath), target)
	cleanDest := filepath.Clean(destDir)
	rel, err := filepath.Rel(cleanDest, resolved)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}

// ExtractImage flattens img with mutate.Extract and untars the result into
// destDir, preserving file modes, symlinks, and best-effort uid/gid. Every
// entry path is validated through secureJoin so malicious tars cannot write
// outside destDir.
func ExtractImage(img v1.Image, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("ociroot: create dest dir: %w", err)
	}

	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()

	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("ociroot: read tar entry: %w", err)
		}
		if err := extractEntry(destDir, tr, hdr); err != nil {
			return err
		}
	}
	return nil
}

func extractEntry(destDir string, tr *tar.Reader, hdr *tar.Header) error {
	target, err := secureJoin(destDir, hdr.Name)
	if err != nil {
		return err
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()); err != nil {
			return fmt.Errorf("ociroot: mkdir %q: %w", hdr.Name, err)
		}
		applyOwnership(target, hdr)

	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociroot: mkdir parent of %q: %w", hdr.Name, err)
		}
		if err := writeRegular(target, tr, os.FileMode(hdr.Mode).Perm()); err != nil {
			return fmt.Errorf("ociroot: write file %q: %w", hdr.Name, err)
		}
		applyOwnership(target, hdr)

	case tar.TypeSymlink:
		if !symlinkStaysInside(destDir, target, hdr.Linkname) {
			return fmt.Errorf("ociroot: symlink %q -> %q escapes dest dir", hdr.Name, hdr.Linkname)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociroot: mkdir parent of symlink %q: %w", hdr.Name, err)
		}
		_ = os.Remove(target)
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("ociroot: symlink %q: %w", hdr.Name, err)
		}

	case tar.TypeLink:
		linkTarget, err := secureJoin(destDir, hdr.Linkname)
		if err != nil {
			return fmt.Errorf("ociroot: hardlink %q target: %w", hdr.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociroot: mkdir parent of hardlink %q: %w", hdr.Name, err)
		}
		_ = os.Remove(target)
		if err := os.Link(linkTarget, target); err != nil {
			return fmt.Errorf("ociroot: hardlink %q: %w", hdr.Name, err)
		}

	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Device, char, and fifo nodes need privilege we may not have and are
		// not needed for the rootfs use case. Log the skip with the safe path.
		log.Printf("ociroot: skipping special node %q (type %d)", hdr.Name, hdr.Typeflag)

	default:
		log.Printf("ociroot: skipping unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
	}
	return nil
}

func writeRegular(target string, tr io.Reader, perm os.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, tr); err != nil {
		return err
	}
	// OpenFile honors umask, so set the requested perm explicitly.
	if err := f.Chmod(perm); err != nil {
		return err
	}
	return f.Close()
}

// applyOwnership sets uid/gid best-effort. Failures (for example when running
// unprivileged) are intentionally ignored: the rootfs is still usable.
func applyOwnership(path string, hdr *tar.Header) {
	_ = os.Lchown(path, hdr.Uid, hdr.Gid)
}
