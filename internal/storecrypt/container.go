package storecrypt

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// scopeIDPattern constrains every caller-supplied scope id that storecrypt
// embeds in a host filesystem path (the image file) and a dm-crypt mapper name.
// It mirrors the daemon's sandbox id allowlist: no dots and no separators, so a
// validated scope id can never introduce a `..` segment or an extra path
// element. Validation runs BEFORE any image file is created or any cryptsetup
// command is built.
var scopeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// Runner executes a single command. argv[0] is the binary; the key, when one is
// needed, is supplied on stdin (never in argv) so it is invisible in /proc and
// in any logging of the command line. A nil/empty stdin means no input is fed.
type Runner func(ctx context.Context, argv []string, stdin []byte) error

// Manager owns the per-scope LUKS containers under root. The runner is injected
// so tests can record the commands and the key-on-stdin discipline without
// executing real cryptsetup; the production default runner execs the command
// with stdin piped.
type Manager struct {
	// root is the directory whose enc/ subdirectory holds the per-scope image
	// files (<root>/enc/<scopeID>.img).
	root string
	// mountBase is reserved for callers that want a default mount location; the
	// methods take an explicit mountPoint, so it is informational only.
	mountBase string
	runner    Runner
}

// New returns a Manager rooted at root that drives cryptsetup through runner.
// mountBase is recorded for callers but the per-call mountPoint is authoritative.
func New(root, mountBase string, runner Runner) *Manager {
	return &Manager{root: root, mountBase: mountBase, runner: runner}
}

// DefaultRunner is the production runner: it execs argv[0] with the remaining
// args and pipes stdin into the child process, so the key (when passed) reaches
// cryptsetup via --key-file - and never appears in argv. Command output is
// captured into the error on failure; the key is never part of argv or the
// error.
func DefaultRunner(ctx context.Context, argv []string, stdin []byte) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // fixed cryptsetup/mount argv built from a validated scope id and host paths
	if len(stdin) > 0 {
		cmd.Stdin = newSecretReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, string(out))
	}
	return nil
}

// secretReader feeds stdin bytes to a child process exactly once. It is a thin
// wrapper so the key bytes are streamed rather than embedded in a struct that
// might be formatted; the bytes themselves are never logged here.
type secretReader struct {
	b   []byte
	pos int
}

func newSecretReader(b []byte) *secretReader { return &secretReader{b: b} }

func (r *secretReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// imgPath returns the LUKS image file for a scope. The caller must have already
// validated scopeID.
func (m *Manager) imgPath(scopeID string) string {
	return filepath.Join(m.root, "enc", scopeID+".img")
}

// mapperName returns the dm-crypt mapper name for a scope (the device appears at
// /dev/mapper/<mapperName>). The caller must have already validated scopeID.
func mapperName(scopeID string) string { return "mitos-" + scopeID }

// validateScopeID rejects a scope id that could escape the image directory or
// the mapper namespace, BEFORE any file is touched or any command is built.
func validateScopeID(scopeID string) error {
	if !scopeIDPattern.MatchString(scopeID) {
		return fmt.Errorf("invalid scope id %q: must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes)", scopeID)
	}
	return nil
}

// Create provisions a fresh LUKS2 container for scopeID, formats an ext4
// filesystem inside it, and mounts it at mountPoint. The sequence is:
// fallocate the image, luksFormat (key on stdin), luksOpen (key on stdin),
// mkfs.ext4 on the mapper device, then mount. On a mid-sequence failure it rolls
// back what was opened (luksClose) and removes the freshly created image so a
// partial container does not leak. The key is passed to cryptsetup ONLY on
// stdin, never in argv.
func (m *Manager) Create(ctx context.Context, scopeID string, key Key, sizeBytes int64, mountPoint string) error {
	if err := validateScopeID(scopeID); err != nil {
		return err
	}
	encDir := filepath.Join(m.root, "enc")
	if err := os.MkdirAll(encDir, 0o700); err != nil {
		return fmt.Errorf("create enc dir: %w", err)
	}
	img := m.imgPath(scopeID)
	dev := "/dev/mapper/" + mapperName(scopeID)

	if err := allocateImage(img, sizeBytes); err != nil {
		return fmt.Errorf("allocate image for scope %s: %w", scopeID, err)
	}

	if err := m.runner(ctx, []string{"cryptsetup", "luksFormat", "--type", "luks2", "--batch-mode", "--key-file", "-", img}, key); err != nil {
		_ = os.Remove(img)
		return fmt.Errorf("luksFormat scope %s: %w", scopeID, err)
	}

	if err := m.runner(ctx, []string{"cryptsetup", "luksOpen", "--key-file", "-", img, mapperName(scopeID)}, key); err != nil {
		_ = os.Remove(img)
		return fmt.Errorf("luksOpen scope %s: %w", scopeID, err)
	}

	if err := m.runner(ctx, []string{"mkfs.ext4", "-q", dev}, nil); err != nil {
		_ = m.runner(ctx, []string{"cryptsetup", "luksClose", mapperName(scopeID)}, nil)
		_ = os.Remove(img)
		return fmt.Errorf("mkfs.ext4 scope %s: %w", scopeID, err)
	}

	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		_ = m.runner(ctx, []string{"cryptsetup", "luksClose", mapperName(scopeID)}, nil)
		_ = os.Remove(img)
		return fmt.Errorf("create mount point for scope %s: %w", scopeID, err)
	}

	if err := m.runner(ctx, []string{"mount", dev, mountPoint}, nil); err != nil {
		_ = m.runner(ctx, []string{"cryptsetup", "luksClose", mapperName(scopeID)}, nil)
		_ = os.Remove(img)
		return fmt.Errorf("mount scope %s: %w", scopeID, err)
	}
	return nil
}

