package husk

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/snapcompat"
)

// snapshotVerifier re-verifies a snapshot at activate time BEFORE it is loaded
// into the VMM. It is the husk-path equivalent of the forkd verify-on-load gate
// (issues #9 digest verify and #32 snapcompat): it fails closed so a snapshot
// tampered on the node disk after forkd's build-time verification, or one that
// is incompatible with THIS node (wrong Firecracker version, CPU, or snapshot
// format), is refused and never restored.
//
// It returns nil only when the snapshot passes BOTH the integrity check (the
// loaded mem+vmstate re-hash to the recorded manifest, and the manifest's own
// digest matches the recorded content address the controller passed) AND the
// compatibility check. Tests inject a no-op verifier through Options; the
// production seam is productionVerifier below.
type snapshotVerifier func(req ActivateRequest) error

// verifyConfig is the production verifier's inputs, captured once at New from
// the stub's configuration: the path the CAS manifest is mounted at, the
// detected host environment to check compatibility against, and whether the
// development escape hatch is set.
type verifyConfig struct {
	// manifestPath is the on-disk path of the recorded CAS manifest, mounted
	// into the husk pod read-only. The verifier decodes it, binds it to the
	// request's ExpectedDigest, re-hashes the loaded snapshot files against it,
	// and runs the compatibility check on its recorded environment.
	manifestPath string
	// env is the host environment detected once at stub start (Firecracker
	// version, CPU model, kernel, supported formats). snapcompat.Check refuses a
	// snapshot whose recorded producing environment this node cannot restore.
	env snapcompat.Environment
	// allowUnverified is the development escape hatch, mirroring forkd's
	// --allow-unverified-snapshots / --allow-incompatible-snapshots. Default
	// false: a missing ExpectedDigest or manifest, or a failed check, refuses the
	// activate. When true the verifier logs a loud warning and proceeds, for the
	// CI latency/handshake phases that drive a real bench snapshot with no digest.
	allowUnverified bool
}

// productionVerifier builds the activate-time snapshot verifier from cfg. It
// fails closed: any missing input (no ExpectedDigest, no mounted manifest), any
// integrity mismatch, or any compatibility refusal returns an error and the
// stub does NOT load the snapshot, UNLESS allowUnverified is set (development
// only), in which case it warns once and proceeds.
func productionVerifier(cfg verifyConfig) snapshotVerifier {
	return func(req ActivateRequest) error {
		err := verifySnapshot(cfg, req)
		if err != nil && cfg.allowUnverified {
			fmt.Fprintf(os.Stderr, "husk: WARNING activating UNVERIFIED snapshot at %s: %v; integrity/compatibility is NOT enforced because --allow-unverified-snapshots is set (development only)\n", req.SnapshotDir, err)
			return nil
		}
		return err
	}
}

// verifySnapshot performs the two fail-closed checks against the snapshot at
// req.SnapshotDir: integrity (manifest digest + on-disk file re-hash) and
// compatibility (snapcompat). It is the husk mirror of forkd's ensureVerified +
// ensureCompatible, sharing the cas chunking/hashing primitives so the two paths
// cannot drift.
func verifySnapshot(cfg verifyConfig, req ActivateRequest) error {
	if req.ExpectedDigest == "" {
		return fmt.Errorf("refusing to activate snapshot at %s: no expected digest supplied; the controller must pass the template's recorded manifest digest (set --allow-unverified-snapshots to override in development)", req.SnapshotDir)
	}
	want := cas.Digest(req.ExpectedDigest)
	if err := want.Validate(); err != nil {
		return fmt.Errorf("refusing to activate snapshot at %s: %w", req.SnapshotDir, err)
	}

	// Load the recorded manifest mounted into the pod and bind it to the recorded
	// content address: the manifest's own digest MUST equal the expected digest,
	// so a tampered manifest cannot vouch for tampered files.
	data, err := os.ReadFile(cfg.manifestPath) //nolint:gosec // manifestPath is the operator-configured mount path
	if err != nil {
		return fmt.Errorf("refusing to activate snapshot at %s: read recorded manifest: %w", req.SnapshotDir, err)
	}
	m, err := cas.DecodeManifest(data)
	if err != nil {
		return fmt.Errorf("refusing to activate snapshot at %s: decode recorded manifest: %w", req.SnapshotDir, err)
	}
	if got := m.Digest(); got != want {
		return fmt.Errorf("refusing to activate snapshot at %s: recorded manifest digest %s does not match expected %s; the mounted manifest does not match the template the controller activated", req.SnapshotDir, got, want)
	}

	// Integrity: re-hash the loaded snapshot files (mem+vmstate) and compare to
	// the manifest. The rootfs is not mounted in a husk pod, so only the loaded
	// subset is re-hashed; the manifest digest binding above pins the rest.
	files := map[string]string{
		"mem":     filepath.Join(req.SnapshotDir, "mem"),
		"vmstate": filepath.Join(req.SnapshotDir, "vmstate"),
	}
	if err := cas.VerifyFilesAgainstManifest(m, files); err != nil {
		return fmt.Errorf("refusing to activate snapshot at %s: %w", req.SnapshotDir, err)
	}

	// Compatibility: the snapshot's recorded producing environment must be
	// restorable on this node (mirror forkd ensureCompatible / #32).
	if err := snapcompat.Check(m, cfg.env); err != nil {
		return fmt.Errorf("refusing to activate snapshot at %s: %w", req.SnapshotDir, err)
	}
	return nil
}
