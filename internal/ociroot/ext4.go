package ociroot

import (
	"fmt"
	"io/fs"
	"math"
	"os/exec"
	"path/filepath"
)

// minRootfsMB is the floor for a generated rootfs. Even a near empty image
// needs room for the agent, busybox, and filesystem metadata.
const minRootfsMB = 64

// rootfsHeadroom multiplies the measured content size to leave slack for
// filesystem overhead and a little runtime growth.
const rootfsHeadroom = 1.4

// Runner executes an argv vector. It is injectable so tests can record the
// command without invoking real tooling.
type Runner func(argv []string) error

// execRunner is the production Runner: it execs the command and surfaces stderr.
func execRunner(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("ociroot: empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv built from fixed flags and validated paths
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ociroot: %s failed: %w: %s", argv[0], err, out)
	}
	return nil
}

// Ext4Builder builds ext4 images via an injectable Runner.
type Ext4Builder struct {
	Runner Runner
}

// BuildExt4 creates an ext4 image at outPath of sizeMB megabytes, seeded with
// the contents of srcDir, by invoking mkfs.ext4 through the builder's Runner.
func (b *Ext4Builder) BuildExt4(srcDir, outPath string, sizeMB int) error {
	runner := b.Runner
	if runner == nil {
		runner = execRunner
	}
	argv := []string{
		"mkfs.ext4",
		"-F",
		"-q",
		"-d", srcDir,
		outPath,
		fmt.Sprintf("%dM", sizeMB),
	}
	if err := runner(argv); err != nil {
		return fmt.Errorf("ociroot: build ext4: %w", err)
	}
	return nil
}

// BuildExt4WithRunner is a convenience wrapper for callers that just want to
// pass a runner without holding a builder.
func BuildExt4WithRunner(srcDir, outPath string, sizeMB int, runner Runner) error {
	return (&Ext4Builder{Runner: runner}).BuildExt4(srcDir, outPath, sizeMB)
}

// DirSizeMB walks dir summing regular file sizes and returns a rootfs size in
// megabytes with headroom applied, never below minRootfsMB.
func DirSizeMB(dir string) (int, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("ociroot: measure dir: %w", err)
	}

	const mib = 1024 * 1024
	withHeadroom := float64(total) * rootfsHeadroom
	sizeMB := int(math.Ceil(withHeadroom / mib))
	if sizeMB < minRootfsMB {
		sizeMB = minRootfsMB
	}
	return sizeMB, nil
}
