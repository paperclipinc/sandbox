// Command bench measures the sandbox fork and exec data path directly against
// the real KVM-backed engine. It is the reproducible source behind every
// latency number the project publishes (CLAUDE.md operating principle 1).
//
// Driver path: bench imports internal/fork and internal/vsock and drives the
// engine in-process. This is the most direct measurement of the data path: it
// forks from a template snapshot already present under --data-dir (the CI
// builds it), connects to the fork's Firecracker vsock UDS, and execs a
// trivial command. There is no forkd, no gRPC, and no HTTP API in the path, so
// the timing reflects fork + vsock + guest agent and nothing else.
//
// The engine validates /dev/kvm at construction, so the timing path runs only
// on a Linux KVM host; on any other platform the tool builds and parses flags
// but exits non-zero at engine construction with a clear message.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/paperclipinc/mitos/internal/benchstat"
	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/metering"
	"github.com/paperclipinc/mitos/internal/vsock"
)

const (
	modeForkExec = "fork-exec"
	modeExecRT   = "exec-rt"
	modeMetering = "metering"
)

// config holds the parsed, validated flags. Parsing is split out so it can be
// unit-tested without touching the KVM-only timing path.
type config struct {
	mode        string
	iterations  int
	warmup      int
	template    string
	dataDir     string
	firecracker string
	kernel      string
	jsonPath    string
	summary     bool
	// forks is the number of sandboxes the metering mode forks from one
	// template before reading the CoW-aware metering report. It is unused by
	// the latency modes.
	forks    int
	settleMs int
}

// parseConfig parses args (excluding the program name) into a validated config.
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)

	var cfg config
	fs.StringVar(&cfg.mode, "mode", modeForkExec, "benchmark mode: fork-exec|exec-rt|metering")
	fs.IntVar(&cfg.iterations, "iterations", 50, "measured iterations")
	fs.IntVar(&cfg.warmup, "warmup", 5, "discarded warmup iterations; in exec-rt mode one mandatory connection-establishment exec always runs in addition to these, even at --warmup=0")
	fs.StringVar(&cfg.template, "template", "", "template (snapshot) id to fork from")
	fs.StringVar(&cfg.dataDir, "data-dir", "/var/lib/mitos", "data directory holding template snapshots")
	fs.StringVar(&cfg.firecracker, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	fs.StringVar(&cfg.kernel, "kernel", "/var/lib/mitos/vmlinux", "guest kernel path")
	fs.StringVar(&cfg.jsonPath, "json", "", "optional path to write results JSON")
	fs.BoolVar(&cfg.summary, "summary", false, "print the summary table to stdout")
	fs.IntVar(&cfg.forks, "forks", 4, "metering mode: number of sandboxes to fork from one template before reading the report")
	fs.IntVar(&cfg.settleMs, "settle-ms", 500, "metering mode: milliseconds to let the forks settle before reading the report")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if cfg.mode != modeForkExec && cfg.mode != modeExecRT && cfg.mode != modeMetering {
		return config{}, fmt.Errorf("invalid --mode %q: want %s, %s, or %s", cfg.mode, modeForkExec, modeExecRT, modeMetering)
	}
	if cfg.template == "" {
		return config{}, fmt.Errorf("--template is required")
	}
	if cfg.iterations < 1 {
		return config{}, fmt.Errorf("--iterations must be at least 1, got %d", cfg.iterations)
	}
	if cfg.warmup < 0 {
		return config{}, fmt.Errorf("--warmup must not be negative, got %d", cfg.warmup)
	}
	if cfg.mode == modeMetering {
		if cfg.forks < 1 {
			return config{}, fmt.Errorf("--forks must be at least 1 in metering mode, got %d", cfg.forks)
		}
		if cfg.settleMs < 0 {
			return config{}, fmt.Errorf("--settle-ms must not be negative, got %d", cfg.settleMs)
		}
	}
	return cfg, nil
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(2)
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	// Mirror cmd/forkd construction with a zero jailer config (jailer
	// disabled) and networking/CAS opts left at their defaults: the bench
	// measures the bare fork + exec path, so no per-fork network is set up.
	engine, err := fork.NewEngine(cfg.dataDir, cfg.firecracker, cfg.kernel, firecracker.JailerConfig{}, fork.EngineOpts{})
	if err != nil {
		return fmt.Errorf("init engine (needs Linux + /dev/kvm + template under --data-dir): %w", err)
	}

	// Metering mode forks N real sandboxes from one template and prints the
	// CoW-aware metering report; it does not produce a latency distribution.
	if cfg.mode == modeMetering {
		return runMetering(engine, cfg)
	}

	var result benchstat.Result
	switch cfg.mode {
	case modeForkExec:
		result, err = benchForkExec(engine, cfg)
	case modeExecRT:
		result, err = benchExecRT(engine, cfg)
	default:
		return fmt.Errorf("invalid mode %q", cfg.mode)
	}
	if err != nil {
		return err
	}

	results := []benchstat.Result{result}

	if cfg.summary {
		fmt.Printf("%s (%s)\n%s", result.Name, result.Unit, result.Summary.Table())
	}
	if cfg.jsonPath != "" {
		f, err := os.Create(cfg.jsonPath)
		if err != nil {
			return fmt.Errorf("create json output: %w", err)
		}
		defer f.Close()
		if err := benchstat.WriteJSON(f, results); err != nil {
			return err
		}
	}
	return nil
}

