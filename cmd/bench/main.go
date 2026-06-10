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
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/paperclipinc/sandbox/internal/benchstat"
	"github.com/paperclipinc/sandbox/internal/firecracker"
	"github.com/paperclipinc/sandbox/internal/fork"
	"github.com/paperclipinc/sandbox/internal/vsock"
)

const (
	modeForkExec = "fork-exec"
	modeExecRT   = "exec-rt"
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
}

// parseConfig parses args (excluding the program name) into a validated config.
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)

	var cfg config
	fs.StringVar(&cfg.mode, "mode", modeForkExec, "benchmark mode: fork-exec|exec-rt")
	fs.IntVar(&cfg.iterations, "iterations", 50, "measured iterations")
	fs.IntVar(&cfg.warmup, "warmup", 5, "warmup iterations (discarded)")
	fs.StringVar(&cfg.template, "template", "", "template (snapshot) id to fork from")
	fs.StringVar(&cfg.dataDir, "data-dir", "/var/lib/agent-run", "data directory holding template snapshots")
	fs.StringVar(&cfg.firecracker, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	fs.StringVar(&cfg.kernel, "kernel", "/var/lib/agent-run/vmlinux", "guest kernel path")
	fs.StringVar(&cfg.jsonPath, "json", "", "optional path to write results JSON")
	fs.BoolVar(&cfg.summary, "summary", false, "print the summary table to stdout")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if cfg.mode != modeForkExec && cfg.mode != modeExecRT {
		return config{}, fmt.Errorf("invalid --mode %q: want %s or %s", cfg.mode, modeForkExec, modeExecRT)
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

// benchForkExec measures the time from fork start to the first successful exec
// result, terminating the sandbox each iteration.
func benchForkExec(engine *fork.Engine, cfg config) (benchstat.Result, error) {
	// Warmup iterations are discarded; they pay the page-cache and
	// snapshot-load costs that should not skew the measured samples.
	for i := 0; i < cfg.warmup; i++ {
		id := fmt.Sprintf("bench-warm-%d", i)
		if err := oneForkExec(engine, cfg.template, id); err != nil {
			return benchstat.Result{}, fmt.Errorf("warmup iteration %d: %w", i, err)
		}
	}

	samples := make([]time.Duration, 0, cfg.iterations)
	for i := 0; i < cfg.iterations; i++ {
		id := fmt.Sprintf("bench-fe-%d", i)
		t0 := time.Now()
		if err := oneForkExec(engine, cfg.template, id); err != nil {
			return benchstat.Result{}, fmt.Errorf("iteration %d: %w", i, err)
		}
		samples = append(samples, time.Since(t0))
	}

	return benchstat.Result{Name: "fork_to_first_exec", Unit: "ms", Summary: benchstat.Summarize(samples)}, nil
}

// oneForkExec forks one sandbox, execs a trivial command over its vsock, and
// terminates it. The timer in the caller spans this whole call.
func oneForkExec(engine *fork.Engine, template, sandboxID string) error {
	res, err := engine.Fork(template, sandboxID, fork.ForkOpts{})
	if err != nil {
		return fmt.Errorf("fork: %w", err)
	}
	defer func() { _ = engine.Terminate(sandboxID) }()

	client, err := connectWithRetry(res.VsockPath)
	if err != nil {
		return err
	}
	defer client.Close()

	if _, err := client.Exec("/bin/true", "/", nil, 10); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
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

	// Warm the connection and the guest exec path with one discarded exec.
	if _, err := client.Exec("/bin/true", "/", nil, 10); err != nil {
		return benchstat.Result{}, fmt.Errorf("warmup exec: %w", err)
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
