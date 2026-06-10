# Jailer + Fork-Correctness (RNG, Clock) Implementation Plan (issues #2, #5, #6)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close issue #2 (Firecracker under the jailer: per-VM UID/GID, chroot, cgroup; forkd drops `privileged: true`) and land the first two fork-correctness rows, #5 (RNG reseed on every fork via virtio-rng plus a guest NotifyForked hook) and #6 (clock resync after restore), each verified in the KVM CI workflow.

**Architecture:** forkd launches every Firecracker process through the jailer binary: a per-VM workspace under the chroot base, a per-VM UID/GID from a configurable range, cgroup attachment, with snapshot and rootfs files hard-linked into the chroot. The guest agent gains a `notify_forked` vsock message carrying a generation counter and the host wall clock; on receipt it injects entropy via RNDADDENTROPY, steps CLOCK_REALTIME when drift exceeds tolerance, bumps `/run/sandbox/fork-generation`, and signals userspace. forkd sends the notification immediately after every snapshot restore, before the fork RPC returns. CI proves distinct urandom streams across forks of one snapshot and wall-clock correctness after restore.

**Constraints from CLAUDE.md:** no em or en dashes anywhere; TDD; explicit-path git add; conventional commits with the `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer; secret values and key material never logged; threat model delta in the same PR; lint on darwin and GOOS=linux.

**Context for the implementer:**
- VM launch: `internal/firecracker/client.go:StartVM` (direct exec of the firecracker binary). Template build: `internal/firecracker/template.go`. Engine: `internal/fork/engine.go` (Fork restores snapshots into fresh FC processes; ForkRunning checkpoints then forks).
- The jailer binary ships in the same release archive as firecracker (the kvm-test workflow downloads `firecracker-vVERSION-x86_64.tgz`, which contains `jailer-vVERSION-x86_64`; install it alongside).
- Jailer invocation shape: `jailer --id <vm-id> --exec-file /usr/local/bin/firecracker --uid <uid> --gid <gid> --chroot-base-dir <base> -- <firecracker args>`. The API socket then lives at `<base>/firecracker/<vm-id>/root/run/firecracker.socket` (note `/run/` inside the chroot). All file paths handed to the FC API (kernel, rootfs, snapshot mem/vmstate, vsock UDS) are interpreted INSIDE the chroot, so files must be hard-linked (same filesystem) or copied into `<chroot>/...` and passed as chroot-relative paths. Hard links require the data dir and chroot base on one filesystem; document this requirement and fail closed with a clear error when violated.
- Guest agent: `guest/agent/main.go` (PID 1, vsock JSON protocol, handleConfigure precedent). Protocol: `internal/vsock/protocol.go` + `client.go` (TypeConfigure precedent; tests in client_test.go use startFakeAgent).
- KVM CI: `.github/workflows/kvm-test.yaml` builds a rootfs with the agent as /init, boots, snapshots, restores, runs `cmd/test-agent` against the vsock UDS. The snapshot restore section and the agent test section are separate; fork-correctness checks extend the workflow with a restore-twice-from-one-snapshot phase.
- forkd manifests: `deploy/daemon/daemonset.yaml` (currently `privileged: true`).
- Suites: `go test ./internal/...` (controller needs `eval $(~/go/bin/setup-envtest use 1.31 -p env)`); lint `~/go/bin/golangci-lint run --timeout=5m` both GOOS.

---

### Task 1: jailer launch path in internal/firecracker

**Files:** Create `internal/firecracker/jailer.go`, `internal/firecracker/jailer_test.go`; modify `internal/firecracker/client.go`, `internal/firecracker/types.go`.

- [ ] `JailerConfig` struct on VMConfig: `JailerBin string` (empty means no jailer, direct exec as today), `ChrootBaseDir string`, `UIDRange [2]uint32` (allocate per VM round-robin with an in-use set; release on Kill), `CgroupVersion int` (2 default). Plumb through `StartVM`.
- [ ] `jailer.go`: pure helpers, unit-testable WITHOUT KVM or root:
  - `jailerArgs(cfg VMConfig, uid, gid uint32) []string` builds the exec invocation
  - `chrootPath(baseDir, vmID, p string) string` maps a host path to its in-chroot location
  - `prepareChroot(cfg, vmID string, files []string) (map[string]string, error)` hard-links each file into the chroot and returns host-path to chroot-relative-path mapping; falls back to copy with a logged warning when EXDEV; never copies or links anything outside the VM workspace
  - `uidAllocator` type with Acquire/Release and exhaustion error
- [ ] TDD the helpers: args shape, path mapping, allocator exhaustion and reuse, EXDEV fallback (simulate by linking across tmpfs and disk if available, else unit-test the error branch with a stub linker function field).
- [ ] `StartVM` with JailerBin set: prepare chroot, exec jailer, wait for the API socket at the jailed location, and rewrite every subsequent API path (boot source, drives, snapshot load and create, vsock UDS) through `chrootPath`. The Client gains the socket path translation; keep the direct-exec path byte-identical to today when JailerBin is empty.
- [ ] Engine wiring: `internal/fork/engine.go` NewEngine gains jailer settings from forkd flags (Task 2); Fork and CreateTemplate pass the snapshot, rootfs, kernel files through prepareChroot.
- [ ] Commit `feat: jailer launch path with per-VM uid, chroot, and path translation`.

### Task 2: forkd flags, manifest privilege drop, threat model

**Files:** `cmd/forkd/main.go`, `deploy/daemon/daemonset.yaml`, `docs/threat-model.md`, `ROADMAP.md`.

- [ ] forkd flags: `--jailer` (path, empty disables), `--chroot-base` (default `/srv/jailer` which must share a filesystem with `--data-dir`), `--uid-range` (default `64000-64999`). Wire into NewEngine. When the jailer is enabled forkd refuses to start as nonroot-without-CAP_SYS_ADMIN misconfigurations with clear errors (the jailer itself needs root or the right caps; document exactly which).
- [ ] daemonset: drop `privileged: true`; replace with the minimal set: capabilities add `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, `CAP_CHOWN`, `CAP_SETUID`, `CAP_SETGID`, `CAP_MKNOD` (verify the jailer documentation set and trim to what the KVM CI run actually proves necessary; start minimal and add only on observed failure), keep the /dev/kvm device mount, set the jailer flags in args. Add a comment per capability stating why.
- [ ] Threat model section 1: jailer row flips to mitigated when deployed as shipped (per-VM UID, chroot, cgroup; direct-exec dev path remains and is flagged). Seccomp row: note the jailer-launched VMM runs Firecracker's default seccomp filters; document level. ROADMAP section 1 jailer line flips with residuals.
- [ ] Commit `feat: forkd runs Firecracker under the jailer; daemonset drops privileged`.

