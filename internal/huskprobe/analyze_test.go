package huskprobe

import "testing"

const mib = int64(1024 * 1024)

// TestAnalyzeCoWSurvives models the load-bearing case: four VMs forked from one
// ~256 MiB snapshot. Each shares the same clean snapshot resident set and has
// only a small per-VM private-dirty divergence. The honest physical footprint
// must be ~256 MiB + sum(dirty), far below the ~4x256 MiB naive sum, CoW must
// survive, and dirty must be attributed per VM.
func TestAnalyzeCoWSurvives(t *testing.T) {
	shared := 256 * mib
	dirty := []int64{4 * mib, 6 * mib, 5 * mib, 8 * mib}
	samples := make([]VMSample, 0, len(dirty))
	for i, d := range dirty {
		samples = append(samples, VMSample{
			PID:          1000 + i,
			Rss:          shared + d, // restored snapshot clean set + own dirty
			PrivateDirty: d,
			Pss:          shared/int64(len(dirty)) + d, // clean set split across sharers
			SharedClean:  shared,
			MemcgCurrent: shared + d,
		})
	}

	r := Analyze(samples)

	if !r.CoWSurvives {
		t.Errorf("CoWSurvives = false, want true (AggregatePhysical=%d NaiveSum=%d SharedResident=%d)",
			r.AggregatePhysical, r.NaiveSum, r.SharedResident)
	}
	if !r.DirtyPerVM {
		t.Error("DirtyPerVM = false, want true (every VM has its own private-dirty)")
	}
	if r.SharedResident != shared {
		t.Errorf("SharedResident = %d, want %d", r.SharedResident, shared)
	}
	var sumDirty int64
	for _, d := range dirty {
		sumDirty += d
	}
	wantAgg := shared + sumDirty
	if r.AggregatePhysical != wantAgg {
		t.Errorf("AggregatePhysical = %d, want %d", r.AggregatePhysical, wantAgg)
	}
	wantNaive := 4*shared + sumDirty
	if r.NaiveSum != wantNaive {
		t.Errorf("NaiveSum = %d, want %d", r.NaiveSum, wantNaive)
	}
	if r.CoWSavings != wantNaive-wantAgg {
		t.Errorf("CoWSavings = %d, want %d", r.CoWSavings, wantNaive-wantAgg)
	}
	// Four forks of one snapshot save three whole shared sets versus naive.
	if r.CoWSavings != 3*shared {
		t.Errorf("CoWSavings = %d, want %d (three shared sets of %d)", r.CoWSavings, 3*shared, shared)
	}
}

// TestAnalyzeNoSharing is the degenerate case: each VM's RSS is entirely its
// own private-dirty (no shared clean set). CoW must NOT survive and the honest
// footprint must equal the naive sum.
func TestAnalyzeNoSharing(t *testing.T) {
	samples := []VMSample{
		{PID: 1, Rss: 100 * mib, PrivateDirty: 100 * mib},
		{PID: 2, Rss: 120 * mib, PrivateDirty: 120 * mib},
		{PID: 3, Rss: 90 * mib, PrivateDirty: 90 * mib},
	}

	r := Analyze(samples)

	if r.CoWSurvives {
		t.Error("CoWSurvives = true, want false (no shared clean set)")
	}
	if r.SharedResident != 0 {
		t.Errorf("SharedResident = %d, want 0", r.SharedResident)
	}
	if r.AggregatePhysical != r.NaiveSum {
		t.Errorf("AggregatePhysical = %d, want NaiveSum %d", r.AggregatePhysical, r.NaiveSum)
	}
	if r.CoWSavings != 0 {
		t.Errorf("CoWSavings = %d, want 0", r.CoWSavings)
	}
	if !r.DirtyPerVM {
		t.Error("DirtyPerVM = false, want true (every VM has dirty here)")
	}
}

// TestAnalyzeSingleVMNeverSurvives: a single VM cannot demonstrate sharing.
func TestAnalyzeSingleVM(t *testing.T) {
	r := Analyze([]VMSample{{PID: 1, Rss: 256 * mib, PrivateDirty: 4 * mib}})
	if r.CoWSurvives {
		t.Error("CoWSurvives = true for a single VM, want false")
	}
}

// TestAnalyzeEmpty: no samples yields the zero report.
func TestAnalyzeEmpty(t *testing.T) {
	r := Analyze(nil)
	if r.CoWSurvives || r.DirtyPerVM || r.NaiveSum != 0 || r.AggregatePhysical != 0 {
		t.Errorf("empty Analyze not zero: %+v", r)
	}
}

// TestAnalyzeDirtyNotPerVM: one VM with zero private-dirty flips DirtyPerVM off
// while sharing can still survive.
func TestAnalyzeDirtyNotPerVM(t *testing.T) {
	shared := 256 * mib
	samples := []VMSample{
		{PID: 1, Rss: shared + 4*mib, PrivateDirty: 4 * mib},
		{PID: 2, Rss: shared, PrivateDirty: 0},
	}
	r := Analyze(samples)
	if r.DirtyPerVM {
		t.Error("DirtyPerVM = true, want false (one VM has zero dirty)")
	}
}

func TestParseSmapsRollup(t *testing.T) {
	text := `55a0b0000000-7ffeffffffff ---p 00000000 00:00 0                          [rollup]
Rss:              262144 kB
Pss:               65536 kB
Shared_Clean:     258048 kB
Shared_Dirty:          0 kB
Private_Clean:         0 kB
Private_Dirty:      4096 kB
Referenced:       262144 kB
`
	pss, rss, sharedClean, privateDirty := ParseSmapsRollup(text)
	if pss != 65536*1024 {
		t.Errorf("pss = %d, want %d", pss, 65536*1024)
	}
	if rss != 262144*1024 {
		t.Errorf("rss = %d, want %d", rss, 262144*1024)
	}
	if sharedClean != 258048*1024 {
		t.Errorf("sharedClean = %d, want %d", sharedClean, 258048*1024)
	}
	if privateDirty != 4096*1024 {
		t.Errorf("privateDirty = %d, want %d", privateDirty, 4096*1024)
	}
}

func TestParseSmapsRollupMissingFields(t *testing.T) {
	pss, rss, sharedClean, privateDirty := ParseSmapsRollup("Rss:  100 kB\n")
	if rss != 100*1024 {
		t.Errorf("rss = %d, want %d", rss, 100*1024)
	}
	if pss != 0 || sharedClean != 0 || privateDirty != 0 {
		t.Errorf("missing fields not zero: pss=%d sharedClean=%d privateDirty=%d", pss, sharedClean, privateDirty)
	}
}

func TestParseMemcgStat(t *testing.T) {
	text := `anon 134217728
file 4194304
kernel_stack 65536
slab 1048576
`
	file, anon := ParseMemcgStat(text)
	if file != 4194304 {
		t.Errorf("file = %d, want %d", file, 4194304)
	}
	if anon != 134217728 {
		t.Errorf("anon = %d, want %d", anon, 134217728)
	}
}

func TestParseMemcgStatMissing(t *testing.T) {
	file, anon := ParseMemcgStat("slab 1048576\n")
	if file != 0 || anon != 0 {
		t.Errorf("missing fields not zero: file=%d anon=%d", file, anon)
	}
}