// runMetering forks cfg.forks real sandboxes from one template, lets them
// settle, then reads the engine's CoW-aware metering report and prints it as
// JSON (machine-readable for the CI jq assertions) plus a human summary. The
// forks are NOT torn down before the report is read: the whole point is to
// observe N concurrent forks of one template sharing the same restored page
// set, so the shared template region is counted once and the per-fork marginal
// cost is the unique set. Every fork is torn down after the report is captured.
//
// This proves metering correctness AND yields an honest density datapoint: the
// shared template footprint is paid once, and each additional fork adds only
// its unique (private-dirty) pages.
func runMetering(engine *fork.Engine, cfg config) error {
	forked := make([]string, 0, cfg.forks)
	// Tear every fork down on the way out, success or failure, so a metering
	// run never leaks VMs on the runner.
	defer func() {
		for _, id := range forked {
			_ = engine.Terminate(id)
		}
	}()

	for i := 0; i < cfg.forks; i++ {
		id := fmt.Sprintf("meter-%d", i)
		if _, err := engine.Fork(cfg.template, id, fork.ForkOpts{}); err != nil {
			return fmt.Errorf("fork %d of %d: %w", i+1, cfg.forks, err)
		}
		forked = append(forked, id)
	}

	// Let the forks settle so their resident set reflects a steady restored
	// state rather than the instant after restore.
	if cfg.settleMs > 0 {
		time.Sleep(time.Duration(cfg.settleMs) * time.Millisecond)
	}

	report := engine.Metering()

	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metering report: %w", err)
	}
	// The JSON is the contract the CI jq assertions parse; print it on its own.
	fmt.Println(string(out))

	if cfg.summary {
		printMeteringSummary(report, cfg.forks)
	}
	if cfg.jsonPath != "" {
		if err := os.WriteFile(cfg.jsonPath, out, 0o644); err != nil {
			return fmt.Errorf("write metering json: %w", err)
		}
	}
	return nil
}

// printMeteringSummary prints a short human-readable summary of the CoW-aware
// metering report to stdout. All numbers are derived from the real engine
// report (smaps-derived memory, stat-derived disk); nothing is invented.
func printMeteringSummary(report metering.Report, forks int) {
	mib := func(b int64) float64 { return float64(b) / (1024 * 1024) }
	fmt.Printf("\n=== CoW-aware metering: %d fork(s) of one template ===\n", forks)
	fmt.Printf("  sandboxes:        %d\n", len(report.Sandboxes))
	fmt.Printf("  templates:        %d\n", len(report.Templates))
	fmt.Printf("  TotalUnique:      %.2f MiB (sum of every fork's private-dirty set)\n", mib(report.TotalUnique))
	fmt.Printf("  UsedCoWAware:     %.2f MiB (unique + each template's shared set counted once)\n", mib(report.UsedCoWAware))
	fmt.Printf("  UsedNaive:        %.2f MiB (unique + every fork's shared set, double-counted)\n", mib(report.UsedNaive))
	fmt.Printf("  CoWSavings:       %.2f MiB (naive - CoW-aware)\n", mib(report.CoWSavings))
	for _, t := range report.Templates {
		fmt.Printf("  template %q: forks=%d sharedOnce=%.2f MiB diskSharedOnce=%.2f MiB\n",
			t.Template, t.ForkCount, mib(t.SharedOnce), mib(t.DiskSharedOnce))
	}
	for _, s := range report.Sandboxes {
		fmt.Printf("  fork %q: unique=%.2f MiB shared=%.2f MiB\n",
			s.ID, mib(s.MemoryUnique), mib(s.MemoryShared))
	}
}

