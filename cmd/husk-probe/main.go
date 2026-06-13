//go:build linux

// Command husk-probe is the load-bearing proof that husk pods share memory.
//
// It forks N microVMs from one template snapshot via the real KVM-backed fork
// engine, places each fork's Firecracker process in its own cgroup v2 memory
// controller, lets them settle, then samples both the per-process smaps_rollup
// and the per-memcg accounting. huskprobe.Analyze turns those samples into a
// CoW-aware report: every fork restores the SAME snapshot with MAP_PRIVATE, so
// the clean snapshot resident set is physically shared and counted ONCE, while
// each fork's private-dirty divergence is charged to that fork's own memcg.
//
// Exit code contract (read by the CI phase, plan Task 2):
//
//   - 0: a clean measurement was taken. The verdict (CoWSurvives, DirtyPerVM)
//     is in the printed JSON; the probe always reports, it never gates. The CI
//     asserts on the JSON so a DESIGN failure is visible as data, not as a
//     probe crash that could be mistaken for a SETUP problem.
//   - 1: a SETUP problem prevented measurement (no writable cgroup2, the memory
//     controller is not delegable, the template is missing, /dev/kvm absent, a
//     fork failed). These are prefixed "SETUP:" on stderr so they are never
//     confused with a DESIGN result.
//
// The driver mirrors cmd/bench: it imports internal/fork and drives the engine
// in-process with the same --template/--data-dir/--firecracker/--kernel flags
// and a zero jailer config.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/huskprobe"
)

type config struct {
	template    string
	dataDir     string
	firecracker string
	kernel      string
	forks       int
	cgroupRoot  string
	settleMs    int
	jsonPath    string
}

func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("husk-probe", flag.ContinueOnError)
	var cfg config
	fs.StringVar(&cfg.template, "template", "", "template (snapshot) id to fork from")
	fs.StringVar(&cfg.dataDir, "data-dir", "/var/lib/mitos", "data directory holding template snapshots")
	fs.StringVar(&cfg.firecracker, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	fs.StringVar(&cfg.kernel, "kernel", "/var/lib/mitos/vmlinux", "guest kernel path")
	fs.IntVar(&cfg.forks, "forks", 4, "number of sandboxes to fork from one template")
	fs.StringVar(&cfg.cgroupRoot, "cgroup-root", "/sys/fs/cgroup/husk-probe", "writable cgroup2 path under which per-VM memcgs are created")
	fs.IntVar(&cfg.settleMs, "settle-ms", 800, "milliseconds to let the forks settle before sampling")
	fs.StringVar(&cfg.jsonPath, "json", "", "optional path to write the report JSON (also printed to stdout)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.template == "" {
		return config{}, fmt.Errorf("--template is required")
	}
	if cfg.forks < 2 {
		return config{}, fmt.Errorf("--forks must be at least 2 to demonstrate sharing, got %d", cfg.forks)
	}
	if cfg.settleMs < 0 {
		return config{}, fmt.Errorf("--settle-ms must not be negative, got %d", cfg.settleMs)
	}
	return cfg, nil
}

// setupError marks a failure that prevented measurement (as opposed to a real
// DESIGN result). main maps it to a "SETUP:"-prefixed stderr line and exit 1.
type setupError struct{ err error }

func (e setupError) Error() string { return e.err.Error() }

func setupf(format string, args ...any) setupError {
	return setupError{fmt.Errorf(format, args...)}
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "husk-probe: %v\n", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		var se setupError
		if errors.As(err, &se) {
			fmt.Fprintf(os.Stderr, "SETUP: %v\n", se.err)
		} else {
			fmt.Fprintf(os.Stderr, "husk-probe: %v\n", err)
		}
		os.Exit(1)
	}
}

