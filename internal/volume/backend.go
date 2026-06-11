// Package volume prepares per-sandbox block-device backing files on a forkd
// node. Each fork policy maps to a different preparation strategy:
//
//   - Fresh:    create an empty ext4 image sized from the spec.
//   - Snapshot: reflink-copy the template image so the fork gets instant
//     copy-on-write on btrfs/xfs (a full copy elsewhere).
//   - Share:    attach the template image read-only with no copy.
//   - Clone:    full byte-for-byte copy of the template image.
//
// The actual Firecracker drive attach happens elsewhere; this package only
// produces the host paths and their read-only flag.
package volume

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ForkPolicy names a backing-image preparation strategy. The values mirror the
// API ForkPolicy strings so a controller can pass them through unchanged.
type ForkPolicy string

const (
	ForkPolicyFresh    ForkPolicy = "Fresh"
	ForkPolicyShare    ForkPolicy = "Share"
	ForkPolicyClone    ForkPolicy = "Clone"
	ForkPolicySnapshot ForkPolicy = "Snapshot"
)

// Spec describes one volume to prepare for a sandbox or fork.
type Spec struct {
	Name      string
	SizeMB    int
	MountPath string
	ReadOnly  bool
	Policy    ForkPolicy
}

// Prepared is the result of preparing a Spec: the host-side backing file, the
// in-guest mount path, and whether the attach must be read-only.
type Prepared struct {
	Name      string
	HostPath  string
	MountPath string
	ReadOnly  bool
}

// Backend prepares volume backing files under root. runner is injectable so
// tests can record argv instead of running real mkfs or cp.
type Backend struct {
	root   string
	runner func(argv []string) error
}

// New returns a Backend rooted at root whose runner execs the given argv.
func New(root string) *Backend {
	return &Backend{
		root:   root,
		runner: execRunner,
	}
}

// NewWithRunner returns a Backend rooted at root that runs argv through runner
// instead of execRunner. It exists so callers in other packages (the fork
// engine's tests) can record mkfs/cp invocations without running real
// subprocesses. Production code uses New.
func NewWithRunner(root string, runner func(argv []string) error) *Backend {
	return &Backend{root: root, runner: runner}
}

// Root reports the directory the backend roots its backing files under.
func (b *Backend) Root() string {
	return b.root
}

// execRunner runs argv as a subprocess, surfacing combined output on failure.
func execRunner(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("volume: empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is built from validated specs, not user shell input.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("volume: %s failed: %w: %s", argv[0], err, string(out))
	}
	return nil
}

// volumePath is the backing file for volume name under the sandbox id. The id
// and name come from validated CRD fields, so they are safe to log.
func (b *Backend) volumePath(sandboxID, name string) string {
	return filepath.Join(b.volumesDir(sandboxID), name+".ext4")
}

func (b *Backend) volumesDir(sandboxID string) string {
	return filepath.Join(b.root, "sandboxes", sandboxID, "volumes")
}

// TemplateVolumePath is the seed backing file for volume name under template
// templateID. The template build writes one empty (or seeded) ext4 here per
// template volume so it can be baked into the snapshot AND used as the
// reflink/copy source for Snapshot and Clone forks. The id and name come from
// validated CRD fields, so they are safe to log.
func (b *Backend) TemplateVolumePath(templateID, name string) string {
	return filepath.Join(b.root, "templates", templateID, "volumes", name+".ext4")
}

