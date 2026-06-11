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
| 1 | Shared RNG state after restore | Reseed CRNG on every fork via host entropy over vsock (NotifyForked) | go: `TestForkNotifiesAgentWithFreshEntropy`, `TestForkGenerationIncrementsAcrossForks`, `TestForkFailsWhenNotifyForkedErrors`; KVM: two forks of one snapshot assert distinct `/dev/urandom` (`URANDOM` lines differ) | **partial** (guest reseed + forkd notify done; virtio-rng device attachment NOT wired; KVM proof is guest-only, see CI job) |
| 2 | Stale wall clock after restore | kvm-clock resync + agent clock step from host wall clock in NotifyForked | go: `TestForkNotifiesAgentWithFreshEntropy` (carries `HostWallClockNanos`); KVM: each fork `WALLCLOCK_NS` within 2s of the runner clock | **partial** (guest clock step done, 500ms tolerance; KVM proof is guest-only, see CI job) |
| 3 | Secrets duplicated into live forks | Per-fork credential reissue; inheritance requires opt-in | `TestLiveForkOfSecretHolderIsRejectedByDefault`, `TestForkDeliversConfigureToAgent`, KVM `test-agent` configure check | **partial** (default-deny gate + vsock delivery implemented; reissue open) |
| 4 | Duplicate MAC/IP/TCP state in forks | Fresh NIC identity per fork; parent TCP dead in fork | `TestForkNetworkIdentity` | **open** (guests currently have no NIC at all; see note) |
| 5 | Misleading memory accounting | Report lifetime unique bytes, not just T=0 dirty pages | `TestMemoryAccountingLifetime` | **partial** (smaps_rollup sampling exists at fork time only, `internal/fork/engine.go:readMemoryStats`) |
| 6 | Incompatible snapshot restored (crash/corruption) | Refuse on load: the manifest records the producing environment (format version, Firecracker version, CPU model, kernel, config hash); require exact VMM match, exact CPU-model match, format version in the supported set (kernel informational); `--allow-incompatible-snapshots` dev escape hatch | go: `internal/snapcompat` `TestCheck*`, `internal/fork/compat_test.go`; KVM: record a real manifest, assert compatible passes and a VMM / format-version mismatch is refused | **done** (load-gate enforcement after the digest verify, before any Firecracker launch; CPU templates + live cross-version restore open) |

## 1. RNG and entropy after restore

Every VM restored from the same snapshot wakes up with byte-identical kernel
CRNG state, identical userspace PRNG state in any already-started runtime, and
identical TLS library state. Consequences: colliding UUIDs, predictable session
tokens, identical TLS ClientHello randoms, broken nonce-based crypto.

Implemented reseed path: the host delivers fresh entropy over vsock. forkd
calls `NotifyForked(generation, entropy)` immediately after restore
(`internal/daemon/server.go:notifyForked` generates a fresh generation plus 32
bytes of `crypto/rand` entropy). The guest agent on `NotifyForked` writes that
entropy into the kernel CRNG via `RNDADDENTROPY`, records the generation at
`/run/sandbox/fork-generation`, and signals userspace runtimes
(`guest/agent/notifyforked.go`). VMGenID is not exposed by Firecracker, so this
host-entropy-over-vsock hook is our equivalent.

**Follow-up (not wired):** a virtio-rng device attached to every restored VM
backed by host entropy is NOT implemented; the current path injects entropy
only at fork time via NotifyForked, not continuously. Tracked as a follow-up.

Tests. go (`internal/daemon`): `TestForkNotifiesAgentWithFreshEntropy` asserts
forkd sends entropy, `TestForkGenerationIncrementsAcrossForks` asserts distinct
generations across forks, `TestForkFailsWhenNotifyForkedErrors` asserts a
real-engine fork fails closed when the guest cannot reseed.

KVM (`kvm-test.yaml`): one snapshot is taken after the agent is up, two VMs are
restored from it, and `test-agent --mode notify` runs against each. The phase
asserts the two `URANDOM=` base64 samples differ (equal would be the shared-RNG
bug). This proves the GUEST applies the reseed; forkd end-to-end notify is
covered by the go tests above. The N=8 / `uuid.uuid4()` / TLS-ClientHello
variants remain a follow-up.

## 2. Clock correctness

A restored guest's wall clock is frozen at snapshot time. TLS certificate
validation (`notBefore`) and JWT `iat`/`exp` checks fail silently or, worse,
pass when they should fail.

