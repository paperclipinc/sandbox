# Roadmap

Ordered by priority. The rule that orders it: **no unverified claims, and
security findings block features.** A fast sandbox that leaks across tenants,
or a README describing a system that does not exist, is worth nothing.

Status legend: ✅ done · 🔨 in progress · ⬜ not started

## Strategic workstreams (standing directives)

Four workstreams extend this roadmap (full prompts live with the project
owner; summaries here so sequencing is explicit). All inherit the core
operating principles, and **none ships to production tenants before the
fork-correctness suite (§1) and failure/GC semantics (§2) are green in CI.**

- **W1: Husk pods (pod-native execution).** Every sandbox VM moves inside a
  pod's cgroup/netns: pools pre-schedule minimal "husk" pods running a dormant
  VMM stub; claim = activate (mmap snapshot + KVM restore inside the pod),
  /dev/kvm via device plugin instead of `privileged: true`. Gains real
  scheduler visibility, ResourceQuota/LimitRange/NetworkPolicy/PSA-restricted
  conformance (each acceptance criterion gets a test), and improves the
  forkd blast radius in the threat model. Load-bearing claim to verify
  first: CoW page-cache sharing across pod memcg boundaries
  (Pss/Rss + cgroup-v2 accounting test). Deliverable: `docs/husk-pods.md` +
  device plugin + stub + migrated controllers + before/after benchmarks.
  Raw-forkd mode stays behind a flag.
  - ✅ Load-bearing claim verified: CoW page sharing survives cgroup v2 memcg
    boundaries, measured in CI. The `husk-probe` phase
    (`.github/workflows/kvm-test.yaml`) forks 4 sandboxes from one snapshot into
    4 separate cgroup v2 memory controllers and proves (gated on `CoWSurvives`)
    that the shared snapshot set is counted ~once across the memcgs while each
    fork's private dirty is charged to its own memcg. `docs/husk-pods.md` records
    the cgroup v2 charging model, the measured numbers, and the honest
    first-faulter nuance (fair per-tenant accounting uses the CoW-aware metering
    #33, not raw `memory.current`).
  - ✅ Dormant-VMM stub + in-place activation: the prepare/activate split is
    implemented and proven. `internal/husk` + `cmd/husk-stub` pre-start a DORMANT
    Firecracker VMM (prepare) and activate it in place via snapshot-load + resume
    + guest-ready on a line-delimited JSON control message (claim = activate),
    failing closed on a failed load. The KVM CI `husk-stub` phase measures the
    activation latency (load-start to first exec, shared-CI-class) and gates on
    activate OK plus a real exec through the activated VM. `docs/husk-pods.md`
    records the prepare/activate model and the honest target-vs-measured framing.
  - ✅ Fork-correctness handshake on activate: the stub's `Activate` runs the
    same `NotifyForked` reseed + clock-step handshake as the engine fork path
    (per-activation RNG reseed, clock step) plus env/secret delivery via
    `Configure`, fail-closed (a guest that did not report `ReseededRNG` is left
    unserved). The KVM husk activate-correctness phase proves it across two
    activations from one bench snapshot: distinct RNG streams, each guest wall
    clock within 2s of the runner, and a delivered env var plus secret readable
    in each guest with the secret value absent from the host-side logs. See
    `docs/fork-correctness.md` and `docs/husk-pods.md`.
  - ✅ The `/dev/kvm` device plugin (vs privileged): `cmd/kvm-device-plugin`
    (in `internal/deviceplugin`) advertises `mitos.run/kvm` only where
    `/dev/kvm` exists (scheduler truth: a no-KVM node advertises zero) and
    injects `/dev/kvm` and `/dev/net/tun` on `Allocate`, so a husk pod requests
    KVM as a scheduled resource instead of `privileged: true`. The DevicePlugin
    and Registration gRPC are unit-tested against a fake kubelet. The kind e2e
    proves the full advertise->schedule->inject path end to end on the
    KVM-capable kind-e2e runner: a non-privileged probe pod (no `privileged`,
    no hostPath, all capabilities dropped) requesting `mitos.run/kvm: 1` is
    scheduled to Running and `kubectl exec` confirms `/dev/kvm` is present
    inside the container, injected solely by `Allocate`. This is the full
    PSA-restricted device-access path proven in CI. The assertion is adaptive:
    on a no-KVM runner it asserts Pending (honest scheduler truth). Migrating
    the forkd DaemonSet off its privileged `/dev/kvm` hostPath is a follow-up.
    See `docs/husk-pods.md` section 5.
  - ✅ Husk pod lifecycle controller (migration slice 1, behind a flag): with
    `--enable-husk-pods` (default off, raw-forkd unchanged) a `SandboxPool`
    maintains a warm pool of pre-scheduled husk pod OBJECTS running the
    dormant-VMM stub, each requesting `mitos.run/kvm` (not `privileged`) with
    cpu/memory requests sized to the template (scheduler-truth-partial),
    locked-down securityContext (drop ALL, no escalation, seccomp
    RuntimeDefault, the documented `/dev/kvm` device exception), owner-ref GC,
    scaled to `replicas`. Proven in envtest: the pod spec, create, idempotent
    reconcile, scale up/down, owner-ref, and flag-off-unchanged. The pod
    actually running + activation are the next slices. `Dockerfile.husk-stub`
    builds the stub image (firecracker as in forkd; the kernel is a
    volume-provided runtime artifact). See `docs/husk-pods.md` section 6.
  - ✅ Claim activates a dormant husk pod in place (migration slice 2, behind the
    flag): with `--enable-husk-pods` a `SandboxClaim` picks a dormant warm husk
    pod and ACTIVATES it over the mTLS control channel (`internal/husk`
    `ServeTLS`, `internal/controller` `ActivateHuskPod`) authorized to the
    controller identity, delivers the claim-time secrets through the
    fork-correctness handshake (fail-closed), and sets `Status.Endpoint` to the
    in-pod sandbox; an unauthenticated/wrong-CA activate is rejected. The snapshot
    is mounted read-only from the node (a placement requirement). Proven: the
    mTLS transport + auth and the claim-activation wiring in envtest; a REAL
    network activate + exec + secret delivery + auth rejection on KVM (the husk
    network-activation CI phase, certs via `internal/pki` so the SANs match). At
    this slice the DEFAULT was still raw-forkd. See `docs/husk-pods.md` section 6b.
  - ✅ Pod-native is the DEFAULT + the full cluster path (migration slice 3): the
    controller runs `--enable-husk-pods` by DEFAULT (`--enable-raw-forkd` selects
    the fork-per-claim fallback; `--mock` forces it), so each `SandboxPool` builds
    the template snapshot on the KVM nodes AND maintains a warm pool of husk pods
    pinned to the snapshot-holding nodes, and a `SandboxClaim` activates one in
    place. The build-vs-run split: forkd stays the PRIVILEGED snapshot BUILDER;
    the husk pod is the UNPRIVILEGED RUNNER (device-plugin `/dev/kvm`, no
    `privileged`, read-only snapshot mount). The production base (`deploy/`) runs
    the controller in husk mode + forkd builder + device plugin with PKI
    bootstrap on; `Dockerfile.husk-stub` is built in CI. The `kind-e2e-husk` job
    proves the full CLUSTER object lifecycle on the KVM-capable runner: stack up,
    PKI Secrets minted, device plugin advertising, the pool creating husk pods the
    scheduler binds to the KVM node, and the claim driven to husk-pod activation;
    when the nested dormant VMM comes up in the kind pod it tightens to the full
    claim -> Ready -> exec gate. The in-VM activation + exec + fork-correctness +
    secret delivery + wrong-CA rejection path is gated on real KVM in
    `kvm-test.yaml`. Sandboxes ARE pods by default. See `docs/husk-pods.md`
    section 6c and `docs/threat-model.md` section 0 (the default execution surface
    is now the unprivileged husk pod).
  - ✅ Kubernetes conformance proven OBJECT-LEVEL on kind (`kind-e2e-husk`):
    scheduler truth (the husk pod carries cpu/memory requests, the scheduler binds
    it, and an over-allocatable probe pod stays Pending: no double-booking);
    ResourceQuota + LimitRange bounding husk-shaped pods with ZERO custom code (the
    over-quota pod is rejected by the Kubernetes quota admission); a NetworkPolicy
    selecting the husk pod (the husk-mode governing egress layer, section 6d); the
    EXACT PSA level, empirically verified (the husk pod is rejected by a restricted
    namespace on EXACTLY the documented read-only-snapshot-hostPath +
    runAsNonRoot-false (`/dev/kvm`) exceptions, the same securityContext minus those
    exceptions IS admitted into restricted, and a privileged pod IS rejected so PSA
    is enforcing); and `kubectl get pods` + `kubectl logs` showing the sandboxes.
    See `docs/husk-pods.md` section 6e.
  - ✅ Eviction, disruption, and drain proven OBJECT-LEVEL on kind (`kind-e2e-husk`,
    slice 4b): the pool creates a `policy/v1` PodDisruptionBudget (`<pool>-husk`,
    `minAvailable = max(1, Replicas-1)`) so a node drain disrupts at most one warm
    slot at a time; the warm pool self-heals a deleted husk pod back to Replicas
    via `Owns(pods)` + the deficit logic; a claim whose husk pod is lost re-pends
    (Phase Pending, endpoint cleared) via `checkHuskPodLost` + a `Watches(pod)`
    mapping and the pool recreates a replacement; an over-allocatable husk-shaped
    pod stays Pending+Unschedulable (the native cluster-autoscaler scale-up
    signal). A pool `drainPolicy` governs the active sandbox on a lost pod: Kill
    (default) re-pends, Checkpoint snapshots the live VM first where the VMM runs.
    See `docs/husk-pods.md` section 6f.
  - ✅ Threat model RE-DERIVED for the unprivileged-stub escape surface
    (`docs/threat-model.md` section 0, "Unprivileged-stub escape surface (issue
    #18 re-derivation)"): the surface is re-derived boundary by boundary (guest ->
    husk-stub container, control channel, read-only snapshot hostPath, `/dev/kvm`
    device, pod netns, in-pod sandbox API, enc key, eviction/drain) with a per-axis
    tally vs old forkd, each claim backed by a CI-proven mechanism (slices 2/4/4b,
    #9/#31/#32) or named as a residual. Honest verdict: the per-sandbox EXECUTION
    surface is strictly improved (unprivileged, capability-dropped,
    restricted-minus-two, pod-netns-governed) while the inherent
    `/dev/kvm`-and-kernel host-escape axis is EQUAL, not better, and
    forkd-the-builder stays a smaller privileged control-plane surface. Residuals
    ACCEPTED and TRACKED: the shared read-only snapshot mount (fully pod-native CAS
    delivery is the follow-up), the `/dev/kvm` inherent host-escape surface
    (unchanged from raw-forkd), the in-memory plaintext-DEK window during a
    container open (envelope wrapping is done; the plaintext DEK is zeroized
    immediately after use, and full HSM custody narrows but cannot eliminate this
    window), and the forkd-builder privilege (a builder redesign is out of
    scope). The IN-VM NetworkPolicy enforcement and the live-Checkpoint-on-drain
    survival are proven only on a KVM-capable kubelet, so they need the bare-metal
    reference node (#16).
  - ⬜ Still open (rest of #18): the nested dormant Firecracker VMM coming up
    reliably INSIDE a kind pod so the full claim -> pod -> exec tail GATES on kind
    too (today best-effort in `kind-e2e-husk`, gated in `kvm-test.yaml` with FC on
    the host); the IN-VM enforcement of a NetworkPolicy over the VM tap (needs a
    KVM-capable kubelet, a bare-metal reference node); the live-VM Checkpoint-on-
    drain snapshot actually surviving end to end and the full re-activate onto a
    Ready dormant pod after a re-pend (both need the VMM running in the husk pod,
    bare metal; the object-level PDB / self-heal / re-pend / unschedulable signal
    are proven above); the BARE-METAL P99 claim-to-first-exec <= 10ms warm-pool
    benchmark (slice 5; the shared-CI activation latency is not that target); and
    fully pod-native snapshot delivery (CAS pull into the pod) plus removing forkd
    entirely (it stays the builder).
- **W2: agents.x-k8s.io conformance facade.** `cmd/facade` implements the
  SIG `agent-sandbox` API (`agents.x-k8s.io/v1alpha1`, the real group/version;
  the issue text guessed v1beta1) on our engine; vendor their e2e suite into
  CI, document justified exceptions in `docs/facade-conformance.md`, never
  silently diverge. Depends on W1 (their API implies pod semantics).
  - ✅ FOUNDATION (slice 1, this slice): the facade controller maps an upstream
    `Sandbox` onto our husk-backed run path (`internal/facade` +
    `cmd/facade`, a separate opt-in binary): replicas 1 creates/owns our
    `SandboxClaim` via the `mitos.run/pool` bridge annotation (or a
    `--default-pool`), the claim Ready phase mirrors into the Sandbox Ready
    condition + replicas + serviceFQDN, replicas 0 / deletion terminates.
    Proven in envtest (the create -> Ready -> delete lifecycle). The faithful
    path was taken: we vendor their Go types after a verified go 1.24 -> 1.26
    bump (both lint runs clean). The naming-collision ADR
    (`docs/adr/0001-facade-and-naming.md`) records the facade approach and
    defers the our-vs-their rename (ForkTemplate/ForkClaim/ForkPool, the
    preferred candidate) to the API v2 migration to avoid two breaking renames.
    The conformance approach (vendor their examples + e2e, run in CI, no silent
    divergence, pause/resume as a memory snapshot documented) is in
    `docs/facade-conformance.md`.
  - ✅ CONFORMANCE HARNESS FOUNDATION (slice 2, this slice): the upstream
    artifacts are vendored verbatim under `third_party/agent-sandbox/` (the full
    `examples/` + `extensions/examples/` + `test/e2e/` reference + the CRDs,
    pinned to v0.4.6) and applied UNCHANGED against the facade. envtest
    (`internal/facade/examples_test.go`) applies every core Sandbox example and
    asserts the bridged claim; the `facade-conformance` kind job applies their
    hello-world Sandbox unchanged against a live apiserver and asserts the five
    object-level facts (admitted, bridged claim with owner + bridge annotation,
    status mirrored, deletion GCs the claim, replicas 0 terminates). The
    conformance MATRIX in `docs/facade-conformance.md` has a row per upstream
    `test/e2e/*_test.go` and per vendored example with a
    PROVEN-OBJECT-LEVEL-ON-KIND / NEEDS-BARE-METAL / JUSTIFIED-EXCEPTION status,
    no silent divergence. Their Go e2e suite is documented as bare-metal-gated
    (its predicates assert upstream-controller pods/services and a running
    sandbox), honestly NOT run here.
  - OPEN (later slices): the in-VM conformance (their PodReady/ChromeReady
    predicates = a running sandbox) on a KVM-capable kubelet / bare-metal node
    (the #18 nested-VMM boundary); the full upstream Go e2e suite run green end
    to end; the latest-two-minors CI matrix (only v0.4.6 is wired now); the
    state-PRESERVING pause (a memory snapshot across the pause via the Checkpoint
    primitive, so resume restores the exact in-VM state) and the in-VM resume
    head-to-head number (a bare-metal-reference-node target, #16); full
    podTemplate fidelity (image/resources/volumeClaimTemplates honored
    per-Sandbox); executing the deferred rename with API v2. The extension-kind
    mappings (SandboxTemplate/SandboxWarmPool/SandboxClaim) are DONE object-level
    in slice 4 below.
  - ✅ DONE (slice 3, this slice): pause/resume mapping + the `bench/facade/`
    harness. The facade maps the upstream Sandbox `spec.replicas` 0<->1
    pause/resume contract onto the husk warm pool: replicas 0 (pause) RELEASES
    the bridged claim so the husk pod returns dormant to the warm pool; replicas
    1 after a 0 (resume) RE-ACTIVATES a dormant warm husk pod via the same fast
    path as create (the ~42ms husk activation, #66), idempotent + stable under a
    1->0->1->0 toggle, with the conformant `Status.Replicas` + endpoint
    observable preserved (envtest `internal/facade` + the `facade-conformance`
    job's replicas 1->0->1 object-level resume assertion). `bench/facade/` is the
    reproducible pause/resume latency harness + methodology; the order-of-
    magnitude in-VM resume number stays a bare-metal-reference-node target (#16),
    since the husk VMM does not boot on kind (the #18 boundary), so only the
    object-level resume is measured there.
  - ✅ DONE (slice 4, this slice, FINAL #19 facade slice): the extension-kind
    mappings + behavioral fidelity. The facade now maps the core `Sandbox` AND
    all three upstream `extensions.agents.x-k8s.io` kinds onto our engine:
    their `SandboxTemplate` to our template (the first podTemplate container's
    image/command/env, bridge annotation `mitos.run/template`); their
    `SandboxWarmPool` to our `SandboxPool` at the requested `spec.replicas`
    (re-read every reconcile so an HPA-controlled scale is honored, bridge
    annotation `mitos.run/warmpool`); their `SandboxClaim` to our
    fork-from-snapshot claim honoring the warmpool policy
    (none/default/<name>), with status conditions (Bound/Ready), the bound
    sandbox identity, and deletion (their object deleted => ours GC'd) mirrored.
    Three new reconcilers in `internal/facade` (`SandboxTemplateReconciler`,
    `SandboxWarmPoolReconciler`, `SandboxClaimReconciler`), registered opt-in in
    `cmd/facade`. envtest covers each mapping + each warmpool policy + the
    replica follow + status mirror + deletion; the `facade-conformance` kind job
    applies their `extensions/examples` manifests UNCHANGED and asserts the
    object-level facts (g)-(j) (their template creates ours, their warm pool
    creates our pool at the replicas, their claim binds our claim from the
    template-matching pool, deletion GCs ours). The fidelity points + the
    warmpool-policy mapping + the justified exceptions (the `none` policy maps to
    a fork from the template snapshot since our engine has no pool-less path;
    volumeClaimTemplates/networkPolicy/securityContext/updateStrategy unmapped)
    are recorded in `docs/facade-conformance.md` with no silent divergence. The
    in-VM running-sandbox conformance + the measured bare-metal resume latency
    stay bare-metal targets (#16/#18); the our-API `Fork*` rename stays deferred
    to API v2 (the ADR).
- **W3: Paperclip/OpenClaw/Hermes integration.** `@paperclipinc/plugin-sandbox`
  implementing the upstream sandbox-provider contract against our claims
  (adapter installs baked at pool build; lease → claim TTL; callback-bridge
  egress as claim-time allowlist; claim-time secrets), paperclip-operator
  `backend: microvm`, shared operator core extracted as a library, OpenClaw
  sandbox driver. Hard-gated on §1+§2 (hostile inputs + real credentials in
  forked VMs). Deferred non-goal, tracked: whole-instance microVM hosting
  ("scale-to-snapshot"); waits on durable per-VM volumes, stable inbound
  endpoints across suspend/resume, balloon reclaim, multi-process guests,
  live-snapshot secrets.
- **W4: Workspace & state.** A `Workspace` CRD: durable, versioned,
  forkable agent state independent of any sandbox (PVC:Pod analogy, NOT a
  CSI volume; see docs/adr/0002-workspace-not-csi.md);
  hydrate/dehydrate via the SAME content-addressed transfer layer as
  snapshot distribution (§3: one pipeline, two artifact types); revision
  DAG lineage (`fromClaim:`/`fromWorkspaceRevision:`); outputs extraction +
  git rendezvous for fork-and-merge (git is the merge layer; we never do
  filesystem merge); single-writer-per-revision doctrine; memory-snapshot
  pairing is principal-bound per the secrets policy. Plus: revision change
  feed for external indexers (no embedded vector DB), per-node toolchain
  cache via Share policy, flagship reversible sleep-consolidation demo.
  Depends on §3; may land as alpha behind a flag with eager-fetch fallback.
  DONE (the declarative foundation): the `Workspace` and `WorkspaceRevision`
  CRDs in mitos.run/v1alpha1, and the Workspace controller that manages
  the revision DAG (head/revisions/resumable status with typed conditions +
  observedGeneration), the Pending -> Committed transition with
  immutability of a committed revision, `fromWorkspaceRevision` lineage
  validation, retention pruning that protects the head's ancestry, and
  owner-ref GC of revisions.
  DONE (slice 2, the hydrate/dehydrate data path): a bulk workspace tar
  transfer on the guest agent (vsock `TarDir`/`UntarDir`, a workspace
  allowlist + traversal sanitize), the host `internal/workspace`
  Hydrate/Dehydrate helpers over the node content-addressed store, and the
  sandbox<->workspace binding (`SandboxClaim.spec.workspaceRef`): the head
  hydrates into `/workspace` on start, a new committed `WorkspaceRevision`
  with `fromClaim` lineage dehydrates on terminate (the head advances),
  single-writer-per-workspace, and secrets excluded from revisions. PROVEN on
  KVM (the in-VM tar round trip byte-identical) and in envtest (the binding +
  revision lifecycle behind a transfer seam); see docs/workspaces.md.
  DONE (slice 3, outputs extraction + git rendezvous for fork-and-merge): a
  claim `spec.outputs` narrows the dehydrate capture to the listed `/workspace`
  subtrees (path filter; no path output captures the whole workspace) and a
  `{diff: true}` output records a content-hash diff of the new revision against
  the parent head on `status.diffSummary`; a `{git}` output renders a per-attempt
  branch from a `{{.name}}` template and pushes the workspace `spec.git.paths`
  content to a rendezvous remote via the git CLI, recorded on
  `status.gitPushes`. Git is the merge layer: the engine pushes branches, a
  human/CI merges them, the engine never merges working trees. PROVEN: the path
  filter + content-hash diff in unit + envtest, and the git push against a local
  bare repo + an envtest push wiring; a `{git}` output is a new egress noted in
  docs/threat-model.md. OPEN: the CloudEvents revision change feed and the
  memory-snapshot pairing producing a resumable head from a real checkpoint
  (slice 4); a real external rendezvous server + credentials (a referenced
  Secret); the SDK/CLI `terminate(outputs=...)` surface; the per-workspace
  encryption key (#31); the per-node toolchain cache via Share; the S3
  object-store backend (this slice uses the node CAS).

**Compliance & observability addendum (amends W1/W4):** permitted claim
language is limited to what a CI job proves (CNCF-conformant clusters, PSA
`restricted` with exactly one documented /dev/kvm exception, standard
quota/policy/eviction semantics, vendored conformance suite); never "fully
Kubernetes conformant". Residuals ship as ADRs in `docs/adr/` (kvm device
exception; the guest boundary; Workspace-not-CSI; forkd control channel
mirrored into Kubernetes Events with a bounded-delay CI test). Observability
acceptance: Hubble-visible per-sandbox flows, OpenCost attribution, a guest
telemetry bridge + `kubectl sandbox` plugin (`top`/`ps`/`logs`/`exec`), and
one trace ID from orchestrator request through exec to workspace revision.
Family maturity bar before 1.0: Grafana dashboards, PrometheusRule alerts
with runbooks, `docs/conditions.md` reason-code catalogue, shipped with the
Helm chart.

## Foundations (decide once, early)

Six clusters around the outside that are cheap to set now and expensive to
retrofit. Three are **design constraints that block format freeze** because
they touch the on-disk and on-wire formats: per-workspace encryption keys
(erasure = crypto-shredding, #31), the snapshot version-compatibility
contract (memory resumability has a stated window, #32), and CoW-aware
metering (the shared-pages billing primitive, #33). The rest is process:
licensing/IP posture (#34), security operations: we ship a kernel
(#35), hosted-service abuse controls with OSS hooks (#36), and
community/credibility operations (#37).

Format-freeze blockers:

- ✅ Snapshot version-compatibility contract (#32): the manifest records the
  snapshot format version (current = 1), the producing Firecracker version, the
  CPU model, the kernel, and the machine-config hash, all part of the
  content-addressed identity. The engine refuses to restore an incompatible
  snapshot on load (exact VMM match, exact CPU-model match, format version in
  the supported set; kernel informational), after the digest verify and before
  any Firecracker launch, with an actionable error and a
  `--allow-incompatible-snapshots` dev escape hatch. Proven by unit tests and a
  KVM CI phase that records a real manifest, confirms it is compatible with its
  producing node, and confirms a VMM mismatch and an unsupported format version
  are refused. See docs/snapshot-format.md. Open follow-ups: Firecracker CPU
  templates for cross-CPU-model restore, and live cross-Firecracker-version
  restore testing (needs two FC versions in CI).
- ✅ CoW-aware metering, the shared-pages billing primitive (#33): forks of one
  template restore the same snapshot with `MAP_PRIVATE`, so `internal/metering`
  counts each template's shared page set ONCE (the max-of-forks representative)
  instead of once per fork. `GetCapacity` reports the CoW-aware resident total
  (sum of per-fork unique + each template's shared-once), so the scheduler no
  longer double-counts the shared template region across forks. The engine
  `Metering()` report also accounts CoW disk for reflink (Snapshot) volumes
  (seed shared once, fork divergence unique). forkd serves it on the operational
  `GET /v1/metering` endpoint, and the memory gauges are CoW-aware plus
  `mitos_cow_memory_savings_bytes` and `mitos_metered_disk_bytes`. Proven
  by unit tests and a KVM CI phase that forks 4 sandboxes from one template and
  asserts the shared region is counted once (CoW-aware total below naive,
  positive savings, per-fork unique below the shared-once set), an honest
  density datapoint. See docs/metering.md and BENCHMARKS.md. Open follow-ups:
  precise reflink/btrfs block accounting (apparent sizes today), PSS-based
  attribution, and per-tenant rollups tied to Workspace (#21).
- ✅ Encryption at rest + crypto-shredding (#31), mechanism and key-custody
  both done and CI-proven; issue #31 is addressed. Each scope (a template now;
  a workspace when #21 lands) gets its own LUKS2 container (`internal/storecrypt`)
  backed by a sparse image file; the template snapshot and volumes are built
  inside the mounted (decrypted) container, so the bytes at rest are ciphertext.
  Because dm-crypt sits below the page cache, the mem mmap CoW restore reads
  decrypted pages and CoW page sharing across forks is preserved exactly as in
  the plaintext case. Erasure is crypto-shredding: `luksErase` wipes the LUKS
  keyslots and the image is removed, so the ciphertext is unrecoverable even with
  the key. Wired into the engine behind `--enable-encryption` (default off):
  `CreateTemplate` builds into a per-scope container, `Fork` opens it and restores
  from the decrypted mount, `DeleteTemplate` crypto-shreds. The key is fed to
  cryptsetup on stdin, never in argv or any log. PR1 mechanism proven by unit
  tests and a KVM CI phase on real cryptsetup: ciphertext at rest (marker absent
  on the raw image, present in the decrypted mount), decrypt/restore works (reopen
  + read intact), and crypto-shred makes the data unrecoverable (reopen with the
  original key fails, image gone). Key custody is ENVELOPE
  encryption (#31 follow-up, done for the local provider): the controller
  generates a per-template 256-bit DEK with `crypto/rand`, wraps it with a KMS
  KEK via `kms.Wrapper` (`internal/kms`), zeroizes the plaintext, and stores ONLY
  the wrapped DEK plus the non-secret KEK id in a `<template>-enc-key` Secret
  owner-referenced to the `SandboxTemplate` for GC crypto-shred; the plaintext DEK
  never reaches etcd or disk. It delivers the wrapped DEK + KEK id to forkd over
  the mTLS gRPC `CreateTemplate` and `Fork` requests; forkd unwraps via its KEK
  (`--kek-file` local AES-256-GCM provider), uses the plaintext DEK for cryptsetup,
  and zeroizes it immediately, holding only the wrapped DEK via
  `RequestKeyProvider` (never on the node data disk). Encryption enabled with no
  wrapped DEK or an unwrap failure fails closed; forkd refuses to start under
  `--enable-encryption` without `--kek-file`; the DEK and KEK are never logged.
  Proven by the `internal/kms` round-trip/tamper/KEK-mismatch unit tests and
  envtest (Secret stores the wrapped DEK + KEK id and never a raw key, RPC carries
  them, fail-closed). The local AES-256-GCM provider is the CI-testable default;
  cloud KMS (AWS/GCP/Vault) is interface-shaped follow-up where the KEK never
  leaves the HSM. See docs/encryption.md. Open follow-ups: forkd
  container-shred-on-template-GC wiring (the TEARDOWN BOUNDARY in
  enc_key_secret.go); cloud KMS/HSM providers; KEK rotation and DEK re-wrap; DEK
  rotation / re-encryption; per-workspace scope (#21); encrypting the CAS chunk
  store.

With #32 (mechanism done), #33 (mechanism done), and #31 (mechanism + custody
both done, CI-proven), all three format-freeze blockers are now fully addressed.

Process foundations status:

- Security operations (#35): cosign keyless signing + SBOM attestation of the
  published images, govulncheck + Trivy as CVE gates, a kernel-CVE tracking doc
  with an exact pinned vmlinux, a CODEOWNERS-backed security-review-required-
  paths policy, and a SECURITY.md gap-fill are DONE (engineering). OPEN
  (human-gated): the monitored disclosure mailbox, the real reviewer org
  membership behind CODEOWNERS, the published response-SLA commitment, an
  automated kernel-CVE feed, and admission-time signature enforcement defaults.

## 0. Make the claimed system real (in progress)

The README previously described an end-to-end system; parts of it were stubs.
This phase closes the gap or keeps the README honest about it.
Plan: `docs/superpowers/plans/2026-06-10-control-plane-wiring.md`.

- ✅ Honest README: every unimplemented feature labeled, every number marked
  measured-or-target
- ✅ controller ↔ forkd gRPC (claim/fork actually produce sandboxes;
  was `not implemented` stubs)
- ✅ SandboxPool snapshot accounting and creation (was a no-op); works
  against the mock engine; the real engine needs an image→rootfs build
  pipeline (template.Spec.Image is currently passed as a rootfs file path)
- ✅ Image→rootfs build pipeline so pool templates can be built from OCI
  images on real nodes: internal/ociroot pulls and flattens an OCI image into
  an ext4 rootfs with the guest agent as /init; Engine.CreateTemplate builds
  from an OCI ref (vs a file path), boots, runs template.Spec.Init IN the VM
  (a failed init aborts the build), waits for agent readiness, then snapshots;
  proven end to end in KVM CI from busybox:stable (see docs/templates.md).
  Open follow-ups: go:embed the agent into forkd so no external --agent-bin is
  needed; OCI layer caching tied to the CAS store for faster pool builds;
  registry credentials / private images and a pull-through mirror; non-ext4
  backends (erofs, virtio-fs)
- ✅ forkd node discovery + capacity heartbeats (was a TODO)
- ✅ Truthful claim endpoints (point at forkd's sandbox API, not a
  fabricated address)
- ✅ Python SDK k8s mode speaks the actual forkd API
- ✅ Volume fork policies attach volumes to VMs for `Fresh` (new empty ext4)
  and `Snapshot` (reflink CoW): node-side `internal/volume` backend, placeholder
  drives baked at snapshot + PATCH-rebind per fork, guest mounts at the mount
  path, KVM-CI-proven (Fresh round-trip + Snapshot CoW independence on a btrfs
  loopback). `Share` (read-only shared attach) and `Clone` (full copy) are
  partial; external volume sources (S3/GCS/PVC/Git) and CSI clone remain open.
- ✅ Secrets delivered into the guest over vsock (strict on real engines;
  wire encryption pending #4)

## 1. Fork-engine correctness + threat model

Spec: `docs/fork-correctness.md`, `docs/threat-model.md`. Blocks everything
below it; a `fork-correctness` CI job gates PRs touching `internal/fork/`,
`internal/firecracker/`, `guest/`.

- 🔨 RNG reseed on every fork (guest-agent NotifyForked hook delivers host
  entropy over vsock + userspace signal; virtio-rng device NOT wired);
  go tests assert forkd sends entropy and fails closed; kvm-test asserts two
  forks of one snapshot produce distinct urandom (UUID/TLS-randoms follow-up)
- 🔨 Clock resync after restore (NotifyForked steps CLOCK_REALTIME from the
  host wall clock, 500ms tolerance); kvm-test asserts each fork wall clock
  within 2s of the runner (post-snapshot TLS cert validation follow-up)
- ✅ Live-fork secret policy: a fork of a secret-holding claim is rejected
  without `allowSecretInheritance: true`, with a `SecretInheritanceDenied`
  condition; envtest-proven in internal/controller/fork_secrets_test.go.
- 🔨 Firecracker under jailer (per-VM UID, chroot, cgroup); forkd drops
  `privileged: true` for an explicit capability list (implemented; kvm-test
  jailer-boot phase restores a snapshot under the jailer to prove the
  chroot/uid mechanics, but the dropped capability set is still unproven since
  the runner is root; direct-exec dev path behind an empty `--jailer` and
  sandbox-server remain unjailed; tracked in threat model residuals)
- ✅ mTLS + authz on controller↔forkd gRPC; auth on the :9091 sandbox API
  (rotation and token expiry pending; tracked in threat model residuals)
- ✅ Snapshot content addressing (#9): manifest digest in pool status,
  verify-on-load refuses a tampered snapshot (dev escape via
  `--allow-unverified-snapshots`). Proven by unit tests and the KVM CI
  tamper-detection phase on a real snapshot; residual (verify-once, not
  per-fork) tracked in the threat model. See docs/snapshot-distribution.md.
- ⬜ Lifetime memory accounting (`mitos_memory_unique_bytes` over time,
  not just T=0)
- ⬜ External security review scheduled before any 1.0 claim

## 2. Failure and GC semantics

Every component gets a defined answer to: crash, node death, slow etcd,
out of capacity. Chaos suite in CI.

- ⬜ forkd crash policy: running VMs reaped deterministically on restart
  (forkd is the VM supervisor; orphan FC processes are killed and claims
  failed with a typed condition). Open: needs forkd-local state so a
  restarted forkd can recognize and reap its own pre-crash VMs; tracked in
  epic #12.
- 🔨 Node loss: claims reach `NodeLost` within the GC interval (done); pools
  rebuild replicas elsewhere is still open (tracked in #12).
- ✅ Controller restart: the GC pass rebuilds the desired set from CRD state
  and sweeps any forkd VM not accounted for; zero orphans.
- 🔨 Orphan sweeps: VM without a backing object is swept past OrphanGrace,
  with a live-claim-by-name safety net (done). Volume without object is
  still open.
- ✅ Claim TTLs: `maxLifetime` and `idleTimeout` reap to a terminal
  `Terminated` phase with status conditions; `idleTimeout` reads activity
  via the forkd `ListSandboxes` primitive.
- 🔨 etcd hygiene: TTL of finished objects, including early-failed claims,
  is done. Rate-limiting and batching of status updates is still open.
- ⬜ Saturation behavior: queue with deadline then a typed fail-fast
  condition. Open (tracked in #12).

See docs/failure-gc.md for each guarantee, its proving test, and bounded
time, plus the explicit open items.

## 3. Snapshot distribution

forkd loading snapshots "from local storage" reinvents image pull with
multi-GB artifacts. No competitor solves this well in open source; treat as
a differentiator.

- ✅ Content-addressed snapshot store: fixed-size sha256 chunks, deduplicated
  across snapshots, manifest digest as the snapshot identity (VMMVersion in
  the manifest aligns it with the version-compat contract #32). Unit tests
  prove dedup and byte-identical reconstruction; the KVM CI integrity phase
  proves byte-identical reconstruction and tamper detection on a real
  multi-hundred-MB Firecracker snapshot. See docs/snapshot-distribution.md.
- ✅ Chunked incremental transfer: Transport interface + HTTP transport pull
  only the MissingChunks (each verified on arrival), so pool rebuilds ship
  deltas, not whole images. Unit tests prove the incremental delta path.
- ✅ Node cache eviction policy for bounded NVMe: mtime-LRU EvictToFit with
  pinned manifests protected, crash-safe via on-disk access times.
- ✅ Build once, distribute by pull (#14): forkd serves its CAS under /cas over
  TLS gated by a peer token plus forkd-to-forkd mTLS; Engine.PullTemplate pulls,
  materializes, and verifies (digest + snapcompat) a template from a holder,
  fail-closed on a bad/tampered source; the pool reconciler builds a
  non-encrypted template ONCE and distributes by pull instead of rebuilding on
  every node. CI-proven on TWO processes / two data dirs: a peer pulls + verifies
  + forks + execs from the pulled snapshot, a wrong token is rejected (403), and
  a wrong digest fails the pull fail-closed (cmd/pull-smoke). See
  docs/snapshot-distribution.md.
- ⬜ Measured cross-node propagation at 10/50/100 nodes (pool-update to
  all-nodes-ready over a real network). OPEN and unmeasured: needs a multi-node
  testbed; propagation-time numbers are not stated until then (#14).
- ⬜ A shared registry/object-store mirror as a pull source (instead of
  peer-to-peer pull from a holder node). OPEN: not built.
- ⬜ Distributing ENCRYPTED templates: encrypted templates are built per node
  and are NOT distributed; needs the CAS chunk store itself encrypted (#31).
- ⬜ Per-node SAN pinning and per-pull minted tokens: today the holder serves
  the one shared serving identity (pki.ServerName) and a single shared bearer
  token. OPEN: a distinct verified identity per holder and short-lived per-pull
  tokens are a follow-up.
- ⬜ `prefetch: full | lazy` pool setting (serve forks from partially
  fetched snapshots). OPEN: lazy partial-fetch serving is not yet built.

## 3b. Guest networking and egress

Spec: `docs/networking.md`, threat model §4. Opt-in per node
(`forkd --enable-networking`); makes the `NetworkPolicy` CRD real for literal
IP:port destinations. Default-deny, host-side enforced, guest cannot influence
or spoof.

- ✅ Host-side IP:port egress allowlist: per-sandbox tap + /30 + MAC identity,
  Firecracker NIC bound to the tap via `network_overrides` per fork, host-side
  nftables default-deny with a shared-table dispatch model (per-tap jump into a
  per-sandbox chain ending in drop, `ip saddr` anti-spoof). Cross-tap isolation
  proven: one sandbox's drop never kills another's allowed traffic. Proven in
  KVM CI: a single VM reaches the allowed destination and is blocked from the
  denied one, and a two-sandbox `nft` install validates against real nft. The
  controller plumbs `template.Spec.networkPolicy` (egress + allow) through the
  Fork RPC. See docs/networking.md.
- ✅ Controlled DNS resolver for name-based allowlists (#47, behind
  `forkd --enable-dns-egress`): a per-node resolver (`internal/dnsproxy`) that
  resolves ONLY allowlisted names and pins each resolved `(ip . port)` into that
  sandbox's nftables timeout set with `max(recordTTL, 30s)` validity; the guest's
  only resolver is the node resolver IP (`169.254.1.1`). Exact-match FQDNs,
  CI-proven: a resolved allowlisted name:port is reachable while an unlisted name
  (refused), the right name on a wrong port, and an un-resolved direct IP are
  blocked, against a stub upstream mapping the allowlisted and a denied name to
  the same IP (allowlisting is by name, not IP).
- ✅ Anchored suffix wildcard names and AAAA/IPv6 in the name allowlist. A
  wildcard `*.D` matches a subdomain of `D` (a non-empty label before `.D`) and
  ONLY a subdomain: never the apex, never a look-alike (`evilexample.com`), never
  `D` as a non-suffix label (`example.com.evil.com`); a literal anchored suffix
  check (no regex), exhaustively bypass-tested. A wildcard is validated at the
  boundary (single leading `*.` plus a valid domain; `*`, `*.`, `*foo.com`,
  `a.*.com`, `**.com` rejected). AAAA is resolved and pinned into a separate v6
  nftables timeout set with the same TTL model as A, and each per-sandbox chain
  carries a v6 default-deny so an unpinned v6 destination is dropped. Honest v6
  scope: the guest has only a v4 `/30` source identity today, so the v6 accept is
  not `ip saddr` anti-spoof-pinned (moot for a single-stack guest, and the v6
  default-deny is the boundary); the v4 path is the KVM-CI-proven one and the v6
  dataplane is covered by the chain-render and pinner unit tests.
- ⬜ Snapshot-fork networking under per-VM netns (lands with husk pods #18):
  live-fork (`ForkRunning`) of a networked sandbox fails closed today because it
  would restore the source's baked NIC and collide on tap/MAC/IP.
- ⬜ Per-fork conntrack flush and parent-connection-death semantics beyond
  fresh-identity; bandwidth/rate limiting; a full dual-stack guest source
  identity (a v6 `/126` + v6 anti-spoof pinning) so guests can source v6 (the
  AAAA pin path and v6 default-deny are already in place).

## 4. Benchmark program + honest comparison

- ✅ `bench/` harness (`cmd/bench` + `internal/benchstat`): drives the real
  fork engine in-process and measures fork->first-exec and warm exec
  round-trip distributions (count/min/p50/p90/p99/max/mean). Reproducible in
  CI: the KVM workflow runs it every run and publishes the tables to the job
  summary plus a JSON artifact. Methodology in `BENCHMARKS.md`,
  reproduction in `bench/README.md`.
- ⬜ claim->first-exec end to end through the controller on a real cluster
  (the harness measures the engine data path, not the controller + pool path)
- ⬜ sustained claims/sec, density curves, pool-rebuild propagation
- ✅ Raw-forkd vs pod-native claim-to-first-exec comparison: the two
  shared-CI datapoints (raw-forkd `fork_to_first_exec` from the bench phase,
  pod-native husk activation from the husk-stub phase) are synthesized in
  `BENCHMARKS.md` with the design-win framing (VMM spawn amortized to
  warm-pool-fill time, claim hot path is activation only, pod-native is
  competitive with raw-forkd). See `BENCHMARKS.md` "Raw-forkd vs pod-native"
  section. The measured bare-metal <=10ms stays a TARGET (#16, needs the
  reference node).
- ⬜ Bare-metal reference numbers on the Hetzner + Talos reference node;
  includes the <=10ms warm-pool claim-to-first-exec TARGET (#18/#16, not a
  shared-CI claim); CI runs on pinned bare-metal hardware per release →
  `BENCHMARKS.md` results section (current CI numbers are shared-runner-class,
  not representative)
- ⬜ Comparison table regenerated from in-repo scripts against E2B
  self-hosted, Daytona OSS, Agent Sandbox + Kata warm pools on the same
  hardware; reproducible by anyone
- ✅ Exec hot-path latency (vsock round-trip + guest spawn) is measured by the
  `exec-rt` mode with the same percentile rigor as fork latency; end-to-end
  gRPC->vsock->spawn through the API remains open

## 5. Talos + Hetzner reference platform

- ✅ `deploy/talos/worker-kvm.yaml` and `deploy/talos/controlplane.yaml`:
  machine-config patches for KVM-capable workers (/dev/kvm, modules kvm /
  kvm_intel / kvm_amd / vhost_vsock / tun, node label, data partition);
  validated by `talosctl validate --mode metal` in the `talos-validate` CI job.
- ✅ `docs/platforms/talos-hetzner.md`: end-to-end bare-metal provisioning
  runbook (Hetzner AX BOM as a reference example, NOT measured; Cloud vs
  dedicated explained; Talos install + machine-config flow; KVM readiness
  checks; operator deploy + PKI bootstrap; smoke test; capacity planning
  pointers). CI-VERIFIED vs HARDWARE-REQUIRED split clearly marked in the
  runbook.
- ⬜ Evaluate dm-thin / xfs-reflink as alternatives to the btrfs dependency
- ⬜ Hetzner AX reference BOM: MEASURED density and cost/sandbox-hour on the
  pinned reference node. Needs the hardware; do NOT fabricate numbers.
  Will update `BENCHMARKS.md` and `docs/platforms/talos-hetzner.md` once
  measured (ROADMAP section 4 bare-metal bench run).
- ⬜ Measure and publish the nested-virt penalty on EKS/GKE/AKS instead of
  hiding it

## 6. Density and scheduling

The admission model, packing policy, overcommit safety argument, and
pending/backpressure behavior are documented in docs/scheduling.md.

- ✅ Capacity-aware admission: each node's budget is host MemTotal minus a
  reserve, times an overcommit factor, checked against the CoW-aware
  MemoryUsed (#33) with a per-template cold-vs-warm marginal-cost projection.
  A fork is admitted to a node only when its projected cost fits. Unit-tested
  in internal/controller/scheduler_test.go.
- ✅ CoW bin-packing: SelectNode packs forks of one template onto a warm
  snapshot-holder to maximize CoW sharing (reversing the old load-spreading)
  and spills cold starts to the most-available node.
- ✅ Documented overcommit policy + saturation behavior: claims with no
  fitting node pend with backpressure (NoCapacity condition, bounded backoff)
  and fail cleanly with an actionable capacity-exhaustion error after a
  bounded --max-pending-duration instead of OOMing a node. The pending-claims
  metric (mitos_claim_pending_total) is the autoscaler/back-pressure
  signal; envtest-proven in internal/controller.
- ⬜ NUMA pinning + hugepage-backed guest memory; KSM same-page-merging
  tuning; per-node max density config (needs hardware)
- ⬜ Multi-resource bin-packing (disk, CPU, and the cold-start
  snapshot-distribution cost, which ties into #14); preemption/eviction under
  pressure; predictive prewarming
- ⬜ MEASURED bare-metal density curve on the pinned reference node (section 4;
  a target until run on hardware, never fabricated); cluster-autoscaler /
  Karpenter integration driven by the pending-claims signal

## 7. Ergonomics, UX, and compat

The DX gap against E2B/Daytona is the adoption bottleneck once the core is
verified. In rough order of leverage:

- ✅ **MCP server interface**: `mitos-mcp` exposes sandboxes as an MCP tool
  server (create/exec/read/write/fork/terminate as tools with versioned JSON
  schemas) over stdio JSON-RPC; every MCP-speaking agent becomes a user with
  zero SDK integration. A conformance test drives the server as a real MCP
  client in standard CI; see docs/mcp.md. Open: workspace tools (#21) and
  capability-budget advertisement (#24).
- ✅ **mitos CLI + one-command local dev**: `mitos run` and `mitos
  sandbox create|ls|exec|fork|terminate` drive the `SandboxClaim` path over
  kubeconfig with token-scoped exec; `mitos dev up|down` brings up a kind
  cluster running a mock control plane (controller `--mock
  --disable-pki-bootstrap`, forkd `--mock`, no KVM) from `deploy/dev/`. PROVEN in
  the kind CI smoke: `dev up` + `sandbox create` reaches Ready + `ls` + `terminate`
  on the mock engine; real in-VM exec is proven by the KVM CI of the API. See
  docs/cli.md. OPEN: workspace verbs (`mitos ws ...`) deferred to Workspace
  (#21).
- ⬜ Streaming exec (stdout/stderr), stdin, **PTY mode**, file transfer,
  port forwarding, the daily-driver agent-harness needs
- ⬜ Code-interpreter-compatible API shim (drop-in for LangChain/LlamaIndex
  sandbox integrations)
- ✅ `kubectl sandbox` plugin: ls (SandboxClaims), ps (SandboxForks), tree (the
  fork/lineage DAG), top (per-sandbox CoW-aware metering from forkd
  `/v1/metering`, honestly labeled, a dash on a missing datum), logs (the husk
  stub pod console plus the guest-console #18 note), and exec (token-scoped
  operator exec over the sandbox API using the claim's `<claim>-sandbox-token`
  Secret, the same gate the SDK uses) are done (pure formatters + the metering
  match and exec/token resolution unit-tested in CI; live over kubeconfig; the
  kind-e2e smoke proves ls/ps/tree object-level, with exec/top/logs of a running
  sandbox as the KVM/bare-metal tail). OPEN: cp (file copy) and port-forward for
  operators, and the PTY-mode streaming exec on the SDK side.
- ✅ TypeScript SDK (`@mitos/sdk`): `Sandbox` exec/fork/terminate/files over
  the forkd HTTP API with bearer auth; `SandboxServer` direct mode;
  `AgentRun` cluster mode over a mockable `K8sApi` interface;
  `@kubernetes/client-node` lazy-loaded so direct mode stays light; token
  never logged, redacted from errors; 31 conformance tests drive the client
  against a mock server reproducing the same wire shapes the Python SDK/MCP/CLI
  use; `typescript-sdk` CI job builds, tests, type-checks, and packs the
  package; real in-VM exec proven by the KVM CI of the underlying API; npm
  publish is a release follow-up. Parity table in sdk/typescript/README.md.
- ⬜ Agent Sandbox (k8s-sigs) CRD adapter: assess, decide, document either way
- ✅ One-command local story: `mitos dev up` (kind + mock control plane from
  deploy/dev/), proven in the kind CI smoke. OPEN: document the KVM-passthrough
  path for real local exec.
- ⬜ Helm chart (README previously implied one exists; it does not yet)

## 8. Observability

- ✅ OpenTelemetry trace for the claim/fork path: controller.reconcileClaim →
  controller.forkOnNode → forkd.Fork → engine.fork, with W3C trace-id
  propagation over gRPC, enabled by --otlp-endpoint, no secrets in spans; PROVEN
  by in-memory span tests in CI.
- ✅ Trace-to-revision link: the claim reconcile trace id is stamped on the
  WorkspaceRevision it produces (the mitos.run/trace-id annotation, valid id
  only, omitted when tracing is off), a workspace.dehydrate child span names the
  revision and the contentManifest digest, and the trace id rides the
  revision.created feed event (traceId field), so a revision resolves to the
  orchestrator request that created it and back. No secrets in spans, the
  annotation, or the feed field; PROVEN by in-memory span and feed unit tests in
  CI. OPEN: guest-ready and first-exec spans need the guest-telemetry vsock
  bridge (the in-VM tail), a single trace id across Hubble network flows needs
  the Cilium/Hubble integration, and Grafana dashboards plus PrometheusRule
  alerts that pivot on the trace id are the 1.0 maturity bar.
- ✅ Metrics: orphan-sweep counts, pending-claim requeues, and per-pool claim
  error rates are exported (mitos_orphan_sweeps_total,
  mitos_claim_pending_total, mitos_claim_errors_total{pool,reason},
  mitos_pool_ready_snapshots{pool}; increments asserted in CI).
- ✅ Grafana dashboard + PrometheusRule alerts + runbooks + the
  conditions/reason-code catalogue (the 1.0 maturity bar): the opt-in
  deploy/monitoring kustomize layer ships a Grafana dashboard and five alerts
  (ClaimErrorRateHigh, ClaimsPendingSustained, WarmPoolStarved, OrphanSweepSpike,
  ForkLatencyHigh) over the exported metrics, each linking a docs/runbooks file;
  the rules are promtool-validated in CI and reference only real metric names;
  docs/conditions.md covers every condition reason the controllers emit. Alert
  thresholds are environment-tunable and the <=10ms p99 fork stays a bare-metal
  target, not an asserted SLO. OPEN: snapshot-distribution lag (needs the
  multi-node distribution path, #3) and its alert; Helm-chart packaging of the
  dashboard and alerts (the deploy/monitoring kustomize layer is the current
  slice).
- ✅ Toggleable structured audit log of every exec/file op (forkd --audit-log;
  records command/path and byte counts, never content or secrets;
  content-safety asserted in CI)
