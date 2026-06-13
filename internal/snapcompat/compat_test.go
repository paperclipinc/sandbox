package snapcompat

import (
	"errors"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
)

func goodEnv() Environment {
	return Environment{
		FormatVersions: []int{cas.CurrentSnapshotFormatVersion},
		VMMVersion:     "1.15.0",
		CPUModel:       "Intel(R) Xeon(R) CPU @ 2.20GHz",
		KernelVersion:  "6.1.0",
	}
}

func goodManifest() cas.Manifest {
	return cas.Manifest{
		SnapshotFormatVersion: cas.CurrentSnapshotFormatVersion,
		VMMVersion:            "1.15.0",
		CPUModel:              "Intel(R) Xeon(R) CPU @ 2.20GHz",
		KernelVersion:         "6.1.0",
	}
}

func TestCheckMatchingEnvReturnsNil(t *testing.T) {
	if err := Check(goodManifest(), goodEnv()); err != nil {
		t.Fatalf("expected nil for matching env, got %v", err)
	}
}

func TestCheckFormatVersionUnsupported(t *testing.T) {
	m := goodManifest()
	m.SnapshotFormatVersion = 99
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for unsupported format version")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"99", "rebuild"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

func TestCheckZeroFormatVersionPreContract(t *testing.T) {
	m := goodManifest()
	m.SnapshotFormatVersion = 0
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for zero format version")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"predates", "rebuild"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("pre-contract message %q missing %q", msg, want)
		}
	}
}

func TestCheckVMMMismatch(t *testing.T) {
	m := goodManifest()
	m.VMMVersion = "1.10.0"
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for VMM mismatch")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	// Both sides and remediation must be named.
	for _, want := range []string{"1.10.0", "1.15.0", "Firecracker", "rebuild"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("VMM message %q missing %q", msg, want)
		}
	}
}

func TestCheckCPUMismatch(t *testing.T) {
	m := goodManifest()
	m.CPUModel = "AMD EPYC 7B12"
	err := Check(m, goodEnv())
	if err == nil {
		t.Fatal("expected error for CPU mismatch")
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("expected ErrIncompatible, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"AMD EPYC 7B12", "Xeon", "CPU template", "schedule"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("CPU message %q missing %q", msg, want)
		}
	}
}

func TestCheckKernelMismatchInformationalOnly(t *testing.T) {
	// Kernel differs but format+VMM+CPU all match: v1 treats this as
	// informational, so Check must return nil.
	m := goodManifest()
	m.KernelVersion = "5.10.0"
	if err := Check(m, goodEnv()); err != nil {
		t.Fatalf("kernel mismatch must not be fatal in v1, got %v", err)
	}
}

func TestCheckOrderFormatBeforeVMM(t *testing.T) {
	// Both format and VMM mismatch: format is reported first.
	m := goodManifest()
	m.SnapshotFormatVersion = 7
	m.VMMVersion = "0.0.1"
	err := Check(m, goodEnv())
	if err == nil || !strings.Contains(err.Error(), "format version") {
		t.Fatalf("expected format mismatch first, got %v", err)
	}
}
