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

### Fork-correctness on activate

A correct fork delivers fresh per-activation entropy (RNG reseed), resyncs the
guest wall clock, and delivers per-claim secrets. That is the engine's
`NotifyForked` handshake (see [docs/fork-correctness.md](fork-correctness.md));
the fork/daemon path drives it, and now the husk stub's `Activate` runs the SAME
handshake. After the snapshot loads, resumes, and the guest answers, `Activate`:

- generates fresh `crypto/rand` entropy and an incrementing generation, then
  sends `NotifyForkedWithConfig` so the guest reseeds its CRNG, steps its wall
  clock forward off the frozen snapshot time, and re-addresses its NIC;
- delivers the claim-time env and secrets via `Configure`, in the same order the
  daemon's `deliverConfig` uses (notify first, then env/secrets);
- FAILS CLOSED: a connect/handshake error, or a guest that does not report
  `ReseededRNG`, leaves the VM NOT active and unserved. A VM that did not reseed
  is never reported as usable. Entropy and secret VALUES are never logged.

This is CI-proven on the KVM runner: the husk activate-correctness phase
activates two VMs from ONE bench template snapshot via two fresh dormant stubs
and asserts, mirroring the fork-correctness suite, distinct RNG streams across
the two activations (equal urandom is a real correctness failure, not a flake),
each guest wall clock within 2s of the runner (the clock stepped), and an env
var plus a secret delivered at activate readable in each guest while the secret
value is absent from every host-side stub/client log. See
[docs/fork-correctness.md](fork-correctness.md).

This PR proves the stub CAN apply claim-time config and reseed per activation.
The remaining #18 work is the controller migration, which includes sourcing the
claim-time secrets and env from the controller (today the stub's `--activate`
client carries them over the local control socket); see "Remaining" below.

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

## 5. Device plugin (/dev/kvm without privileged)

A husk pod needs `/dev/kvm` to restore and run its VM, but mounting the device
the way forkd does today (a `hostPath` to `/dev/kvm` plus a permissive
capability set) is incompatible with the PSA `restricted` profile husk pods
target. The Kubernetes device-plugin mechanism is the restricted-profile path:
the pod requests `agentrun.dev/kvm` like any extended resource and the kubelet,
not the pod spec, injects the device.

`cmd/kvm-device-plugin` (implemented in `internal/deviceplugin`) is that plugin.
It runs as a DaemonSet on every node and implements the v1beta1 `DevicePlugin`
gRPC service:

- **Scheduler truth.** `ListAndWatch` advertises `--device-count` healthy slots
  (synthetic ids `kvm-0..kvm-{N-1}`) ONLY where the host `/dev/kvm` exists, and
  ZERO where it does not. A node without `/dev/kvm` advertises nothing, so the
  scheduler never places a pod that requests the resource there. `/dev/kvm` is
  shareable, so the slot count is a soft per-node concurrency cap on husk pods,
  not a count of physical devices. The DaemonSet runs everywhere (no
  `nodeSelector`) on purpose: a node that gains `/dev/kvm` is covered
  automatically on the plugin's next registration, with no relabel.
- **Device injection.** `Allocate` returns a `DeviceSpec` per configured device
  path (`/dev/kvm` and `/dev/net/tun`), each mapped host-path to the same
  container-path with `rw` permissions, so the kubelet bind-mounts the device
  nodes into the admitted container. The pod never sets `privileged: true` and
  carries no `/dev/kvm` `hostPath` of its own.
- **Registration.** The Registrar serves the plugin on a unix socket under the
  kubelet device-plugins dir and registers with the kubelet (Version, Endpoint,
  ResourceName), re-registering on failure so a kubelet restart is recovered.

The DaemonSet (`deploy/device-plugin/daemonset.yaml`) needs only minimal
privileges, NOT forkd's privileged set: it runs as root because the kubelet
device-plugins dir is root-owned (it must write its socket there), but it is
unprivileged with all capabilities dropped, `allowPrivilegeEscalation: false`
and a read-only root filesystem. Its only host access is the kubelet
device-plugins dir (to serve and register) and a read-only `/dev` mount (to
`stat /dev/kvm`); it creates no device nodes and starts no VMs. So a husk pod
requesting `agentrun.dev/kvm` is PSA-restricted minus exactly the documented
device-plugin exception, not a privileged escape.

This is the PSA-restricted alternative to forkd's current privileged
`/dev/kvm` hostPath. It does NOT remove that hostPath: migrating the forkd
DaemonSet to request the resource instead of mounting the device is a follow-up.

