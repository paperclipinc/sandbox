package fork

import (
	"fmt"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/metering"
)

func TestHostMemTotalBytes(t *testing.T) {
	meminfo := `MemTotal:       16384000 kB
MemFree:         8000000 kB
MemAvailable:   12000000 kB
`
	got, err := hostMemTotalBytes(func() (string, error) { return meminfo, nil })
	if err != nil {
		t.Fatalf("hostMemTotalBytes: %v", err)
	}
	want := int64(16384000) * 1024
	if got != want {
		t.Fatalf("MemTotal: got %d want %d", got, want)
	}
}

func TestHostMemTotalBytesMissing(t *testing.T) {
	if _, err := hostMemTotalBytes(func() (string, error) {
		return "MemFree: 100 kB\n", nil
	}); err == nil {
		t.Fatal("expected error when MemTotal is absent")
	}
}

func TestHostMemTotalBytesReaderError(t *testing.T) {
	if _, err := hostMemTotalBytes(func() (string, error) {
		return "", fmt.Errorf("boom")
	}); err == nil {
		t.Fatal("expected error propagated from reader")
	}
}

func TestTemplateEstimatesFromReport(t *testing.T) {
	// Two forks of template "alpha" (unique 100 + 300, shared-once 256), one
	// fork of "beta" (unique 50, shared-once 64).
	report := metering.Aggregate([]metering.Sample{
		{ID: "a1", Template: "alpha", MemoryUnique: 100, MemoryShared: 256},
		{ID: "a2", Template: "alpha", MemoryUnique: 300, MemoryShared: 256},
		{ID: "b1", Template: "beta", MemoryUnique: 50, MemoryShared: 64},
	})
	digests := map[string]string{"alpha": "sha256:aaa"}

	ests := templateEstimatesFromReport(report, digests)
	byID := make(map[string]TemplateEstimate, len(ests))
	for _, e := range ests {
		byID[e.TemplateID] = e
	}

	alpha := byID["alpha"]
	if alpha.ForkCount != 2 {
		t.Errorf("alpha ForkCount: got %d want 2", alpha.ForkCount)
	}
	if alpha.SharedOnceBytes != 256 {
		t.Errorf("alpha SharedOnceBytes: got %d want 256", alpha.SharedOnceBytes)
	}
	if alpha.AvgForkUniqueBytes != 200 { // (100+300)/2
		t.Errorf("alpha AvgForkUniqueBytes: got %d want 200", alpha.AvgForkUniqueBytes)
	}
	if alpha.SnapshotDigest != "sha256:aaa" {
		t.Errorf("alpha SnapshotDigest: got %q want sha256:aaa", alpha.SnapshotDigest)
	}

	beta := byID["beta"]
	if beta.AvgForkUniqueBytes != 50 {
		t.Errorf("beta AvgForkUniqueBytes: got %d want 50", beta.AvgForkUniqueBytes)
	}
}

func TestGetCapacityReportsMemoryTotalAndEstimates(t *testing.T) {
	dataDir := t.TempDir()
	tmplMgr := firecracker.NewTemplateManager("fc", "vmlinux", dataDir, firecracker.JailerConfig{})
	const meminfo = "MemTotal:       16384000 kB\nMemFree: 1000 kB\n"
	const reserve = int64(2 * 1024 * 1024 * 1024)

	e := &Engine{
		sandboxes:       make(map[string]*Sandbox),
		templateMgr:     tmplMgr,
		templateDigests: make(map[string]cas.Digest),
		memReserveBytes: reserve,
		meminfoReader:   func() (string, error) { return meminfo, nil },
	}
	e.sandboxes["s1"] = &Sandbox{ID: "s1", TemplateID: "alpha", MemoryUnique: 100, MemoryShared: 256}
	e.sandboxes["s2"] = &Sandbox{ID: "s2", TemplateID: "alpha", MemoryUnique: 300, MemoryShared: 256}

	cap := e.GetCapacity()

	wantTotal := int64(16384000)*1024 - reserve
	if cap.MemoryTotal != wantTotal {
		t.Errorf("MemoryTotal: got %d want %d", cap.MemoryTotal, wantTotal)
	}
	if len(cap.TemplateEstimates) != 1 {
		t.Fatalf("TemplateEstimates: got %d want 1", len(cap.TemplateEstimates))
	}
	est := cap.TemplateEstimates[0]
	if est.TemplateID != "alpha" || est.SharedOnceBytes != 256 || est.AvgForkUniqueBytes != 200 || est.ForkCount != 2 {
		t.Errorf("estimate: got %+v", est)
	}
}

func TestGetCapacityMemoryTotalZeroWhenReaderFails(t *testing.T) {
	dataDir := t.TempDir()
	tmplMgr := firecracker.NewTemplateManager("fc", "vmlinux", dataDir, firecracker.JailerConfig{})
	e := &Engine{
		sandboxes:       make(map[string]*Sandbox),
		templateMgr:     tmplMgr,
		templateDigests: make(map[string]cas.Digest),
		memReserveBytes: 1024,
		meminfoReader:   func() (string, error) { return "", fmt.Errorf("no /proc on darwin") },
	}
	if got := e.GetCapacity().MemoryTotal; got != 0 {
		t.Errorf("MemoryTotal with failing reader: got %d want 0", got)
	}
}

func TestTemplateEstimatesFloorForZeroForks(t *testing.T) {
	// A template with no forks yet (no samples) does not appear in the report;
	// templateEstimateFloor supplies the floor a cold-only template uses.
	if got := templateEstimateFloor().AvgForkUniqueBytes; got != defaultForkUniqueFloorBytes {
		t.Errorf("floor AvgForkUniqueBytes: got %d want %d", got, defaultForkUniqueFloorBytes)
	}
}