// benchForkExec measures the time from fork start to the first successful exec
// result, terminating the sandbox each iteration.
func benchForkExec(engine *fork.Engine, cfg config) (benchstat.Result, error) {
	// Warmup iterations are discarded; they pay the page-cache and
	// snapshot-load costs that should not skew the measured samples.
	for i := 0; i < cfg.warmup; i++ {
		id := fmt.Sprintf("bench-warm-%d", i)
		if _, err := oneForkExec(engine, cfg.template, id); err != nil {
			return benchstat.Result{}, fmt.Errorf("warmup iteration %d: %w", i, err)
		}
	}

	samples := make([]time.Duration, 0, cfg.iterations)
	for i := 0; i < cfg.iterations; i++ {
		id := fmt.Sprintf("bench-fe-%d", i)
		elapsed, err := oneForkExec(engine, cfg.template, id)
		if err != nil {
			return benchstat.Result{}, fmt.Errorf("iteration %d: %w", i, err)
		}
		samples = append(samples, elapsed)
	}

	return benchstat.Result{Name: "fork_to_first_exec", Unit: "ms", Summary: benchstat.Summarize(samples)}, nil
}

// oneForkExec forks one sandbox, execs a trivial command over its vsock, and
// terminates it, returning the measured fork-to-first-exec elapsed time.
//
// Measurement boundary (do not regress): the clock starts immediately before
// Fork and stops the instant the first exec result is in. Teardown (client
// close and engine.Terminate, which SIGKILLs Firecracker, waits on the
// process, and removes the sandbox/jailer chroot) runs AFTER the elapsed value
// is captured and is therefore NOT counted in the returned duration. The
// directive is fork -> first successful exec, not fork -> teardown.
func oneForkExec(engine *fork.Engine, template, sandboxID string) (time.Duration, error) {
	t0 := time.Now()
	res, err := engine.Fork(template, sandboxID, fork.ForkOpts{})
	if err != nil {
		return 0, fmt.Errorf("fork: %w", err)
	}
	// From here every path must tear the sandbox down so a failed iteration
	// does not leak a VM. cleanup is invoked explicitly (never deferred on the
	// success path) so that it runs only AFTER elapsed is computed.
	cleanup := func() { _ = engine.Terminate(sandboxID) }

	client, err := connectWithRetry(res.VsockPath)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("connect: %w", err)
	}

	if _, err := client.Exec("/bin/true", "/", nil, 10); err != nil {
		client.Close()
		cleanup()
		return 0, fmt.Errorf("exec: %w", err)
	}

	elapsed := time.Since(t0) // clock stops here, before any teardown
	client.Close()
	cleanup() // teardown is NOT part of elapsed
	return elapsed, nil
}

// benchExecRT forks one sandbox, warms it, then measures M trivial exec
// round-trips against the live agent.
func benchExecRT(engine *fork.Engine, cfg config) (benchstat.Result, error) {
	const sandboxID = "bench-execrt"
	res, err := engine.Fork(cfg.template, sandboxID, fork.ForkOpts{})
	if err != nil {
		return benchstat.Result{}, fmt.Errorf("fork: %w", err)
	}
	defer func() { _ = engine.Terminate(sandboxID) }()

	client, err := connectWithRetry(res.VsockPath)
	if err != nil {
		return benchstat.Result{}, err
	}
	defer client.Close()

	// Connection establishment: one mandatory discarded exec that pays the
	// first-exec costs (guest exec path cold start, any lazy connection
	// setup) which must happen once before the agent can serve execs at all.
	// This is distinct from and always runs in addition to the --warmup execs
	// below; it is not counted by --warmup. With --warmup=0 the agent still
	// gets this single connection-establishing exec, but zero discretionary
	// warmup iterations on top of it.
	if _, err := client.Exec("/bin/true", "/", nil, 10); err != nil {
		return benchstat.Result{}, fmt.Errorf("connection-establishment exec: %w", err)
	}
	for i := 0; i < cfg.warmup; i++ {
		if _, err := client.Exec("/bin/true", "/", nil, 10); err != nil {
			return benchstat.Result{}, fmt.Errorf("warmup exec %d: %w", i, err)
		}
	}

	samples := make([]time.Duration, 0, cfg.iterations)
	for i := 0; i < cfg.iterations; i++ {
		t0 := time.Now()
		if _, err := client.Exec("/bin/true", "/", nil, 10); err != nil {
			return benchstat.Result{}, fmt.Errorf("exec iteration %d: %w", i, err)
		}
		samples = append(samples, time.Since(t0))
	}

	return benchstat.Result{Name: "exec_round_trip", Unit: "ms", Summary: benchstat.Summarize(samples)}, nil
}

// connectWithRetry dials the fork's vsock UDS, retrying briefly because the
// guest agent needs a moment to accept connections after a restore.
func connectWithRetry(vsockPath string) (*vsock.Client, error) {
	const attempts = 50
	var lastErr error
	for i := 0; i < attempts; i++ {
		client, err := vsock.Connect(vsockPath, vsock.AgentPort)
		if err == nil {
			return client, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("connect vsock %s after %d attempts: %w", vsockPath, attempts, lastErr)
}
