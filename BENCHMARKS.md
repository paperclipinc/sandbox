# Benchmarks

Every latency number this project publishes must come from a benchmark anyone
can rerun. This file documents the methodology behind the reproducible harness
and records where the current (shared-CI-class) numbers come from. It is
deliberately honest about what is measured and what is still open.

## What is measured today

The harness is `cmd/bench`. It imports `internal/fork` and drives the real
KVM-backed engine in-process (no forkd, no gRPC, no HTTP API in the path), so
the timing reflects the fork + vsock + guest-agent data path and nothing else.
The percentile statistics are computed by `internal/benchstat` (count, min, p50,
p90, p99, max, mean; nearest-rank percentiles).

Two modes:

- **`fork-exec`** (`fork_to_first_exec`): measures the wall time from the start
  of a fork to the first successful exec result returned through the guest
  agent. Each iteration forks a fresh sandbox from the template snapshot,
  connects to the fork's Firecracker vsock UDS, execs a trivial command, and
  terminates the sandbox. This is the cold-claim-shaped number: snapshot restore
  plus the time for the guest agent to service the first exec.
- **`exec-rt`** (`exec_round_trip`): forks one sandbox once, warms the
  connection and the guest exec path, then measures a stream of trivial exec
  round-trips against the already-warm agent. This isolates the warm exec
  hot-path (vsock round-trip + `/bin/sh -c` spawn in the guest) from the
  one-time restore cost.

Warmup iterations are discarded; they pay the page-cache and snapshot-load costs
that should not skew the measured samples.

## Hardware and configuration (CI run)

- **Runner:** GitHub Actions `ubuntu-latest` (a shared, oversubscribed runner,
  frequently itself nested-virt). This is NOT bare metal.
- **VMM:** Firecracker v1.15.0.
- **Kernel:** the Firecracker CI kernel (`vmlinux-*`) pulled from
  `spec.ccfc.min` for the v1.15 CI series.
- **Rootfs:** a minimal ext4 image with the project's guest agent as `/init`
  plus a static busybox (`/bin/sh`, `/bin/true`, and the small tool set the
  agent's exec path needs).
- **Iterations:** 30 measured, 5 warmup, per mode (modest on purpose; the runner
  is noisy).
- **VM size:** 1 vCPU, 256 MiB.

## Results

The numbers are produced by the `KVM Integration Test` workflow on every run of
the bench phase and are **SHARED-CI-CLASS**: noisy, oversubscribed, and not
representative of bare metal. They exist to prove the harness runs end to end and
to give a reproducible distribution that is regenerated every run, not to be
quoted as the product's latency.

> Populated from the CI artifact. The latency tables for `fork_to_first_exec`
> and `exec_round_trip` are printed to the step log AND appended to the run's
> job summary by the bench phase, and the raw JSON is uploaded as the
> `bench-results` workflow artifact. See the most recent `KVM Integration Test`
> workflow run summary for the current shared-CI-class distribution. (We do not
> paste numbers here: this file is committed before the post-merge CI run that
> produces them, and a hand-copied number would be exactly the kind of
> unverifiable claim this harness exists to eliminate.)

To regenerate the numbers on your own hardware, see [`bench/README.md`](bench/README.md).

## Open (not yet measured)

These are explicitly out of scope for the current harness and tracked in
[#15](https://github.com/paperclipinc/sandbox/issues/15) / roadmap section 4:

- **Bare-metal reference numbers** on the Hetzner + Talos reference node. The
  CI numbers above are shared-runner-class; the representative numbers need the
  reference hardware to exist.
- **Claim to first-exec end to end through the controller** on a real cluster
  (claim a `Sandbox` CRD, wait for the pool to hand back a forked VM, exec):
  the current harness measures the engine data path, not the controller +
  scheduler + pool path.
- **Sustained claims/sec** and **density curves** (how many forks per node
  before p99 degrades, unique-vs-shared memory at density).
- **Pool-rebuild propagation**: time from a new template snapshot landing to it
  being claimable across the pool.
- **Head-to-head competitor comparison** against E2B (self-hosted), Daytona
  (OSS), and Agent Sandbox + Kata, on identical hardware, regenerated from
  in-repo scripts so anyone can reproduce or refute it.
- **Latency regression gating**: deliberately not done on shared CI, which is
  too noisy to threshold without flaking.
