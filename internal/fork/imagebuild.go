package fork

import (
	"context"
	"fmt"
	"os"

	"github.com/paperclipinc/mitos/internal/ociroot"
)

// buildRootfsFromImage turns an OCI image reference into a bootable ext4 rootfs
// at outPath. It pulls the image, extracts its layers into a temp directory,
// injects the guest agent as /init (and a shell if the image lacks one), sizes
// the filesystem from the extracted content, and builds the ext4 image. The
// temp directory is always cleaned up. agentBin is required; busyboxBin is
// optional and only used when the image ships no /bin/sh.
func buildRootfsFromImage(ctx context.Context, ref, outPath, agentBin, busyboxBin string) error {
	if agentBin == "" {
		return fmt.Errorf("building rootfs from image %q requires a guest agent binary (--agent-bin); none configured", ref)
	}

	img, err := ociroot.PullImage(ctx, ref)
	if err != nil {
		// Marked so callers (and CI) can distinguish a registry/network pull
		// flake, which is retryable and not a pipeline defect, from a genuine
		// build/boot/init failure downstream.
		return fmt.Errorf("PULL_FAILED: pull image %q: %w", ref, err)
	}

	tmpDir, err := os.MkdirTemp("", "ociroot-extract-")
	if err != nil {
		return fmt.Errorf("create extract temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := ociroot.ExtractImage(img, tmpDir); err != nil {
		return fmt.Errorf("extract image %q: %w", ref, err)
	}

	if err := ociroot.InjectAgent(tmpDir, agentBin, busyboxBin); err != nil {
		return fmt.Errorf("inject agent into rootfs for %q: %w", ref, err)
	}

	sizeMB, err := ociroot.DirSizeMB(tmpDir)
	if err != nil {
		return fmt.Errorf("size rootfs for %q: %w", ref, err)
	}

	if err := (&ociroot.Ext4Builder{}).BuildExt4(tmpDir, outPath, sizeMB); err != nil {
		return fmt.Errorf("build rootfs ext4 for %q: %w", ref, err)
	}
	return nil
}