### Proven vs remaining for the device plugin

**Proven:**

- The gRPC service and the kubelet registration are unit-tested
  (`internal/deviceplugin/plugin_test.go`): `ListAndWatch` advertises
  `deviceCount` healthy devices when `/dev/kvm` is present and zero when it is
  absent (driven over an in-memory plugin server); `Allocate` returns the
  `/dev/kvm` and `/dev/net/tun` `DeviceSpec`s with matching host/container paths
  and `rw` permissions; and the Registrar registers against a FAKE kubelet
  Registration server over a unix socket with the right Version, Endpoint, and
  ResourceName. No real kubelet is involved.
- The kind e2e job deploys the DaemonSet and gates on it becoming Ready and not
  crashlooping. The kind-e2e GitHub Actions runner has `/dev/kvm` (the same
  runner class also runs the Firecracker KVM suite), so the plugin advertises a
  non-zero count. The e2e assertion is adaptive: it reads the node allocatable
  for `agentrun.dev/kvm` and branches on whether KVM is advertised.
- **Full advertise->schedule->inject path proven on the kind-e2e runner** (the
  stronger result): the probe pod requests `agentrun.dev/kvm: 1`, the scheduler
  places it (Running, not Pending), and `kubectl exec` into the running pod
  confirms `/dev/kvm` is present inside the container. The probe pod is
  explicitly non-privileged (`privileged: false`, `allowPrivilegeEscalation:
  false`, all capabilities dropped, read-only root filesystem): `/dev/kvm`
  access comes entirely from the device plugin's `Allocate` response, not from
  any host-path mount or privilege escalation. This proves the full
  PSA-restricted device-access path end to end on the CI runner. The e2e
  assertion also handles the no-KVM case honestly: on a runner without
  `/dev/kvm` the plugin advertises zero and the probe pod stays Pending (also
  asserted in the adaptive branch).

**Open:**

- Running the husk stub INSIDE a pod that requests this resource (the pod spec
  wiring) and migrating the forkd DaemonSet off its privileged `/dev/kvm`
  hostPath to request the resource are follow-ups (see section 6).

## 6. Controller migration: husk pod lifecycle (slice 1)

This is the first controller-migration slice. It is gated behind the
`--enable-husk-pods` flag on the controller and is OFF by default; with the flag
off the controller's behavior is unchanged (raw-forkd: the pool builds
node-local snapshots). Nothing here makes sandboxes pods on its own; it manages
the warm pool of husk pod OBJECTS so the later slices (activation, default flip)
have pre-scheduled pods to activate.

### What the flag does

When `--enable-husk-pods` is set, a `SandboxPool` no longer drives the
snapshot-on-nodes deficit. Instead it maintains a warm pool of husk pods sized
to `spec.replicas`:

- `buildHuskPod` (`internal/controller/huskpod.go`) emits a `GenerateName
  <pool>-husk-` Pod in the pool's namespace, owner-referenced to the pool, with
  the labels `agentrun.dev/pool=<pool>` and `agentrun.dev/husk=true`. The single
  container `husk-stub` runs the image from `--husk-stub-image` with args to
  Prepare a dormant Firecracker VMM (the `--firecracker`/`--kernel` paths and a
  `--control-socket` to listen on). The activation transport over that socket is
  slice 2; slice 1 only stands the dormant stub up.
- The container requests one `agentrun.dev/kvm` slot (request AND limit, the
  device-plugin contract) so the pod is scheduled only onto a `/dev/kvm` node,
  without `privileged: true`. It also carries cpu/memory REQUESTS sized from the
  template's `spec.resources` (or a documented default of 1 cpu / 512Mi when the
  template carries no sizing). Those requests are the scheduler-truth-partial
  result: the sandbox now shows up to the scheduler as ordinary pod requests
  (it counts against `kubectl describe node`, ResourceQuota, and LimitRange as a
  normal workload). The FULL scheduler-truth conformance (a quota actually
  bounding the pool, a LimitRange defaulting it, eviction/preemption) is the
  conformance slice; this slice proves the object exists with the requests set.