// Open unlocks an existing container for scopeID (luksOpen with the key on
// stdin) and mounts it at mountPoint. It is used to reattach a container that
// already exists, e.g. after a daemon restart or to fork from an encrypted
// template whose container is not currently open. On a mount failure it rolls
// back the luksOpen. The key is passed only on stdin, never in argv.
func (m *Manager) Open(ctx context.Context, scopeID string, key Key, mountPoint string) error {
	if err := validateScopeID(scopeID); err != nil {
		return err
	}
	img := m.imgPath(scopeID)
	dev := "/dev/mapper/" + mapperName(scopeID)

	if err := m.runner(ctx, []string{"cryptsetup", "luksOpen", "--key-file", "-", img, mapperName(scopeID)}, key); err != nil {
		return fmt.Errorf("luksOpen scope %s: %w", scopeID, err)
	}
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		_ = m.runner(ctx, []string{"cryptsetup", "luksClose", mapperName(scopeID)}, nil)
		return fmt.Errorf("create mount point for scope %s: %w", scopeID, err)
	}
	if err := m.runner(ctx, []string{"mount", dev, mountPoint}, nil); err != nil {
		_ = m.runner(ctx, []string{"cryptsetup", "luksClose", mapperName(scopeID)}, nil)
		return fmt.Errorf("mount scope %s: %w", scopeID, err)
	}
	return nil
}

// Close unmounts mountPoint and closes the dm-crypt device for scopeID. It is
// tolerant of an already-unmounted or already-closed container: both steps are
// best effort and a not-mounted / not-open condition is not an error. The
// Manager holds no key, so there is nothing to zeroize here; luksClose needs
// only the mapper name.
func (m *Manager) Close(ctx context.Context, scopeID, mountPoint string) error {
	if err := validateScopeID(scopeID); err != nil {
		return err
	}
	_ = m.runner(ctx, []string{"umount", mountPoint}, nil)
	_ = m.runner(ctx, []string{"cryptsetup", "luksClose", mapperName(scopeID)}, nil)
	return nil
}

// Shred crypto-shreds the container for scopeID: best-effort umount + luksClose,
// then luksErase to wipe the LUKS keyslots (after which the ciphertext is
// unrecoverable even with the key) and removal of the image file. It is
// idempotent: a missing image or an already-closed device is not an error, so
// repeated GC of the same scope is safe. No key is needed.
func (m *Manager) Shred(ctx context.Context, scopeID, mountPoint string) error {
	if err := validateScopeID(scopeID); err != nil {
		return err
	}
	img := m.imgPath(scopeID)
	_ = m.runner(ctx, []string{"umount", mountPoint}, nil)
	_ = m.runner(ctx, []string{"cryptsetup", "luksClose", mapperName(scopeID)}, nil)

	if _, err := os.Stat(img); err != nil {
		// No image: nothing to erase. Idempotent: a missing container is fine.
		return nil
	}
	if err := m.runner(ctx, []string{"cryptsetup", "luksErase", "--batch-mode", img}, nil); err != nil {
		return fmt.Errorf("luksErase scope %s: %w", scopeID, err)
	}
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove image for scope %s: %w", scopeID, err)
	}
	return nil
}

// allocateImage creates (or truncates) the image file to sizeBytes. The file is
// created with restrictive permissions; the bytes are sparse until written by
// luksFormat and the filesystem.
func allocateImage(path string, sizeBytes int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		return err
	}
	return nil
}
