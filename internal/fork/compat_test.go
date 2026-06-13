package fork

import (
	"errors"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/snapcompat"
)

// goodEnv is the host environment the test engine claims to run.
func goodEnv() snapcompat.Environment {
	return snapcompat.Environment{
		FormatVersions: []int{cas.CurrentSnapshotFormatVersion},
		VMMVersion:     "v1.15.0",
		CPUModel:       "Intel(R) Xeon(R) CPU @ 2.20GHz",
		KernelVersion:  "6.1.0",
	}
}

// newCompatEngine builds a minimal Engine for exercising the Fork-time
// compatibility gate without launching Firecracker or requiring KVM. It records
// a template manifest stamped with recordMeta, then returns an engine whose
// detected env is engineEnv.
func newCompatEngine(t *testing.T, allowIncompatible bool, engineEnv snapcompat.Environment, recordMeta cas.Metadata) (*Engine, string) {
	t.Helper()
	id := "py"
	dataDir := writeFakeTemplate(t, id)
	store := newTestStore(t, dataDir)
	if _, err := recordTemplateDigest(store, dataDir, id, recordMeta); err != nil {
		t.Fatalf("recordTemplateDigest: %v", err)
	}
	e := &Engine{
		dataDir:            dataDir,
		casStore:           store,
		allowIncompatible:  allowIncompatible,
		env:                engineEnv,
		unverifiedWarned:   make(map[string]struct{}),
		incompatibleWarned: make(map[string]struct{}),
		templateDigests:    make(map[string]cas.Digest),
	}
	return e, id
}

// matchingMeta produces the metadata a same-host build would stamp.
func matchingMeta() cas.Metadata {
	env := goodEnv()
	return cas.Metadata{
		SnapshotFormatVersion: cas.CurrentSnapshotFormatVersion,
		VMMVersion:            env.VMMVersion,
		CPUModel:              env.CPUModel,
		KernelVersion:         env.KernelVersion,
	}
}

func TestEnsureCompatibleMatchingProceeds(t *testing.T) {
	e, id := newCompatEngine(t, false, goodEnv(), matchingMeta())
	if err := e.ensureCompatible(id); err != nil {
		t.Fatalf("expected matching snapshot to be compatible, got %v", err)
	}
}

func TestEnsureCompatibleRefusesVMMMismatch(t *testing.T) {
	meta := matchingMeta()
	meta.VMMVersion = "v1.10.0"
	e, id := newCompatEngine(t, false, goodEnv(), meta)
	err := e.ensureCompatible(id)
	if err == nil {
		t.Fatal("expected refusal on VMM mismatch")
	}
	if !errors.Is(err, snapcompat.ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	for _, want := range []string{"v1.10.0", "v1.15.0", "--allow-incompatible-snapshots"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestEnsureCompatibleRefusesCPUMismatch(t *testing.T) {
	meta := matchingMeta()
	meta.CPUModel = "AMD EPYC 7B12"
	e, id := newCompatEngine(t, false, goodEnv(), meta)
	err := e.ensureCompatible(id)
	if err == nil || !errors.Is(err, snapcompat.ErrIncompatible) {
		t.Fatalf("expected CPU mismatch refusal, got %v", err)
	}
}

func TestEnsureCompatibleRefusesFormatMismatch(t *testing.T) {
	// Format version 0 (pre-contract): the fake template recorded with zero
	// format version must be refused.
	meta := matchingMeta()
	meta.SnapshotFormatVersion = 0
	e, id := newCompatEngine(t, false, goodEnv(), meta)
	err := e.ensureCompatible(id)
	if err == nil || !errors.Is(err, snapcompat.ErrIncompatible) {
		t.Fatalf("expected format mismatch refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "predates") {
		t.Fatalf("expected pre-contract message, got %v", err)
	}
}

func TestEnsureCompatibleAllowIncompatibleDowngradesToWarning(t *testing.T) {
	meta := matchingMeta()
	meta.VMMVersion = "v1.10.0"
	e, id := newCompatEngine(t, true, goodEnv(), meta)
	if err := e.ensureCompatible(id); err != nil {
		t.Fatalf("expected allow-incompatible to proceed, got %v", err)
	}
}

func TestEnsureCompatibleNoManifestIsCompatible(t *testing.T) {
	// A snapshot id with no recorded digest (e.g. a live-fork checkpoint) has
	// nothing to check and must not be refused.
	dataDir := writeFakeTemplate(t, "py")
	e := &Engine{
		dataDir:            dataDir,
		casStore:           newTestStore(t, dataDir),
		env:                goodEnv(),
		incompatibleWarned: make(map[string]struct{}),
	}
	if err := e.ensureCompatible("py"); err != nil {
		t.Fatalf("expected no-manifest snapshot to be compatible, got %v", err)
	}
}