### Task 3: NotifyForked protocol + guest handling (RNG + clock)

**Files:** `internal/vsock/protocol.go`, `internal/vsock/client.go`, `internal/vsock/client_test.go`, `guest/agent/main.go`, `internal/guestenv` untouched.

- [ ] Protocol: `TypeNotifyForked`; `NotifyForkedRequest{ Generation uint64; HostWallClockNanos int64; Entropy []byte }` (32 bytes of host crypto/rand entropy; comment: entropy and clock never logged). Response carries `AppliedClockStepNanos int64` and `ReseededRNG bool` for observability.
- [ ] Client: `NotifyForked(generation uint64, entropy []byte) (*NotifyForkedResponse, error)` stamping HostWallClockNanos at send time. TDD against startFakeAgent.
- [ ] Guest agent handler (linux-only): inject entropy via the RNDADDENTROPY ioctl on /dev/urandom (fall back to writing to /dev/urandom plus logging when the ioctl fails; write COUNTS not contents); compare CLOCK_REALTIME with HostWallClockNanos and `clock_settime` when |drift| > 500ms (respond with the applied step); write the generation number to `/run/sandbox/fork-generation` (mkdir -p /run/sandbox); send SIGUSR2 to all processes except PID 1 via sweeping /proc (best effort, count signaled processes in stdout log). Cross-compile and lint GOOS=linux.
- [ ] Commit `feat: guest NotifyForked reseeds RNG, steps clock, signals userspace`.

### Task 4: forkd sends NotifyForked on every restore

**Files:** `internal/daemon/server.go`, `internal/daemon/delivery_test.go` (extend), `internal/fork/engine.go` or server-level counter.

- [ ] After a successful Fork or ForkRunning and agent registration, forkd sends NotifyForked with a per-sandbox generation (monotonic counter per forkd process is sufficient; uniqueness matters, ordering does not) and fresh crypto/rand entropy, BEFORE the RPC returns. Strictness mirrors deliverConfig: real engine plus failure means the fork fails and the sandbox is reaped (a fork with shared RNG state is incorrect, not degraded); mock engine skips. Combine into the existing deliverConfig flow so there is one post-restore guest handshake function.
- [ ] TDD with the fake vsock agent (record the NotifyForked payload; assert entropy non-zero, generation increments across two forks, fork fails when the fake agent errors the notify).
- [ ] Commit `feat: forkd notifies guests on fork; restore without reseed fails closed`.

### Task 5: KVM CI fork-correctness checks

**Files:** `.github/workflows/kvm-test.yaml`, `cmd/test-agent/main.go`.

- [ ] test-agent gains a `--mode` flag: default current behavior; `notify` mode connects, sends NotifyForked with random entropy, then execs `head -c 32 /dev/urandom | base64` and `date +%s%N`, printing both to stdout for the workflow to capture; also reads `/run/sandbox/fork-generation`.
- [ ] Workflow: extend the agent-test section to restore TWO VMs from the SAME snapshot (the snapshot taken after the agent is up), run test-agent in notify mode against each, and assert in bash: the two base64 urandom samples differ; each guest wall clock is within 2 seconds of the runner clock; fork-generation file content matches the sent generation. Also verify the jailer path: install the jailer binary from the same release archive and run the snapshot restore phase under `--jailer` flags via forkd OR direct jailer invocation mirroring Task 1 args (prefer exercising the engine: build a tiny `cmd/kvm-smoke` ONLY if unavoidable; otherwise script the jailer invocation in the workflow as the existing steps script firecracker directly).
- [ ] Commit `ci: fork-correctness checks, distinct RNG streams and clock resync across forks`.

### Task 6: docs truth pass, full verification, PR

- [ ] `docs/fork-correctness.md`: rows 1 and 2 flip to done with the CI checks named; section 1 and 2 prose updated (virtio-rng device attachment is NOT yet wired; the reseed path is host-entropy-over-vsock; say exactly that and keep a virtio-rng follow-up line). ROADMAP section 1 RNG and clock lines flip accordingly. Threat model untouched beyond Task 2 unless the surface moved again.
- [ ] Full verification: build, vet, lint both GOOS, all Go suites with envtest, Python suite, gofmt, dash grep zero.
- [ ] Push `feat/jailer-fork-correctness`, PR `Jailer plus fork-correctness: RNG reseed and clock resync` body with Closes #2, Closes #5, Closes #6; watch CI (the kvm-test workflow is the load-bearing check here; rerun infra flakes); merge when green per the standing workflow.

**Out of scope:** virtio-rng device wiring (follow-up noted in fork-correctness.md), network identity (#3 epic row 4), per-fork credential reissue, seccomp customization beyond Firecracker defaults.
