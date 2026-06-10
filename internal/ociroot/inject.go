package ociroot

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// InjectAgent prepares an extracted rootfs at destDir for booting: it installs
// the guest agent as /init (mode 0755), ensures a /bin/sh exists (copying in a
// static busybox and wiring up applets when the image lacks a shell), and
// creates the mount points the agent needs.
func InjectAgent(destDir, agentBinPath, busyboxPath string) error {
	if err := copyFile(agentBinPath, filepath.Join(destDir, "init"), 0o755); err != nil {
		return fmt.Errorf("ociroot: install init: %w", err)
	}

	if err := ensureShell(destDir, busyboxPath); err != nil {
		return err
	}

	for _, mp := range mountPoints {
		if err := os.MkdirAll(filepath.Join(destDir, mp), 0o755); err != nil {
			return fmt.Errorf("ociroot: create mount point %q: %w", mp, err)
		}
	}
	return nil
}

// ensureShell guarantees destDir/bin/sh resolves to a usable shell. If the
// image already ships one it is left untouched. Otherwise a busybox is copied
// in and common applets are symlinked to it. With no shell and no busybox the
// rootfs cannot run init commands, so we fail loudly.
func ensureShell(destDir, busyboxPath string) error {
	binDir := filepath.Join(destDir, "bin")
	shPath := filepath.Join(binDir, "sh")

	if _, err := os.Lstat(shPath); err == nil {
		return nil // shell already present
	}

	if busyboxPath == "" {
		return fmt.Errorf("ociroot: image has no /bin/sh and no busybox provided")
	}

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("ociroot: create bin dir: %w", err)
	}
	if err := copyFile(busyboxPath, filepath.Join(binDir, "busybox"), 0o755); err != nil {
		return fmt.Errorf("ociroot: install busybox: %w", err)
	}

	for _, applet := range []string{"sh", "echo", "cat", "ls"} {
		link := filepath.Join(binDir, applet)
		if _, err := os.Lstat(link); err == nil {
			continue // do not clobber an applet the image already has
		}
		if err := os.Symlink("busybox", link); err != nil {
			return fmt.Errorf("ociroot: symlink applet %q: %w", applet, err)
		}
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(perm); err != nil {
		return err
	}
	return out.Close()
}