Implemented clock step: `NotifyForked` carries `HostWallClockNanos`, stamped by
the host at send time (`internal/vsock/client.go`). The guest agent reads it
and calls `clock_settime(CLOCK_REALTIME)` when drift exceeds a 500ms tolerance,
then signals userspace as in section 1 (`guest/agent/notifyforked.go`). kvm-clock
remaining the active clocksource and Firecracker's restore path updating it is
relied on but not separately asserted here.

Tests. go: `TestForkNotifiesAgentWithFreshEntropy` covers that forkd sends the
notification carrying the host wall clock.

KVM (`kvm-test.yaml`): each restored fork's `WALLCLOCK_NS` (from in-guest
`date +%s%N`) is asserted within 2 seconds of the runner clock. This proves the
GUEST holds a correct wall clock after restore. The post-snapshot TLS-cert
validation variant remains a follow-up.

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

### vsock UDS identity (device-identity-after-fork)

The host-side vsock device is itself a device-identity hazard. Firecracker
bakes the vsock `uds_path` string verbatim into the snapshot and rebinds that
exact path on every restore. If a template were snapshotted with an *absolute*
`uds_path`, every fork of that one snapshot would try to bind the same host
socket: the second and later restores fail with `VsockUnixBackend: Error
binding to the host-side Unix socket: Address in use (os error 98)`, and a
lingering socket file from a killed source VM blocks even the first restore.

Handling: template snapshots bake a *relative* `uds_path`
(`firecracker.VsockRelPath` = `"vsock.sock"`). A relative path is resolved by
each restored Firecracker process against its own working directory, so an
identical baked path plus a distinct per-VM cwd yields a distinct host socket
and forks never collide. In raw direct-exec (`forkd`) mode the working
directory is the per-sandbox `WorkDir`, set as the Firecracker process's
`cmd.Dir` in `internal/firecracker/client.go:StartVM`; the engine reports the
resolved socket via `Client.VsockHostPath`. Under the jailer the chroot already
isolates each VM (the relative path resolves against the per-VM chroot root), so
the hazard is moot there; raw mode is the one that depends on the relative path.
The invariant is locked in by `TestVsockHostPathPerCwd`
(`internal/firecracker/jailer_test.go`) and exercised end to end by the
`fork-correctness` CI phase, which restores two VMs from one snapshot in
separate working directories and asserts both bind distinct sockets and answer.

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

## 6. Snapshot compatibility on restore

A Firecracker snapshot is not portable across arbitrary hosts. Restoring memory
and device state captured under a different Firecracker version or on a
different CPU model is a crash or silent-corruption hazard: the guest can fault,
hang, or run with subtly wrong CPU feature assumptions. The snapshot format
itself can also change incompatibly between builds.

Contract fields. Every manifest records the producing environment as part of its
content-addressed identity (`internal/cas.Manifest`):

- `SnapshotFormatVersion` (current = `cas.CurrentSnapshotFormatVersion` = 1)
- `VMMVersion` (the producing Firecracker version)
- `CPUModel` (the host CPU model)
- `KernelVersion` (the guest kernel; informational)
- `ConfigHash` (the microvm machine config the snapshot was captured under)

Policy (`internal/snapcompat.Check`). A restore is refused unless the format
version is in the set this build supports, the Firecracker version matches
exactly, and the CPU model matches exactly. A kernel mismatch is informational
when the rest match (the guest kernel is baked into the snapshot image). The
check is part of the load gate: it runs after the content-addressed digest
verify and before any Firecracker launch, so an incompatible snapshot never
reaches a VM. Refusals carry an actionable message (rebuild the template on this
node, or schedule the fork on a matching node). A `--allow-incompatible-snapshots`
dev escape hatch logs loudly and proceeds; it is for development only.

Open: Firecracker CPU templates would relax the exact-CPU-model rule for a
defined family; live cross-Firecracker-version restore testing needs two FC
versions in CI. Both are tracked as follow-ups; the contract refuses them today.

See docs/snapshot-format.md for the full format-version and migration policy.

## CI job

`kvm-test.yaml` (GitHub Actions, KVM-capable runner) runs the RNG and clock
proofs above on every PR touching `internal/firecracker/`, `internal/fork/`,
`guest/`, `cmd/test-agent/`, or `internal/vsock/`. It takes one snapshot after
the agent is up, restores two VMs from it, and asserts distinct `/dev/urandom`,
each wall clock within 2s of the runner, and the fork-generation file matching
the generation sent. A jailer-boot phase restores the same snapshot under the
jailer to prove the chroot/uid mechanics (it does not prove the dropped
capability set, since the runner is root; that sub-step is `continue-on-error`,
see issue #2).

Until every row above is `done`, fork correctness is the top engineering
priority and blocks feature work (see `ROADMAP.md`).
