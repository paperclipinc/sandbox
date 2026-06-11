// Package huskprobe holds the pure measurement math behind the husk-probe
// command (cmd/husk-probe). The probe forks N microVMs from one template
// snapshot, places each Firecracker process in its own cgroup v2 memory
// controller, and samples both the kernel's per-process smaps_rollup and the
// per-memcg accounting. This package turns those raw samples into a CoW-aware
// report; it is deliberately free of any cgroup, KVM, or syscall dependency so
// the aggregation and parsing can be unit-tested on any platform.
//
// The model proves the load-bearing claim of husk pods: forks of one template
// restore the SAME snapshot with MAP_PRIVATE, so the clean snapshot page set is
// physically shared across all forks and only each fork's private-dirty pages
// are charged per VM. Charging every VM its full RSS (NaiveSum) therefore wildly
// over-counts; the honest physical footprint counts the shared clean set ONCE.
package huskprobe

import (
	"strconv"
	"strings"
)

// VMSample is one forked microVM's measured footprint. PID is the Firecracker
// process. The Pss/Rss/SharedClean/PrivateDirty fields come from that PID's
// /proc/<pid>/smaps_rollup (bytes). MemcgCurrent is the VM's cgroup v2
// memory.current and MemcgFile/MemcgAnon are the "file"/"anon" lines of its
// memory.stat (bytes). The memcg fields cross-check the smaps view: the kernel
// charges each shared clean page to exactly one memcg, so memory.current is not
// itself a CoW-aware total, which is precisely why the smaps-derived split is
// the source of truth for the report.
type VMSample struct {
	PID          int
	Pss          int64
	Rss          int64
	SharedClean  int64
	PrivateDirty int64
	MemcgCurrent int64
	MemcgFile    int64
	MemcgAnon    int64
}

// Report is the CoW-aware rollup over a set of VM samples.
//
// NaiveSum is what a non-CoW-aware accountant charges: every VM's full Rss,
// with no sharing. TotalPrivateDirty is the sum of every VM's own private-dirty
// pages (each VM's divergence from the snapshot, charged to that VM alone).
// SharedResident is the shared snapshot clean resident set counted ONCE.
// AggregatePhysical is the honest physical footprint = SharedResident +
// TotalPrivateDirty. CoWSavings is NaiveSum - AggregatePhysical.
//
// CoWSurvives is true when sharing materially lowered the physical footprint
// (see Analyze for the threshold) for at least two VMs. DirtyPerVM is true when
// every VM has its own private-dirty pages, i.e. the per-VM divergence is
// attributed to each VM rather than collapsed.
type Report struct {
	VMs               []VMSample
	NaiveSum          int64
	AggregatePhysical int64
	SharedResident    int64
	TotalPrivateDirty int64
	CoWSavings        int64
	CoWSurvives       bool
	DirtyPerVM        bool
}

// Analyze folds per-VM samples into a CoW-aware Report.
//
// SharedResident representative choice: every VM restored the SAME snapshot
// with MAP_PRIVATE, so the clean (non-dirty) resident portion of each VM,
// (Rss - PrivateDirty), is approximately equal across VMs and refers to the
// SAME physical pages. We therefore count that shared clean set a single time
// and take the MAX over VMs of (Rss - PrivateDirty) as the representative. The
// max is the conservative choice: it never under-states the shared set, so it
// never over-states CoWSavings.
func Analyze(samples []VMSample) Report {
	r := Report{VMs: samples}
	if len(samples) == 0 {
		return r
	}

	r.DirtyPerVM = true
	for _, s := range samples {
		r.NaiveSum += s.Rss
		r.TotalPrivateDirty += s.PrivateDirty
		clean := s.Rss - s.PrivateDirty
		if clean < 0 {
			clean = 0
		}
		if clean > r.SharedResident {
			r.SharedResident = clean
		}
		if s.PrivateDirty <= 0 {
			r.DirtyPerVM = false
		}
	}

	r.AggregatePhysical = r.SharedResident + r.TotalPrivateDirty
	r.CoWSavings = r.NaiveSum - r.AggregatePhysical

	// CoWSurvives: sharing is material only when it saved at least one whole
	// shared set across the N>=2 VMs, i.e. the honest physical footprint is at
	// least one SharedResident below the naive sum. With no sharing every VM is
	// all-private-dirty, SharedResident is ~0, and AggregatePhysical == NaiveSum,
	// so this is false. A single VM can never demonstrate sharing.
	if len(samples) >= 2 && r.SharedResident > 0 {
		r.CoWSurvives = r.AggregatePhysical <= r.NaiveSum-r.SharedResident
	}

	return r
}

// ParseSmapsRollup extracts the Pss, Rss, Shared_Clean, and Private_Dirty
// values from the text of a /proc/<pid>/smaps_rollup file. The kernel reports
// those lines in kB; the returned values are bytes. Missing fields yield 0,
// matching the engine's own readMemoryStats parsing (internal/fork/engine.go).
func ParseSmapsRollup(text string) (pss, rss, sharedClean, privateDirty int64) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		b := kb * 1024
		switch fields[0] {
		case "Pss:":
			pss = b
		case "Rss:":
			rss = b
		case "Shared_Clean:":
			sharedClean = b
		case "Private_Dirty:":
			privateDirty = b
		}
	}
	return pss, rss, sharedClean, privateDirty
}

// ParseMemcgStat extracts the "file" and "anon" byte counts from the text of a
// cgroup v2 memory.stat file. Those lines are already in bytes. Missing fields
// yield 0.
func ParseMemcgStat(text string) (file, anon int64) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "file":
			file = v
		case "anon":
			anon = v
		}
	}
	return file, anon
}
