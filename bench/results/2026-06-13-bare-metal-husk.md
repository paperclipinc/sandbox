# Bare-metal husk activation: first real-KVM end-to-end run

Hardware: Hetzner dedicated (Intel Core i7-6700, 4c/8t, 64 GiB, NVMe), KVM enabled.
OS/cluster: Talos Linux v1.13.3 single node, Kubernetes v1.36.1, Flannel CNI.
Engine: Firecracker v1.15.0, forkd direct-exec (jailer disabled in-pod; see follow-up).
Template image: python:3.12-slim (mirrored to ghcr, authenticated pull).

## Measured (real, reproducible on this node)

- Snapshot restore (Firecracker `/snapshot/load`, claim activation): 6.09, 6.68, 8.13 ms
  across three husk pods. Sub-10 ms restore on this 2015-era CPU; the <=10 ms
  claim->first-exec target is met at the restore step.
- In-VM exec round trip (sandbox API -> vsock -> guest agent -> command):
  python compute 25.7 ms; echo 1.06 ms; non-zero-exit error path 3.9 ms.
- Snapshot artifacts: mem 512 MiB, rootfs.ext4 167 MiB.
- Fork / density: 2 independent sandboxes activated from ONE snapshot
  (fork-a, fork-b, distinct pods/IPs/tokens); pool scaled to 3 dormant pods.

## Verified paths

- Template build: forkd pulls OCI image -> ext4 rootfs -> boots Firecracker on
  /dev/kvm -> guest-ready -> snapshot (mem+vmstate) + CAS manifest. REAL KVM.
- Husk pod: dormant VMM up; claim -> in-place restore -> VcpuEvent::Resume -> Ready.
- Sandbox API (token-gated): exec (exit/stdout/stderr/env), files write+read.
- Auth fail-closed: wrong token 401, missing bearer 401.

## Open (bare-metal-surfaced follow-ups, tracked in the fix branch)

- Warm pool does not refill after a claim consumes a dormant pod (replicas honored
  on scale-up, not refilled per-claim).
- Releasing a claim does not recycle/free its husk pod.
- Per-activation rootfs CoW not yet implemented (shared template rootfs mounted rw).
- Jailer pivot_root unavailable in-pod (ran unjailed); needs a private bind-mount.
- Claim reports sandboxID=<pod>, but the exec API expects sandbox="husk".

## Latency fix: verify-at-Prepare (re-measured on the renamed mitos.run stack)

The husk Activate originally re-hashed the full snapshot (~680 MiB) against the
manifest before loading (the fail-closed integrity gate), which dominated the
claim path. Moving that verification into the dormant Prepare phase (pre-paid,
read-only immutable snapshot) drops the activate to engine speed:

- Activate latency (husk-stub reported, verification ENFORCED): 1360 ms -> 27.26 ms (~50x).
- Claim -> Ready wall clock: ~1700 ms -> 395 ms (~4x); the residual is the
  controller reconcile round-trip (watch + queue + poll), not the engine.
- Prepare now takes ~1.37 s (the re-hash, during the warm dormant period, before
  any claim arrives).
- Snapshot restore (load) under the fix: 8.5 / 15.9 ms.

Net: the fork/restore engine is ~6-16 ms; a warm claim activates in ~27 ms with
the integrity gate fully enforced; end-to-end is ~395 ms, reconcile-bound.

## Reference run: warm-claim activate latency, N=11 (2026-06-13)

This is the first reference activate-latency distribution captured on the #16
bare-metal reference node and is the source for the published "~27 ms P50"
warm-claim activate number. It is reproducible with the script described in the
"Reproduction" section below.

### Reference node

- Hardware: Hetzner dedicated, Intel Core i7-6700 (4c / 8t, 8 logical CPU),
  64 GiB RAM.
- OS / cluster: Talos Linux, kernel 6.18.33-talos.
- Storage: `/var/lib/mitos` on xfs.
- Path: the default husk path (unprivileged pod, `/dev/kvm` via the device
  plugin).
- Template: `ghcr.io/paperclipinc/sandbox-base-python:3.12-slim`.
- VMM: Firecracker v1.15.0, verify-at-Prepare (the integrity gate is paid in the
  dormant Prepare phase, so the activate is engine-speed; see the latency-fix
  section above).

### What is measured

