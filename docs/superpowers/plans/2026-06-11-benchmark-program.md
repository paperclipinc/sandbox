# Benchmark Program Implementation Plan (issue #15)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A reproducible `bench/` harness that measures the latencies that matter, run in CI on the KVM runner, with results written to a `BENCHMARKS.md` that states the methodology and hardware honestly. This is the no-unverified-claims principle made concrete: instead of target numbers, the repo carries measured fork->first-exec and exec-round-trip latencies that anyone can reproduce, clearly labeled as shared-CI-class (not bare metal). The bare-metal reference numbers and the head-to-head competitor comparison remain explicitly open (they need the reference hardware and the competitors installed on the same machine).

**Honesty constraint (the whole point):** every number in BENCHMARKS.md and the README is produced by the in-repo harness on documented hardware. The harness measures end-to-end fork->first-successful-exec (snapshot restore plus the first command actually completing through the guest agent), not just the restore syscall. Numbers measured on GitHub Actions shared runners are labeled as such with a loud disclaimer that they are noisy and not representative of bare metal; the README does not state a single latency number that the harness did not produce.

**Architecture:** A `cmd/bench` tool drives the real fork data path against a running forkd (or directly the engine on a KVM host): build/load a template, then over N iterations fork a sandbox and exec a trivial command, timing restore-start to exec-result; separately, on one warm sandbox, measure exec round-trip over M iterations. It computes P50/P90/P99 and emits JSON plus a human table. The KVM CI workflow runs it after building a template and appends the numbers to the GitHub step summary and to a committed BENCHMARKS.md template via a documented manual-update step (CI prints; a human or a follow-up automation commits, to avoid CI committing to main). Pure statistics (percentile computation, result formatting) are unit-tested on any platform; the real timing runs only on the KVM runner.

**Context for the implementer:**
- The KVM CI (`.github/workflows/kvm-test.yaml`) already: installs Firecracker v1.15 + jailer, downloads a kernel and rootfs, builds the guest agent into a rootfs, boots a VM, snapshots it, restores it, and execs via the guest agent over vsock (`cmd/test-agent`). Reuse this machinery: the bench needs a template snapshot and the vsock exec path.
- Fork data path: `internal/fork/engine.go` Fork (restore snapshot into a fresh Firecracker process) and the vsock exec via `internal/vsock` + the guest agent. `cmd/test-agent` shows how to connect and exec over the Firecracker vsock UDS.
- For a representative end-to-end measurement WITHOUT a full k8s cluster, drive the engine or forkd directly on the KVM host: fork from a prepared template snapshot, connect to the new sandbox's vsock, exec `true` or `echo`, measure. The standalone `cmd/sandbox-server` (no k8s) already wires the engine + the HTTP exec path and may be the simplest driver; or call the engine directly from `cmd/bench`.
- Conventions: CLAUDE.md authoritative. No em/en dashes. TDD for the pure stats. Explicit-path git add. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Lint darwin + GOOS=linux.

---

### Task 1: `internal/benchstat` percentile + result formatting (pure, unit-tested)

**Files:** Create `internal/benchstat/stats.go`, `internal/benchstat/stats_test.go`.

- [ ] `type Sample float64` (milliseconds) or use `time.Duration`. `type Summary{ Count int; Min, P50, P90, P99, Max, Mean Duration }`. `func Summarize(samples []time.Duration) Summary` (sort, nearest-rank percentiles, handle empty and single-sample). `func (s Summary) Table() string` (human-readable aligned table) and a JSON-serializable struct `type Result{ Name string; Unit string; Summary Summary }` with a `func WriteJSON(w io.Writer, results []Result) error`. TDD: known input yields known P50/P99 (nearest-rank, deterministic), empty input is safe, single sample, monotonic ordering; JSON round-trips.
- [ ] Commit `feat: benchstat percentile summarization and result formatting`.

### Task 2: `cmd/bench` driver

**Files:** Create `cmd/bench/main.go` (+ a `main_other.go` stub if the timing path is Linux-only; keep the arg parsing and the benchstat usage cross-platform).