// FreshTemplate creates the seed backing for a template volume at
// TemplateVolumePath: an empty ext4 of the spec size. It mirrors Fresh but
// targets the template-scoped path so the snapshot can bake the block device
// and forks can reflink/copy from it. Returns the seed host path.
func (b *Backend) FreshTemplate(spec Spec, templateID string) (string, error) {
	dst := b.TemplateVolumePath(templateID, spec.Name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("template volume %s: mkdir: %w", spec.Name, err)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("template volume %s: create backing file: %w", spec.Name, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("template volume %s: close backing file: %w", spec.Name, err)
	}
	size := fmt.Sprintf("%dM", spec.SizeMB)
	if err := b.runner([]string{"mkfs.ext4", "-F", "-q", dst, size}); err != nil {
		return "", fmt.Errorf("template volume %s: mkfs: %w", spec.Name, err)
	}
	return dst, nil
}

// Fresh creates an empty ext4 image sized from the spec.
func (b *Backend) Fresh(spec Spec, sandboxID string) (Prepared, error) {
	dst := b.volumePath(sandboxID, spec.Name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Prepared{}, fmt.Errorf("volume %s: mkdir: %w", spec.Name, err)
	}
	// Create (truncate) the backing file before mkfs writes the filesystem.
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return Prepared{}, fmt.Errorf("volume %s: create backing file: %w", spec.Name, err)
	}
	if err := f.Close(); err != nil {
		return Prepared{}, fmt.Errorf("volume %s: close backing file: %w", spec.Name, err)
	}
	size := fmt.Sprintf("%dM", spec.SizeMB)
	if err := b.runner([]string{"mkfs.ext4", "-F", "-q", dst, size}); err != nil {
		return Prepared{}, fmt.Errorf("volume %s: mkfs: %w", spec.Name, err)
	}
	return Prepared{Name: spec.Name, HostPath: dst, MountPath: spec.MountPath, ReadOnly: spec.ReadOnly}, nil
}

// Snapshot reflink-copies sourcePath to a per-fork backing file. It first tries
// cp --reflink=always for a true copy-on-write clone; on filesystems without
// reflink support (anything but btrfs/xfs) that fails, so it falls back to
// cp --reflink=auto, which performs a full copy. The fallback is logged by the
// caller through the returned error path only on hard failure; a successful
// fallback is silent here.
func (b *Backend) Snapshot(spec Spec, sandboxID, sourcePath string) (Prepared, error) {
	dst := b.volumePath(sandboxID, spec.Name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Prepared{}, fmt.Errorf("volume %s: mkdir: %w", spec.Name, err)
	}
	err := b.runner([]string{"cp", "--reflink=always", sourcePath, dst})
	if err != nil {
		// Reflink unavailable on this filesystem; fall back to a full copy.
		if err := b.runner([]string{"cp", "--reflink=auto", sourcePath, dst}); err != nil {
			return Prepared{}, fmt.Errorf("volume %s: snapshot copy: %w", spec.Name, err)
		}
	}
	return Prepared{Name: spec.Name, HostPath: dst, MountPath: spec.MountPath, ReadOnly: spec.ReadOnly}, nil
}

// Share returns the source image as a read-only attach with no copy. All forks
// share the same backing file.
func (b *Backend) Share(spec Spec, _, sourcePath string) (Prepared, error) {
	return Prepared{Name: spec.Name, HostPath: sourcePath, MountPath: spec.MountPath, ReadOnly: true}, nil
}

// Clone makes a full byte-for-byte copy of sourcePath into a per-fork file.
func (b *Backend) Clone(spec Spec, sandboxID, sourcePath string) (Prepared, error) {
	dst := b.volumePath(sandboxID, spec.Name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Prepared{}, fmt.Errorf("volume %s: mkdir: %w", spec.Name, err)
	}
	if err := b.runner([]string{"cp", sourcePath, dst}); err != nil {
		return Prepared{}, fmt.Errorf("volume %s: clone copy: %w", spec.Name, err)
	}
	return Prepared{Name: spec.Name, HostPath: dst, MountPath: spec.MountPath, ReadOnly: spec.ReadOnly}, nil
}

// Cleanup removes the volumes directory for a sandbox. Missing is not an error.
func (b *Backend) Cleanup(sandboxID string) error {
	if err := os.RemoveAll(b.volumesDir(sandboxID)); err != nil {
		return fmt.Errorf("volume cleanup for %s: %w", sandboxID, err)
	}
	return nil
}