- The container `securityContext` is the new-execution-surface lockdown,
  documented per field in `huskpod.go`: `privileged: false`,
  `allowPrivilegeEscalation: false`, all capabilities dropped (`drop: [ALL]`,
  none added back; networking capabilities arrive with the networking slice),
  `seccompProfile: RuntimeDefault`. `runAsNonRoot: false` is the single
  documented exception: Firecracker opens the device-plugin-injected `/dev/kvm`,
  and the dormant-VMM bring-up is simplest as uid 0 WITHOUT `privileged`; moving
  to a non-root uid in the kvm group is a follow-up once the device permissions
  are pinned. This is NOT privileged and escalation is denied.
- `reconcileHuskPods` lists the pool's husk pods by label, keeps the
  non-terminating ones it owns, creates the deficit toward `replicas`, and
  deletes the surplus deterministically (by name) on scale-down. Deleting the
  pool garbage-collects its husk pods via the owner reference.

### The readiness nuance (envtest vs production)

In PRODUCTION a husk slot is "ready" only when its pod is Running AND Ready (the
dormant VMM is up and serving the control socket); the warm-pool size would gate
on that. envtest has no kubelet, so pods never run, never go Ready, and have no
phase. To keep the reconcile convergent in BOTH, `reconcileHuskPods` counts by
object EXISTENCE of non-terminating owned husk pods: it creates up to `replicas`
and deletes the extras. The production Running+Ready gate is layered on in the
activation slice; object existence is the correct convergence target for this
object-lifecycle slice.

### Proven vs open for slice 1

**PROVEN (envtest, `internal/controller/huskpod_test.go`):**

- the husk pod spec: the `agentrun.dev/kvm` request and limit of 1, the
  non-privileged securityContext (privileged false, escalation false, drop ALL,
  seccomp RuntimeDefault), the owner-ref to the pool, the two labels, the
  cpu/memory requests (template-sized and the default fallback), and the stub
  image;
- the warm-pool object lifecycle: a pool with `--enable-husk-pods` and
  `replicas: 3` creates 3 husk pod objects owned by the pool; a second reconcile
  is idempotent; scaling `replicas` to 1 deletes 2; with the flag off, no husk
  pods are created (the raw-forkd path runs unchanged).

**OPEN (later slices):**

- the husk pod actually RUNNING and the dormant VMM ACTIVATING end to end (the
  control-socket activation transport, slice 2; kind-e2e);
- the securityContext being genuinely PSA-`restricted` enforced (the conformance
  slice; the device-plugin exception is the one documented carve-out);
- flipping pod-native to the default with raw-forkd behind the flag (slice 3).

The default is still raw-forkd (flag off). The pod-native default is slice 3.

## 6b. Claim activation (slice 2)

Slice 1 (section 6) builds the warm pool of pre-scheduled DORMANT husk pod
objects. Slice 2 is the claim side: when a `SandboxClaim` binds to a pool with
`--enable-husk-pods`, the controller picks a dormant warm husk pod and ACTIVATES
it in place over the husk pod's mTLS control channel, instead of asking forkd to
fork a node-local VM. The activated VM runs inside the Kubernetes pod; the
claim's `Status.Endpoint` is set to the in-pod sandbox (the pod IP and the
in-pod sandbox port).

The activation path:

- the claim reconciler selects a `Running` + `Ready`, unclaimed husk pod for the
  pool (the pod the slice-1 lifecycle pre-scheduled), and dials its control
  server at `podIP:HuskControlPort`;
- it CLAIMS that pod BEFORE activating it, stamping the `agentrun.dev/claim`
  label under an OPTIMISTIC LOCK (`client.MergeFromWithOptimisticLock`): the
  patch carries the pod's `resourceVersion`, so two concurrent claims that both
  select the same dormant pod both attempt the patch but exactly ONE wins; the
  loser gets a `409 Conflict` and does NOT activate the pod, it requeues and
  picks a different dormant slot. Winning the label patch is the gate to
  Activate, so a dormant pod is claimed and activated by EXACTLY one claim: there
  is no double-assignment (two tenants on one VM);
- it activates over the NETWORK mTLS control channel (`internal/husk`
  `ServeTLS`), authorized to the controller identity: the husk server requires
  and verifies a client certificate and `AuthorizeControllerIdentity` accepts
  only a verified peer presenting the `pki.ControllerName` SAN. An
  UNAUTHENTICATED or wrong-CA activate is rejected by the handshake before any
  request is read, so a non-controller peer can never drive the secret-bearing
  activate path;
