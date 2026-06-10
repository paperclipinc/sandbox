# Fork-engine correctness

The CoW snapshot-fork is the product. It must be correct before it is fast.
This document enumerates every known correctness hazard of restoring multiple
VMs from one snapshot, the policy we have chosen for each, the test that
verifies it, and its current status.

**Status legend:** `open` = not implemented, no test. `partial` = implemented,
test missing or incomplete. `done` = implemented + test runs in the
`fork-correctness` CI job.

| # | Hazard | Policy | Test | Status |
|---|--------|--------|------|--------|
| 1 | Shared RNG state after restore | Reseed CRNG on every fork | `TestForkDistinctRandomness` | **open** |
| 2 | Stale wall clock after restore | kvm-clock resync + agent clock step | `TestForkClockCorrectness` | **open** |
| 3 | Secrets duplicated into live forks | Per-fork credential reissue; inheritance requires opt-in | `TestLiveForkOfSecretHolderIsRejectedByDefault`, `TestForkDeliversConfigureToAgent`, KVM `test-agent` configure check | **partial** (default-deny gate + vsock delivery implemented; reissue open) |
| 4 | Duplicate MAC/IP/TCP state in forks | Fresh NIC identity per fork; parent TCP dead in fork | `TestForkNetworkIdentity` | **open** (guests currently have no NIC at all; see note) |
| 5 | Misleading memory accounting | Report lifetime unique bytes, not just T=0 dirty pages | `TestMemoryAccountingLifetime` | **partial** (smaps_rollup sampling exists at fork time only, `internal/fork/engine.go:readMemoryStats`) |

## 1. RNG and entropy after restore

Every VM restored from the same snapshot wakes up with byte-identical kernel
CRNG state, identical userspace PRNG state in any already-started runtime, and
identical TLS library state. Consequences: colliding UUIDs, predictable session
tokens, identical TLS ClientHello randoms, broken nonce-based crypto.

Required implementation:

- virtio-rng device attached to every restored VM, backed by host entropy.
- A fork-generation signal the guest can observe (VMGenID is not exposed by
  Firecracker; our equivalent is a guest-agent hook: forkd calls
  `NotifyForked(generation)` over vsock immediately after restore).
- Guest agent on `NotifyForked`: write fresh entropy from virtio-rng into
  `/dev/urandom` via `RNDADDENTROPY`, then deliver a userspace signal
  (configurable: SIGUSR2 to session leader, or an inotify-able generation file
  at `/run/sandbox/fork-generation`) so language runtimes and TLS libraries can
  regenerate state.

Test (`fork-correctness` CI job): fork N=8 sandboxes from one snapshot and
assert pairwise-distinct: (a) 1KB reads from `/dev/urandom`, (b) UUIDs from
Python `uuid.uuid4()` in a runtime started *before* snapshot, (c) TLS
ClientHello random captured from an in-guest `openssl s_client` against a local
listener.

## 2. Clock correctness

A restored guest's wall clock is frozen at snapshot time. TLS certificate
validation (`notBefore`) and JWT `iat`/`exp` checks fail silently or, worse,
pass when they should fail.

Required implementation:

- Verify kvm-clock is the active clocksource in the guest kernel config and
  that Firecracker's snapshot restore path updates it.
- Guest agent on `NotifyForked`: read host wall time delivered in the
  notification payload, `clock_settime(CLOCK_REALTIME)` if drift exceeds
  tolerance, then SIGHUP/notify as in §1 so chrony-like daemons (if any) and
  userspace re-read time.

Test: snapshot a VM, wait ≥10s, fork, immediately exec `date +%s%N` and assert
within 500ms of host time. Also exec a TLS handshake against a cert issued
*after* the snapshot was taken; it must validate.

## 3. Live-fork memory hygiene (secrets)

`SandboxFork` of a running sandbox duplicates everything in guest memory,
including claim-time secrets, into every fork.

**Chosen policy (default-safe):** live forks of a sandbox that holds claim-time
secrets are **rejected** unless one of:

- `spec.allowSecretInheritance: true` is set on the `SandboxFork` (explicit
  opt-in, recorded in the fork's status), or
- the platform implements revoke-and-reissue for the secret class in question
  (each fork receives fresh credentials over vsock post-restore; the parent's
  copies that leaked into fork memory are revoked upstream). This is the
  long-term default; rejection is the stopgap.

The default-deny gate plus opt-in audit trail is implemented in the fork
controller (`internal/controller/sandboxfork_controller.go`): forks of
secret-holding sandboxes get a terminal typed `Rejected` condition without
`spec.allowSecretInheritance: true`, and explicit opt-ins are recorded as an
audit condition. Secret *delivery* is implemented too: the controller resolves
Secret refs (`internal/controller/sandboxclaim_controller.go:resolveSecrets`)
and forkd delivers them over vsock post-restore
(`internal/daemon/server.go:deliverConfig`); never baked into snapshots,
never in Firecracker boot args or the FC API socket request log. Per-fork
credential reissue remains open.

Test: claim with a secret, exec to confirm the secret is visible in the parent,
fork without opt-in → typed `Rejected` condition; fork with opt-in → secret
present and an audit annotation recorded.

## 4. Network identity after fork

Forked guests must not wake up with the parent's MAC/IP/TCP state.

**Current reality:** restored VMs have *no* network device: no NIC is attached
anywhere in `internal/fork/engine.go` or `internal/firecracker/template.go`,
and exec/files run over vsock. There is therefore no collision today, but also
no egress at all; the README's egress-allowlist feature is unimplemented.

Required design (when guest networking lands):

- Snapshot templates are taken with the NIC detached (or with a stub device),
  so restored memory holds no live TCP state tied to an address.
- On fork: attach a tap device with a freshly generated MAC, assign a unique
  IP from the node-local sandbox subnet, host-side conntrack entries flushed
  for that IP before the guest resumes.
- Parent's open TCP connections must be dead in the fork: the fork has a
  different IP, and the guest agent's `NotifyForked` hook brings the interface
  down/up so in-guest sockets error out promptly rather than half-living.
- Document for users: any in-flight connection at fork time is broken in the
  fork by design; reconnect logic belongs in the workload.

Test: parent holds an open TCP connection to a host-side echo server; fork;
assert in the fork that (a) MAC and IP differ from parent, (b) writing to the
inherited socket fd fails within 5s, (c) parent's connection still works.

## 5. Memory accounting truthfulness

"~KB per fork" measured at T=0 is the dirty-page count immediately after
restore. It is not a density planning number; a `pip install` in the fork
makes pages unique.

Required implementation:

- `agentrun_memory_unique_bytes` (per-sandbox label) sampled periodically over
  the sandbox lifetime, not only at fork time. The current implementation
  samples `/proc/<pid>/smaps_rollup` once, at fork (`readMemoryStats`).
- Published density numbers must include unique-memory-after-representative-
  workload (e.g., after `pip install numpy && python -c "import numpy"`)
  alongside the T=0 number, produced by `bench/`.

Test: fork, record T=0 unique bytes; run a write-heavy workload; assert the
exported metric grows accordingly and that `GetCapacity` reflects it.

## CI job

`fork-correctness` (GitHub Actions, KVM-capable runner) runs all tests above on
every PR touching `internal/fork/`, `internal/firecracker/`, or `guest/`.
**Status: job not yet created**; it lands with the first test in this list.
Until every row above is `done`, fork correctness is the top engineering
priority and blocks feature work (see `ROADMAP.md`).
