# Husk pods

This document covers the husk-pods execution model (issue #18, workstream W1),
the load-bearing memory-sharing claim the model rests on, the cgroup v2 memory
charging behavior that makes that claim hold, the measured CI proof, the honest
first-faulter accounting nuance, and what this work proves today versus what the
full epic still needs.

> Honest scope, stated up front: sandboxes are NOT pods today. forkd owns VMs
> directly; the controller does not create a pod per sandbox. This document and
> the CI phase it describes do NOT change that. They verify the single
> precondition issue #18 demanded we confirm FIRST, before any controller
> migration: that copy-on-write page sharing across forks of one snapshot
> survives being placed in separate cgroup v2 memory controllers (per-pod
> memcgs). The rest of the migration is enumerated under "Proven vs remaining".

## 1. The husk-pods architecture (issue #18)

Today a sandbox VM is a Firecracker process forkd launches and tracks directly.
The husk-pods model moves every sandbox VM inside a Kubernetes pod so the VM
inherits real pod semantics: the scheduler sees it, ResourceQuota/LimitRange
bound it, NetworkPolicy (Cilium) governs its netns, and PSA can hold the
namespace to `restricted` (with exactly one documented device exception).

The shape:

- **Pre-scheduled husk pods.** A pool pre-schedules minimal pods, each running a
  dormant VMM stub on a vsock or unix control channel. The stub holds no VM yet;
  it has been scheduled, admitted, and placed, so the expensive Kubernetes work
  (scheduling, admission, netns, cgroup creation) is already paid for.
- **Claim = activate.** Claiming a sandbox activates a husk: the stub mmaps the
  template snapshot and performs a KVM restore INSIDE the pod's own cgroup and
  netns. The VM's memory is therefore charged to the pod's memcg and its traffic
  rides the pod's network namespace.
- **/dev/kvm via a device plugin, not `privileged: true`.** KVM access is
  granted through a Kubernetes device plugin that exposes `/dev/kvm` to the pod,
  so the pod is NOT privileged. This is the one documented PSA-restricted
  exception (an ADR under `docs/adr/`), not a blanket privilege grant.
- **Overflow beyond the warm pool.** A claim that exceeds the warm husk pool is
  served by scheduling a fresh husk pod: seconds rather than the warm-pool
  target, a degraded but correct mode. `pendingClaims` is the autoscaling signal
  that drives the pool to grow.

The density argument for this model is that many husk pods on one node share the
SAME template snapshot's clean page set, so the marginal memory of an additional
activated husk is its private-dirty divergence, not a whole VM. That argument is
only valid if page sharing survives the per-pod memcg boundary. That is the
load-bearing claim.

## 2. The load-bearing analysis: CoW sharing across cgroup v2 memcgs

### How the shared snapshot pages are mapped

Every fork of one template restores the SAME snapshot memory file with
`MAP_PRIVATE` (Firecracker mmaps the snapshot `mem` file). MAP_PRIVATE clean
pages are backed by the page cache for that file and are physically shared by
every process that maps them, until a process writes one. A write triggers
copy-on-write: the kernel allocates a fresh anonymous page for the writer and
leaves every other mapper on the original shared page. So at any instant a
fork's resident set splits into:

- a **clean, shared** portion: snapshot pages no fork has written, one physical
  copy backing all forks; and
- a **private-dirty** portion: pages this fork wrote after restore, an anonymous
  page unique to this fork.

### The cgroup v2 memory charging model

cgroup v2 charges memory to a memcg on the page-fault that brings a page
resident, per these rules that matter here:

- **First-faulter charging.** When a file-backed (page-cache) page is first
  faulted resident, it is charged to the memcg of the faulting task. A different
  memcg whose task later reads the SAME already-resident page is NOT re-charged:
  the page is already accounted, and a shared read does not duplicate the charge.
- **MAP_PRIVATE clean pages stay shared.** A clean MAP_PRIVATE page is the
  shared page-cache page; it is charged once (to whoever faulted it in) and read
  by all other mappers without additional charge.
- **CoW writes allocate an anon page charged to the writer.** When a fork writes
  a clean shared page, the kernel allocates a new anonymous page and charges it
  to the WRITER's memcg. The original shared page stays put, still charged to
  its first-faulter, still shared by the non-writers.

### Why CoW sharing survives a per-pod memcg

Putting each fork's Firecracker process in its own memcg changes WHO is charged
for a page; it does not change WHETHER the page is physically shared. The memcg
is an accounting boundary, not a copy boundary. The clean snapshot pages remain
one physical copy in the page cache shared across all forks regardless of which
memcgs their mappers belong to. Only a CoW write creates a new (anonymous) page,
and that page is genuinely private to the writer and correctly charged to the
writer's memcg. So:

- the shared clean snapshot set exists physically ONCE no matter how many
  per-pod memcgs map it; and
- each fork's private-dirty divergence is a distinct set of pages charged to
  that fork's own memcg.

This is exactly the property the husk-pods density argument needs: per-pod
memcgs do not multiply the shared snapshot footprint.

### The measured result (CI husk-probe phase)

The claim is verified, not asserted. The KVM integration workflow
(`.github/workflows/kvm-test.yaml`) runs a `husk-probe` phase that:

1. builds one template snapshot (reusing the bench template),
2. forks 4 real sandboxes from it via the KVM-backed engine,
3. places each fork's Firecracker PID in its OWN cgroup v2 memory controller
   under `/sys/fs/cgroup/husk-probe/vm-<i>` (enabling `+memory` in the root and
   the probe subtree so the leaf memcgs account memory), and
4. samples each fork's `/proc/<pid>/smaps_rollup` and each memcg's
   `memory.current` / `memory.stat`, then runs `internal/huskprobe.Analyze`.

`Analyze` produces a CoW-aware `Report`:

- `NaiveSum` is every fork's full RSS summed, the non-CoW-aware charge.
- `SharedResident` is the snapshot clean resident set, counted ONCE (the max
  over forks of `Rss - PrivateDirty`, the conservative representative).
- `TotalPrivateDirty` is the sum of every fork's own private-dirty pages.
- `AggregatePhysical = SharedResident + TotalPrivateDirty` is the honest
  physical footprint.
- `CoWSavings = NaiveSum - AggregatePhysical`.
- `CoWSurvives` is true when sharing materially lowered the footprint (the
  honest footprint is at least one whole `SharedResident` below the naive sum),
  which is only possible if the shared snapshot set was counted once across the
  separate memcgs.
- `DirtyPerVM` is true when every fork has its own non-zero private dirty.

The CI phase gates on `CoWSurvives == true`, `AggregatePhysical < NaiveSum` by
at least one `SharedResident` (a material margin), and `DirtyPerVM == true` with
every fork's `PrivateDirty > 0`. The exact NaiveSum, AggregatePhysical,
SharedResident, TotalPrivateDirty, CoWSavings, and per-fork private dirty are
published to that run's `$GITHUB_STEP_SUMMARY`. They are SHARED-CI-CLASS numbers:
`ubuntu-latest` is a noisy, oversubscribed, often nested-virt runner, so the
absolute values are reproducible per run but are NOT bare-metal figures. The
verdict reported there, and the property this PR claims, is `CoWSurvives`: the
shared snapshot pages are counted once across the four cgroup v2 memcgs, not four
times, while each fork's private dirty is charged to its own memcg. The
conclusion: the load-bearing precondition of husk pods holds; the design stands
on this point.

If a future run ever reports `CoWSurvives = false`, the phase fails loudly as a
`HUSK-DESIGN-FAILED` result (distinct from a `HUSK-SETUP-LIMITATION`, which is a
runner-class cgroup restriction that could not measure). `CoWSurvives = false`
would mean CoW does NOT survive the per-pod memcg boundary and the husk-pods
density argument would need rethinking; this document would be updated to report
that.

## 3. Prepare and activate: the dormant-VMM stub

The husk-pods model splits a sandbox's bring-up into two phases with very
different costs. `internal/husk` (driven by `cmd/husk-stub`) is the stub that
implements that split today, ahead of the controller migration.

- **Prepare (pre-claim, off the hot path).** The stub brings up a DORMANT
  Firecracker VMM: the `firecracker` process and its API socket are up, but no
  snapshot is loaded and no guest is running (`internal/husk` `StateDormant`).
  In production this happens when a husk pod is pre-scheduled into the warm pool,
  so the expensive work, scheduling, admission, netns and cgroup creation, and
  spawning the VMM process itself, is already paid for before any claim arrives.
- **Activate (claim time, the only cost paid on the hot path).** A claim sends
  one `ActivateRequest{SnapshotDir, NetworkOverrides}` over the stub's control
  socket (a line-delimited JSON protocol). The stub does
  `LoadSnapshotWithOverrides` against the already-running VMM, remapping the
  baked NIC to this husk's tap, resumes the VM in place, and waits for the guest
  agent to answer over vsock before replying `ActivateResult{OK, VsockPath,
  LatencyMs}`. Because the VMM process was pre-started, the claim-time cost is
  the in-place snapshot load + resume + guest-ready handshake, NOT a VMM spawn.

The stub FAILS CLOSED: a snapshot-load or guest-readiness failure returns
`OK=false` with actionable error text and leaves the husk NOT active. It never
reports a usable VM it could not verify over vsock. One stub owns exactly one
VM, so a successful activate is terminal for that stub.

### Measured activation latency (CI husk-stub phase)

The KVM integration workflow runs a `husk-stub` phase that proves the split and
measures the activation cost. It reuses the bench template snapshot, then for
each iteration: starts a fresh dormant stub (prepare), runs the
`husk-stub --activate` control client to activate that snapshot in place,
asserts the `ActivateResult` is `OK`, and on the first iteration execs a real
command through the guest agent over the returned `VsockPath`. The gate is
**activate OK AND exec works through the activated VM**, not merely a control
reply. The phase publishes nearest-rank P50/P99 of the stub-measured
`LatencyMs` (load-start to guest-ready) to that run's `$GITHUB_STEP_SUMMARY`.

These are SHARED-CI-CLASS numbers: `ubuntu-latest` is a noisy, oversubscribed,
often nested-virt runner, so the absolute values are reproducible per run but
are NOT bare-metal figures. The **<= 10ms warm activation figure is the
bare-metal reference-node TARGET (#18/#15), not a shared-CI claim**; the CI
phase does not assert it and it must not be quoted as achieved. The honest claim
this phase supports is narrow: the prepare/activate split works, an in-place
snapshot load activates a usable VM, and the claim-time cost is the activation
alone (the measured latency, with its shared-CI caveat), not a VMM spawn.

### Fork-correctness is NOT yet wired into activate

A correct fork delivers fresh per-activation entropy (RNG reseed), resyncs the
guest wall clock, and delivers per-claim secrets. That is the engine's
`NotifyForked` handshake (see [docs/fork-correctness.md](fork-correctness.md));
the fork/daemon path drives it today. The husk stub's `Activate` is the RESTORE
mechanism only: it loads and resumes the snapshot and confirms the guest
answers. It does NOT yet send `NotifyForked`, so it does not yet reseed the RNG,
resync the clock, or deliver secrets per activation. Wiring the fork-correctness
handshake into the stub's activate path is an integration follow-up and is
REQUIRED before pod-native bring-up can become the default; this PR does not
silently handle it, and the gap is tracked under "Remaining" below.

## 4. The honest nuance: first-faulter charging is not fair per-tenant accounting

CoW sharing surviving the memcg boundary is what the density argument needs, but
it does NOT by itself give fair per-tenant memory accounting. Raw cgroup v2
charges each shared snapshot page to the FIRST pod that faulted it resident, in
full, and never re-charges the other pods that share it. So if pod A activates
first, A's `memory.current` carries the entire shared snapshot set and pods B, C,
D that share those exact pages appear to carry almost none of it. The shared set
is charged once (good for the node's total), but it is charged to ONE tenant, not
split across the sharers (unfair if `memory.current` is read as a per-tenant
bill). This is visible in the probe: the per-memcg `memory.current` values are
lopsided even though the physical sharing is real, which is precisely why the
smaps-derived split, not raw `memory.current`, is the source of truth for the
report.

Fair per-tenant memory accounting therefore does NOT use raw `memory.current`.
It uses the CoW-aware metering (issue #33, the shared-once model): the shared
restored page set is counted once and attributed as shared, and each tenant is
billed its own unique (private-dirty) set plus a share of the common set, rather
than whichever tenant happened to fault the page first. See
[docs/metering.md](metering.md) for the CoW-aware accounting model, its exact
versus approximate boundaries, and the `cmd/bench --mode metering` CI proof that
the shared template set is counted exactly once across forks. Husk pods inherit
that metering: the per-pod memcg is the right enforcement boundary
(`memory.max` per pod) and a useful signal, but the per-tenant BILL comes from
the CoW-aware metering, not the first-faulter `memory.current`.

## 5. Proven vs remaining

### Proven so far

- CoW page sharing survives cgroup v2 memory-controller boundaries: forks of one
  snapshot in separate per-pod memcgs share the clean snapshot set physically
  (counted once, not once per memcg) while each fork's private dirty is charged
  to its own memcg. This is the precondition issue #18 demanded be verified
  FIRST, measured by a real KVM probe in CI and gated on `CoWSurvives`.
- The prepare/activate split: the dormant-VMM stub (`internal/husk`,
  `cmd/husk-stub`) and its line-delimited JSON control protocol pre-start a
  Firecracker VMM and activate it in place via snapshot-load + resume +
  guest-ready on a control message. The KVM CI husk-stub phase measures the
  activation latency (load-start to first exec, shared-CI-class) and gates on
  activate OK plus a working exec through the activated VM. Fail-closed on a
  failed load.

### Remaining (the rest of issue #18, follow-up PRs)

This PR does NOT migrate any controller and does NOT make sandboxes pods. The
full husk-pods epic still needs:

- the `/dev/kvm` device plugin (exposing KVM to the pod instead of
  `privileged: true`);
- running the stub INSIDE a real husk pod (the pod spec, the device-plugin
  resource request, the cgroup/netns placement); the stub binary and its control
  protocol exist, but nothing runs it in a pod yet;
- wiring the fork-correctness handshake into the stub's activate path
  (per-activation RNG reseed, clock resync, secret delivery via `NotifyForked`);
  the stub's `Activate` is the restore mechanism only and does NOT yet do this,
  and it is REQUIRED before pod-native bring-up becomes the default;
- migrating the pool/claim/fork controllers to create + activate husk pods (with
  raw-forkd mode kept behind a flag and pod-native as the default);
- the conformance suite, each acceptance criterion a test: scheduler truth,
  ResourceQuota/LimitRange, NetworkPolicy/Cilium over the pod netns, PSA
  `restricted` minus the documented device-plugin exception, `kubectl get pods`,
  and eviction/preemption/PDB/drain behavior;
- the bare-metal P99 claim-to-first-exec <= 10ms warm-pool benchmark
  (before/after); the shared-CI activation latency is not this number;
- the re-derived threat model for the unprivileged-stub escape surface (the
  dormant VMM activating a VM inside the pod is a new boundary;
  [docs/threat-model.md](threat-model.md) must be re-derived in the migration
  PRs that introduce the stub and device plugin).

Until those land, sandboxes remain forkd-owned VMs, not pods. This PR moves the
epic forward by removing the single largest uncertainty: that the memory-sharing
the model depends on does not evaporate at the memcg boundary.