- it delivers the claim-time env and secrets through the SAME fork-correctness
  handshake the engine fork path uses (`NotifyForked` reseed + clock-step, then
  `Configure` for env/secrets), FAIL-CLOSED: a VM that did not reseed or whose
  secret delivery failed is reported as an error and never served. The controller
  refuses to send activation secrets over a nil (unauthenticated) TLS config
  (`ActivateHuskPod`), so secrets are never put on an unauthenticated wire;
- it delivers the per-sandbox bearer token the controller mints for the claim in
  the same `ActivateRequest` (a SECRET, riding the mTLS control channel, never
  logged). After a successful activate the husk stub SERVES THE IN-POD SANDBOX
  HTTP API (exec/files) on the sandbox port, reusing the same
  `internal/daemon` `SandboxAPI` forkd serves: it registers the activated VM (by
  its host vsock path) and the delivered token, then serves the bearer-gated
  exec/files Handler on `--sandbox-listen` (default `:9091`). Every request is
  gated on the per-sandbox token (constant-time compare; a request with no token
  or the wrong token is `401`), exactly as forkd does. This makes the endpoint
  the claim advertises (`Status.Endpoint = podIP:sandboxPort`) actually reachable
  and token-gated: the exec/files path is `SDK -> podIP:sandboxPort -> vsock ->
  guest agent`.

The snapshot the pod activates is mounted READ-ONLY from the node (the
node-local template the pool built). This is a PLACEMENT requirement for now: a
husk pod can only activate a template present on its node. Fully pod-native
snapshot delivery (the pod pulling the template into itself over the CAS wire
rather than relying on a node read-only mount) is a refinement, not done here.

### Proven vs open for slice 2

**PROVEN:**

- the mTLS network control TRANSPORT and AUTH: `internal/husk` `ServeTLS`
  requires and verifies the controller client cert and authorizes the
  `pki.ControllerName` identity; `ActivateHuskPod` dials it with the controller
  client config and refuses a nil config (unit-tested in
  `internal/husk` and `internal/controller`);
- the claim-activation WIRING: the claim reconciler selects a dormant Running +
  Ready husk pod and activates it over the control channel, sets the endpoint to
  the in-pod sandbox, pends when no dormant pod is available, and stays not-Ready
  when the activate fails (envtest, `internal/controller/husk_activation_test.go`:
  `TestHuskClaimActivatesDormantPod`, `TestHuskClaimNoDormantPodPends`,
  `TestHuskClaimActivateFailureNotReady`);
- the dormant-pod NO-DOUBLE-ASSIGN guarantee: the reconciler claims the pod under
  an optimistic lock BEFORE activating it, so two claims racing for one dormant
  pod resolve to exactly one activation; the loser gets a `409 Conflict` and
  requeues (envtest, `TestHuskClaimSingleDormantPodNoDoubleAssign`: one dormant
  pod + two claims yields exactly one Ready claim, the pod's `agentrun.dev/claim`
  label names only that winner, and the activator was called exactly once);
- the in-pod SANDBOX API is served and token-gated: after a successful activate
  the husk stub registers the activated VM and the delivered per-sandbox bearer
  token with `internal/daemon` `SandboxAPI` and serves the exec/files Handler on
  the sandbox port; a tokened HTTP exec reaches the guest over vsock and an
  untokened / wrong-token request is rejected (`internal/husk`
  `TestActivateServesTokenGatedSandboxAPI`), and the token value is never logged;
- a REAL network activate end to end on KVM: the KVM CI husk network-activation
  phase issues `internal/pki` certs (server leaf `pki.ServerName`, controller
  leaf `pki.ControllerName`), starts a dormant husk pod serving mTLS network
  control, activates it via `ActivateHuskPod` over the wire with a claim-time env
  var + secret + the per-sandbox bearer token, execs through the activated guest
  over vsock, asserts the secret is readable in the guest and absent from
  host-side logs, asserts a WRONG-CA controller cert is REJECTED by the mTLS gate,
  and ALSO execs over the in-pod SANDBOX HTTP API on the sandbox port (the
  `Status.Endpoint` wire) using the bearer token, asserting the tokened exec
  reaches the guest and an untokened / wrong-token request is rejected (401/403)
  with the token value absent from host-side logs. This proves the network
  transport + auth + the real activate + vsock exec + secret delivery + auth
  rejection + the advertised endpoint reachable and token-gated.

**OPEN (later slices):**

- raw-forkd is still the DEFAULT; the husk-pod claim path runs only under
  `--enable-husk-pods`. The full kind claim -> pod -> exec as the DEFAULT
  (flipping pod-native on, raw-forkd behind the flag) is slice 3;
