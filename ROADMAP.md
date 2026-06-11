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
- **W2: agents.x-k8s.io conformance facade.** `cmd/facade` implements the
  SIG `agent-sandbox` API (`agents.x-k8s.io/v1beta1`) on our engine; vendor
  their e2e suite into CI, document justified exceptions in
  `docs/facade-conformance.md`, never silently diverge. Depends on W1 (their
  API implies pod semantics). Includes the naming-collision ADR
  (our SandboxTemplate/SandboxClaim vs theirs; rename to
  ForkTemplate/ForkClaim/ForkPool is the preferred candidate; decide before
  1.0).
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
  forkable agent state independent of any sandbox (PVC:Pod analogy);
  hydrate/dehydrate via the SAME content-addressed transfer layer as
  snapshot distribution (§3: one pipeline, two artifact types); revision
  DAG lineage (`fromClaim:`/`fromWorkspaceRevision:`); outputs extraction +
  git rendezvous for fork-and-merge (git is the merge layer; we never do
  filesystem merge); single-writer-per-revision doctrine; memory-snapshot
  pairing is principal-bound per the secrets policy. Plus: revision change
  feed for external indexers (no embedded vector DB), per-node toolchain
  cache via Share policy, flagship reversible sleep-consolidation demo.
  Depends on §3; may land as alpha behind a flag with eager-fetch fallback.

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
  `agentrun_cow_memory_savings_bytes` and `agentrun_metered_disk_bytes`. Proven
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
  original key fails, image gone). PR2 key custody: the controller generates a
  per-template 256-bit key with `crypto/rand`, stores it in a `<template>-enc-key`
  Secret owner-referenced to the `SandboxTemplate` for GC crypto-shred, and
  delivers it to forkd over the mTLS gRPC `CreateTemplate` and `Fork` requests;
  forkd holds the key in process memory only via `RequestKeyProvider` (never on
  the node data disk), encryption enabled with no delivered key fails closed, the
  key is never logged; proven by envtest (Secret lifecycle, key-over-RPC,
  fail-closed, key-never-logged) and unit tests. See docs/encryption.md. Open
  follow-ups: forkd container-shred-on-template-GC wiring (the TEARDOWN BOUNDARY
  in enc_key_secret.go); KMS/HSM envelope encryption instead of a raw Secret;
  key rotation / re-encryption; per-workspace scope (#21); encrypting the CAS
  chunk store.

With #32 (mechanism done), #33 (mechanism done), and #31 (mechanism + custody
both done, CI-proven), all three format-freeze blockers are now fully addressed.

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
- ⬜ Lifetime memory accounting (`agentrun_memory_unique_bytes` over time,
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
  the same IP (allowlisting is by name, not IP). Open follow-ups: suffix/wildcard
  names (`*.anthropic.com`) and AAAA/IPv6 (A-only in v1).
- ⬜ Snapshot-fork networking under per-VM netns (lands with husk pods #18):
  live-fork (`ForkRunning`) of a networked sandbox fails closed today because it
  would restore the source's baked NIC and collide on tap/MAC/IP.
- ⬜ Per-fork conntrack flush and parent-connection-death semantics beyond
  fresh-identity; bandwidth/rate limiting; IPv6.

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
- ⬜ Bare-metal reference numbers on the Hetzner + Talos reference node; CI
  runs on pinned bare-metal hardware per release → `BENCHMARKS.md` results
  section (current CI numbers are shared-runner-class, not representative)
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
  metric (agentrun_claim_pending_total) is the autoscaler/back-pressure
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

- ✅ **MCP server interface**: `agentrun-mcp` exposes sandboxes as an MCP tool
  server (create/exec/read/write/fork/terminate as tools with versioned JSON
  schemas) over stdio JSON-RPC; every MCP-speaking agent becomes a user with
  zero SDK integration. A conformance test drives the server as a real MCP
  client in standard CI; see docs/mcp.md. Open: workspace tools (#21) and
  capability-budget advertisement (#24).
- ✅ **agentrun CLI + one-command local dev**: `agentrun run` and `agentrun
  sandbox create|ls|exec|fork|terminate` drive the `SandboxClaim` path over
  kubeconfig with token-scoped exec; `agentrun dev up|down` brings up a kind
  cluster running a mock control plane (controller `--mock
  --disable-pki-bootstrap`, forkd `--mock`, no KVM) from `deploy/dev/`. PROVEN in
  the kind CI smoke: `dev up` + `sandbox create` reaches Ready + `ls` + `terminate`
  on the mock engine; real in-VM exec is proven by the KVM CI of the API. See
  docs/cli.md. OPEN: workspace verbs (`agentrun ws ...`) deferred to Workspace
  (#21).
- ⬜ Streaming exec (stdout/stderr), stdin, **PTY mode**, file transfer,
  port forwarding, the daily-driver agent-harness needs
- ⬜ Code-interpreter-compatible API shim (drop-in for LangChain/LlamaIndex
  sandbox integrations)
- ✅ `kubectl sandbox` plugin: ls (SandboxClaims) and ps (SandboxForks) are
  done (pure table formatter unit-tested in CI; live listing over kubeconfig).
  OPEN: top/tree/exec/cp/logs/port-forward for operators.
- ✅ TypeScript SDK (`@agentrun/sdk`): `Sandbox` exec/fork/terminate/files over
  the forkd HTTP API with bearer auth; `SandboxServer` direct mode;
  `AgentRun` cluster mode over a mockable `K8sApi` interface;
  `@kubernetes/client-node` lazy-loaded so direct mode stays light; token
  never logged, redacted from errors; 31 conformance tests drive the client
  against a mock server reproducing the same wire shapes the Python SDK/MCP/CLI
  use; `typescript-sdk` CI job builds, tests, type-checks, and packs the
  package; real in-VM exec proven by the KVM CI of the underlying API; npm
  publish is a release follow-up. Parity table in sdk/typescript/README.md.
- ⬜ Agent Sandbox (k8s-sigs) CRD adapter: assess, decide, document either way
- ✅ One-command local story: `agentrun dev up` (kind + mock control plane from
  deploy/dev/), proven in the kind CI smoke. OPEN: document the KVM-passthrough
  path for real local exec.
- ⬜ Helm chart (README previously implied one exists; it does not yet)

## 8. Observability

- ✅ OpenTelemetry trace for the claim/fork path: controller.reconcileClaim →
  controller.forkOnNode → forkd.Fork → engine.fork, with W3C trace-id
  propagation over gRPC, enabled by --otlp-endpoint, no secrets in spans; PROVEN
  by in-memory span tests in CI. OPEN: guest-ready and first-exec spans need the
  guest-telemetry vsock bridge, and a single trace id stamped across
  pod/logs/Hubble/Workspace revisions needs husk pods (#18) and the Workspace
  (#21).
- ✅ Metrics: orphan-sweep counts, pending-claim requeues, and per-pool claim
  error rates are exported (agentrun_orphan_sweeps_total,
  agentrun_claim_pending_total, agentrun_claim_errors_total{pool,reason},
  agentrun_pool_ready_snapshots{pool}; increments asserted in CI). OPEN:
  snapshot-distribution lag, plus Grafana dashboards and PrometheusRule alerts
  with runbooks for 1.0.
- ✅ Toggleable structured audit log of every exec/file op (forkd --audit-log;
  records command/path and byte counts, never content or secrets;
  content-safety asserted in CI)