func run(cfg config) error {
	// Template presence is a SETUP precondition; surface it before touching KVM.
	memFile := filepath.Join(cfg.dataDir, "templates", cfg.template, "snapshot", "mem")
	if _, err := os.Stat(memFile); err != nil {
		return setupf("template %q snapshot not found under %s: %v", cfg.template, cfg.dataDir, err)
	}

	if err := ensureMemoryDelegated(cfg.cgroupRoot); err != nil {
		return err
	}

	// Mirror cmd/bench: zero jailer config, default opts. Engine construction
	// validates /dev/kvm; a failure here is a SETUP problem.
	engine, err := fork.NewEngine(cfg.dataDir, cfg.firecracker, cfg.kernel, firecracker.JailerConfig{}, fork.EngineOpts{})
	if err != nil {
		return setupf("init engine (needs Linux + /dev/kvm + template under --data-dir): %v", err)
	}

	type vm struct {
		id    string
		pid   int
		memcg string
	}
	vms := make([]vm, 0, cfg.forks)

	// Tear down every fork and remove its memcg on the way out, success or
	// failure, so a probe run never leaks VMs or cgroups on the runner.
	defer func() {
		for _, v := range vms {
			_ = engine.Terminate(v.id)
			// A memcg can only be removed once its last process has left; the
			// Terminate above kills Firecracker, so the rmdir should succeed.
			_ = os.Remove(v.memcg)
		}
	}()

	for i := 0; i < cfg.forks; i++ {
		id := fmt.Sprintf("husk-%d", i)
		if _, err := engine.Fork(cfg.template, id, fork.ForkOpts{}); err != nil {
			return setupf("fork %d of %d: %v", i+1, cfg.forks, err)
		}
		pid, ok := engine.SandboxPID(id)
		if !ok || pid <= 0 {
			return setupf("fork %s has no Firecracker PID", id)
		}
		memcg := filepath.Join(cfg.cgroupRoot, fmt.Sprintf("vm-%d", i))
		if err := os.MkdirAll(memcg, 0o755); err != nil {
			return setupf("create memcg %s: %v", memcg, err)
		}
		if err := os.WriteFile(filepath.Join(memcg, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
			return setupf("place pid %d in %s: %v", pid, memcg, err)
		}
		vms = append(vms, vm{id: id, pid: pid, memcg: memcg})
	}

	if cfg.settleMs > 0 {
		time.Sleep(time.Duration(cfg.settleMs) * time.Millisecond)
	}

	samples := make([]huskprobe.VMSample, 0, len(vms))
	for _, v := range vms {
		smaps, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", v.pid))
		if err != nil {
			return setupf("read smaps_rollup for pid %d: %v", v.pid, err)
		}
		pss, rss, sharedClean, privateDirty := huskprobe.ParseSmapsRollup(string(smaps))

		var current int64
		if cur, err := os.ReadFile(filepath.Join(v.memcg, "memory.current")); err == nil {
			current, _ = strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64)
		}
		var file, anon int64
		if stat, err := os.ReadFile(filepath.Join(v.memcg, "memory.stat")); err == nil {
			file, anon = huskprobe.ParseMemcgStat(string(stat))
		}

		samples = append(samples, huskprobe.VMSample{
			PID:          v.pid,
			Pss:          pss,
			Rss:          rss,
			SharedClean:  sharedClean,
			PrivateDirty: privateDirty,
			MemcgCurrent: current,
			MemcgFile:    file,
			MemcgAnon:    anon,
		})
	}

	report := huskprobe.Analyze(samples)

	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	fmt.Println(string(out))
	if cfg.jsonPath != "" {
		if err := os.WriteFile(cfg.jsonPath, out, 0o644); err != nil {
			return fmt.Errorf("write report json: %w", err)
		}
	}
	printSummary(report)
	return nil
}

// ensureMemoryDelegated makes the cgroup-root usable as a parent for per-VM
// memcgs: the directory must exist on a cgroup2 hierarchy, and the memory
// controller must be enabled in its cgroup.subtree_control so child cgroups can
// account memory. A non-writable hierarchy or an undelegatable memory
// controller is a SETUP problem.
func ensureMemoryDelegated(cgroupRoot string) error {
	if err := os.MkdirAll(cgroupRoot, 0o755); err != nil {
		return setupf("create cgroup root %s (cgroup2 not writable?): %v", cgroupRoot, err)
	}
	subtree := filepath.Join(cgroupRoot, "cgroup.subtree_control")
	cur, err := os.ReadFile(subtree)
	if err != nil {
		return setupf("read %s (is %s a cgroup2 mount?): %v", subtree, cgroupRoot, err)
	}
	for _, c := range strings.Fields(string(cur)) {
		if c == "memory" {
			return nil // already delegated
		}
	}
	if err := os.WriteFile(subtree, []byte("+memory"), 0o644); err != nil {
		return setupf("enable +memory in %s (controller not delegable): %v", subtree, err)
	}
	return nil
}

func printSummary(r huskprobe.Report) {
	mib := func(b int64) float64 { return float64(b) / (1024 * 1024) }
	fmt.Fprintf(os.Stderr, "\n=== husk-probe: %d fork(s) of one template ===\n", len(r.VMs))
	fmt.Fprintf(os.Stderr, "  NaiveSum:          %.2f MiB (every VM charged its full RSS, no sharing)\n", mib(r.NaiveSum))
	fmt.Fprintf(os.Stderr, "  SharedResident:    %.2f MiB (snapshot clean set, counted once)\n", mib(r.SharedResident))
	fmt.Fprintf(os.Stderr, "  TotalPrivateDirty: %.2f MiB (sum of every VM's own dirty)\n", mib(r.TotalPrivateDirty))
	fmt.Fprintf(os.Stderr, "  AggregatePhysical: %.2f MiB (SharedResident + TotalPrivateDirty)\n", mib(r.AggregatePhysical))
	fmt.Fprintf(os.Stderr, "  CoWSavings:        %.2f MiB (NaiveSum - AggregatePhysical)\n", mib(r.CoWSavings))
	fmt.Fprintf(os.Stderr, "  CoWSurvives:       %v\n", r.CoWSurvives)
	fmt.Fprintf(os.Stderr, "  DirtyPerVM:        %v\n", r.DirtyPerVM)
	for _, v := range r.VMs {
		fmt.Fprintf(os.Stderr, "  pid %d: Pss=%.2f Rss=%.2f PrivateDirty=%.2f MemcgCurrent=%.2f MiB\n",
			v.PID, mib(v.Pss), mib(v.Rss), mib(v.PrivateDirty), mib(v.MemcgCurrent))
	}
}