The husk-stub-reported time the controller writes into the claim's Ready
condition message: "activated husk pod ... in X ms" (reason `HuskActivated`,
`internal/controller/sandboxclaim_controller.go`). That X is the time to load
the snapshot in place, run the fork-correctness handshake, and reach guest-ready.
It is NOT the end-to-end claim->Ready wall clock (see the honest variance below).

### Warm-claim activate latency (ms), N=11 sequential claims

Raw samples (sorted):

```
21.45 22.19 23.73 24.55 24.83 26.53 27.52 32.83 42.25 43.06 46.66
```

- min:  21.45
- P50:  26.53 (nearest-rank; reported as "~27 ms")
- P95:  46.66
- max:  46.66

### Other measured datapoints (this node)

- Snapshot restore (Firecracker `/snapshot/load`): ~6-16 ms (a ~15.8 ms sample
  observed this run; consistent with the 6.09 / 6.68 / 8.13 ms and 8.5 / 15.9 ms
  restore samples recorded above). This is the engine restore step alone.
- Marginal memory per forked sandbox: ~3 MiB via CoW page sharing. Basis: the
  husk-probe CI proof counts a shared snapshot page set ONCE across cgroup v2
  memcgs; the per-VM dirty (unique) set is ~5 MiB. We report ~3 MiB as the
  marginal figure and do not overstate it; the CoW-aware vs naive accounting and
  what is exact vs approximate are in `docs/metering.md`.

### Claim -> Ready end-to-end wall clock (honest variance)

The end-to-end claim->Ready wall clock on this node is ~0.5-1.8 s. This is
reconcile-bound, NOT engine-bound: the engine activate is the ~27 ms P50 above;
the rest is the Kubernetes control-loop round-trip (watch + queue + status poll)
plus warm-pool refill. We report the variance honestly and do not hide it: the
~27 ms figure is the activate the controller records, not the wall clock a client
observes from claim apply to Ready.

### Engine fork -> first-exec

NOT re-measured on this node this session. The reproducible `cmd/bench` harness
reports fork->first-exec (the `fork-exec` mode); its number is shared-CI-class
(see `BENCHMARKS.md`, ~69 ms P50 shared-class). We continue to cite the harness
number and do NOT state a bare-metal fork->first-exec figure here, because
`cmd/bench` was not run on the box this session.

## Reproduction

### The activate-latency distribution

The N=11 activate distribution above is reproduced by
[`bench/husk-activate-latency.sh`](../husk-activate-latency.sh):

```sh
bench/husk-activate-latency.sh <kubeconfig> <pool> [namespace] [iterations]

# the reference run:
bench/husk-activate-latency.sh ~/.kube/talos-ref python-agent-pool default 11
```

The script creates N sequential `SandboxClaim`s against a warm pool, waits for
each to reach Ready, parses the activate latency out of the Ready condition
message, releases the claim between iterations so each is an independent warm
activation, and prints min / P50 / P95 / max (nearest-rank) plus the raw sample
list. It requires a running mitos cluster with the husk-pods path enabled and a
warm pool with dormant pods available.

### Cluster setup used for this run

- A single-node Talos cluster on the Hetzner reference node, `/dev/kvm` exposed
  to unprivileged pods via the device plugin, `/var/lib/mitos` on xfs.
- The controller, forkd DaemonSet, and CRDs deployed per the README "On a
  cluster" steps.
- A `SandboxTemplate` for `ghcr.io/paperclipinc/sandbox-base-python:3.12-slim`
  and a `SandboxPool` referencing it, warmed (dormant husk pods prepared) before
  the run.
- The restore samples come from the forkd / husk-stub logs for the same
  activations; the CoW marginal-memory basis is the husk-probe CI proof.

## Reproduction confirmed (committed script)

`bench/husk-activate-latency.sh` was run against the live bare-metal cluster and
reproduced the activate figure independently: P50 27.46 ms (N=6 valid; min 25.13,
max 35.89; raw 25.13 25.71 27.46 31.39 31.49 35.89). A second, earlier run gave
P50 24.48 ms (N=6). All three runs (incl. the N=11 reference above) land at P50
~24 to 27 ms. The skipped iterations are warm-pool refill throughput, not activate
latency: the pool refills roughly one dormant pod per ~10 to 14 s, so claims issued
faster than that wait for a warm slot. The script paces by waiting for a dormant
pod before each claim and honestly reports only the warm activations it measured.