- fully pod-native snapshot delivery (CAS pull into the pod) rather than the node
  read-only mount.

Sandboxes are pods ONLY on the husk path, which is opt-in (`--enable-husk-pods`)
until the default flips (slice 3). With the flag off the controller is unchanged
raw-forkd and sandboxes are forkd-owned VMs, not pods.

## 7. Proven vs remaining

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
- Fork-correctness on activate: the stub's `Activate` runs the same
  `NotifyForked` reseed + clock-step handshake as the engine fork path plus
  env/secret delivery via `Configure`, fail-closed (a VM that did not report
  `ReseededRNG` is not served). The KVM husk activate-correctness phase activates
  two VMs from one bench snapshot and gates on distinct RNG streams, each guest
  wall clock within 2s of the runner, and a delivered env var plus secret
  readable in each guest with the secret value absent from the host-side logs.
- The `/dev/kvm` device plugin: `cmd/kvm-device-plugin` (in
  `internal/deviceplugin`) advertises `agentrun.dev/kvm` only where `/dev/kvm`
  exists and injects `/dev/kvm` and `/dev/net/tun` on `Allocate`, so a husk pod
  gets KVM as a scheduled resource instead of `privileged: true` (section 5).
  The DevicePlugin and Registration gRPC are unit-tested against a fake kubelet.
  The kind e2e gates on the DaemonSet becoming Ready, then proves the full
  advertise->schedule->inject path: a non-privileged probe pod requesting
  `agentrun.dev/kvm: 1` is scheduled to Running on the KVM-capable kind-e2e
  runner, and `kubectl exec` into the running pod confirms `/dev/kvm` is present
  inside the container (injected by `Allocate`, not by any privilege or hostPath).
  This is the PSA-restricted device-access path proven end to end. The assertion
  is adaptive: on a no-KVM runner it asserts Pending instead. The forkd-DaemonSet
  migration off its privileged hostPath remains a follow-up.
- Claim activation over the mTLS network control channel (slice 2, section 6b):
  with `--enable-husk-pods` the claim picks a dormant warm husk pod, CLAIMS it
  under an optimistic lock (exactly one claim per pod, no double-assign), and
  activates it in place over the mTLS control channel (`internal/husk`
  `ServeTLS`, `internal/controller` `ActivateHuskPod`) authorized to the
  controller identity, delivering claim-time secrets AND the per-sandbox bearer
  token through the fork-correctness handshake (fail-closed) and setting
  `Status.Endpoint` to the in-pod sandbox. An unauthenticated/wrong-CA activate
  is rejected by the mTLS gate. After activation the husk stub SERVES the in-pod
  sandbox HTTP API (exec/files) on the sandbox port, reusing `internal/daemon`
  `SandboxAPI`, gated by the per-sandbox bearer token, so `Status.Endpoint` is
  reachable and token-gated. The claim-activation WIRING, the no-double-assign
  guarantee, and the token-gated sandbox API are proven in envtest +
  `internal/husk` (`husk_activation_test.go`,
  `TestActivateServesTokenGatedSandboxAPI`); a REAL network activate + vsock exec
  + secret delivery + auth rejection + token-gated HTTP exec over the sandbox
  endpoint is proven on KVM (the husk network-activation CI phase, certs via
  `internal/pki` so the SANs match). The snapshot is mounted read-only from the
  node (a placement requirement). The DEFAULT is still raw-forkd; the default
  flip is slice 3.

### Remaining (the rest of issue #18, follow-up PRs)

This PR does NOT migrate any controller and does NOT make sandboxes pods. The
full husk-pods epic still needs:

- running the stub INSIDE a real husk pod (the cgroup/netns placement, the
  dormant VMM actually starting); slice 1 (section 6) now builds and scales the
  husk pod OBJECTS behind `--enable-husk-pods` with the device-plugin resource
  request and the locked-down securityContext, proven in envtest, but kind-e2e
  has not yet run the pod;
- flipping pod-native to the DEFAULT with raw-forkd behind the flag (slice 3).
  Claim activation itself (the claim picks a dormant husk pod and activates it
  over the mTLS control channel, delivering the claim-time env and secrets) is
  DONE as slice 2 (section 6b): the transport + auth + the real network
  activate + exec + secret are proven on KVM and the claim-activation wiring is
  proven in envtest. What remains is making it the default and running the full
  kind claim -> pod -> exec path as the default;
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