- [ ] Flags: `--mode fork-exec|exec-rt`, `--iterations N` (default 50), `--warmup W` (default 5, discarded), `--template <id>`, `--data-dir`, `--firecracker`, `--kernel`, and the engine/forkd connection params, plus `--json <path>` and `--summary` (print the table). 
- [ ] `fork-exec` mode: for each iteration (after warmup), fork a sandbox from the template, connect to its vsock, exec a trivial command (`/bin/true` or `echo x`), record the duration from fork-start to exec-result, terminate the sandbox. Report the Summary as `fork_to_first_exec_ms`.
- [ ] `exec-rt` mode: fork one sandbox, warm it, then exec a trivial command M times recording each round-trip; report `exec_round_trip_ms`.
- [ ] Drive the REAL engine (import internal/fork + internal/vsock) on a KVM host, OR drive a running forkd/sandbox-server over its API; choose the path that most directly measures the data path with the least orchestration. Document the choice. Keep the tool dependency-light and scriptable; nonzero exit on setup failure, clear stderr.
- [ ] No unit test of the timing path (KVM-only); a smoke build test and the benchstat tests cover the logic. Ensure `go build ./cmd/bench` works on darwin (guard the Linux-only engine calls behind a build tag if needed, with a clear not-supported message on non-Linux).
- [ ] Commit `feat: cmd/bench fork-exec and exec round-trip latency driver`.

### Task 3: KVM CI benchmark phase + BENCHMARKS.md

**Files:** `.github/workflows/kvm-test.yaml`, `BENCHMARKS.md` (new), `bench/README.md` (new, how to run it locally/on bare metal).

- [ ] KVM CI phase (after a template snapshot is available): build `cmd/bench`, run `fork-exec` mode with a modest iteration count (e.g. 30 plus 5 warmup) and `exec-rt` mode, print the tables to the step log AND append them to `$GITHUB_STEP_SUMMARY` so the numbers are visible on the run page. Save the JSON as a workflow artifact. This phase is allowed to be non-gating on the NUMBERS (shared runners are noisy; do not fail on a latency threshold) but MUST gate on the bench running successfully (a crash or zero samples fails the job). Add a comment stating the numbers are shared-CI-class.
- [ ] `BENCHMARKS.md`: the methodology (what fork->first-exec measures: restore start to first exec result through the guest agent; what exec-round-trip measures), the hardware (GitHub Actions runner class, Firecracker v1.15, kernel/rootfs versions, iteration counts), a results section that says the current numbers are produced by the CI run and are shared-CI-class (noisy, not bare metal), and an explicit OPEN section: bare-metal reference numbers on the Hetzner+Talos reference node (pending that hardware), claim->first-exec end-to-end through the controller (pending a bench cluster), density curves, pool-rebuild propagation, and the head-to-head competitor comparison (E2B self-hosted, Daytona OSS, Agent Sandbox + Kata) on the same hardware. Do NOT paste fabricated numbers; if you cannot capture the CI numbers into the file in this PR (CI runs after merge), put a clearly-marked placeholder line that says the numbers are populated from the CI artifact and link the workflow, and have the README point at BENCHMARKS.md rather than stating numbers inline.
- [ ] `bench/README.md`: how to run `cmd/bench` on a real KVM host (bare metal) to reproduce or extend, so the reference-hardware numbers can be captured when that hardware exists.
- [ ] Commit `ci: run the bench harness on the KVM runner, publish to step summary`.

### Task 4: README + ROADMAP honesty pass

**Files:** `README.md` (CAUTION: this is the one place the session has historically been careful; make ONLY the performance-section change described), `ROADMAP.md`.

- [ ] README performance section: ensure it states no latency number that the harness did not produce. Point to BENCHMARKS.md for measured (CI-class) numbers and state that bare-metal reference numbers are pending the reference hardware. The mechanism description (Firecracker CoW snapshot restore) stays; the specific millisecond claims must either be the harness-measured CI numbers (clearly labeled) or remain qualitative with a pointer to BENCHMARKS.md. If the README already avoids inline numbers (it was rewritten this way earlier), just add the BENCHMARKS.md pointer.
- [ ] ROADMAP section 4 (benchmark program): flip the harness + fork->exec/exec-rt measurement to done (reproducible in CI); leave bare-metal reference numbers, claim->exec end-to-end, density curves, propagation, and the competitor comparison open with notes.
- [ ] Commit `docs: point performance claims at the reproducible bench harness`.

### Task 5: verification + PR

- [ ] Full verification: build darwin + GOOS=linux, vet, lint both, all Go suites with envtest, Python suite, gofmt zero, dash grep zero, YAML parse, `go build ./cmd/bench` on darwin.
- [ ] Push `feat/benchmark-program`, PR `Benchmark harness: reproducible fork-exec and exec round-trip latency` body references #15, states what is measured (CI-class) and what is open (bare-metal, claim->exec, competitor comparison), watch CI (confirm the bench phase runs and prints numbers), merge when green.

**Out of scope (remain open in #15 / roadmap section 4):** bare-metal reference numbers on Hetzner+Talos (needs the hardware); claim->first-exec end-to-end through the controller on a real cluster; sustained claims/sec, density curves, pool-rebuild propagation; the reproducible head-to-head comparison table against E2B/Daytona/Agent-Sandbox on identical hardware; regression-gating on latency (too noisy on shared CI).
