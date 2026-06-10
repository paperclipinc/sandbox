# Roadmap

Ordered by priority. The rule that orders it: **no unverified claims, and
security findings block features.** A fast sandbox that leaks across tenants,
or a README describing a system that does not exist, is worth nothing.

Status legend: ✅ done · 🔨 in progress · ⬜ not started

## 0. Make the claimed system real (in progress)

The README previously described an end-to-end system; parts of it were stubs.
This phase closes the gap or keeps the README honest about it.
Plan: `docs/superpowers/plans/2026-06-10-control-plane-wiring.md`.

- ✅ Honest README: every unimplemented feature labeled, every number marked
  measured-or-target
- ✅ controller ↔ forkd gRPC (claim/fork actually produce sandboxes;
  was `not implemented` stubs)
- ✅ SandboxPool snapshot accounting and creation (was a no-op) — works
  against the mock engine; the real engine needs an image→rootfs build
  pipeline (template.Spec.Image is currently passed as a rootfs file path)
- ⬜ Image→rootfs build pipeline so pool templates can be built from OCI
  images on real nodes (guest/rootfs/build.sh is offline tooling today)
- ✅ forkd node discovery + capacity heartbeats (was a TODO)
- ✅ Truthful claim endpoints (point at forkd's sandbox API, not a
  fabricated address)
- ✅ Python SDK k8s mode speaks the actual forkd API
- ⬜ Volume fork policies actually attach volumes to VMs (handlers are
  currently no-ops; `Snapshot` needs a real btrfs/reflink backend)
- ⬜ Secrets delivered into the guest over vsock (resolved by controller
  today, then dropped)

## 1. Fork-engine correctness + threat model

Spec: `docs/fork-correctness.md`, `docs/threat-model.md`. Blocks everything
below it; a `fork-correctness` CI job gates PRs touching `internal/fork/`,
`internal/firecracker/`, `guest/`.

- ⬜ RNG reseed on every fork (virtio-rng + guest-agent NotifyForked hook +
  userspace signal); test: distinct urandom/UUID/TLS-randoms across N forks
- ⬜ Clock resync after restore; test: wall-clock within 500ms, post-snapshot
  TLS cert validates
- ⬜ Live-fork secret policy: reject without `allowSecretInheritance: true`
- ⬜ Firecracker under jailer (per-VM UID, chroot, cgroup) — priority zero in
  the threat model
- ⬜ mTLS + authz on controller↔forkd gRPC; auth on the :9091 sandbox API
- ⬜ Snapshot content addressing (digest in CRD status, verify-on-load)
- ⬜ Lifetime memory accounting (`agentrun_memory_unique_bytes` over time,
  not just T=0)
- ⬜ External security review scheduled before any 1.0 claim

## 2. Failure and GC semantics

Every component gets a defined answer to: crash, node death, slow etcd,
out of capacity. Chaos suite in CI.

- ⬜ forkd crash policy: running VMs reaped deterministically on restart
  (forkd is the VM supervisor; orphan FC processes are killed and claims
  failed with a typed condition)
- ⬜ Node loss → claims `NodeLost` within bounded time; pools rebuild
  elsewhere
- ⬜ Controller restart: reconcile CRD state against forkd-reported actual
  VMs; zero orphans
- ⬜ Orphan sweeps (VM without claim, volume without object)
- ⬜ Claim TTLs: `maxLifetime`, `idleTimeout` with status conditions
- ⬜ etcd hygiene: TTL finished objects, rate-limit status updates
- ⬜ Saturation behavior: queue with deadline → typed fail-fast condition

## 3. Snapshot distribution

forkd loading snapshots "from local storage" reinvents image pull with
multi-GB artifacts. No competitor solves this well in open source — treat as
a differentiator.

- ⬜ Content-addressed snapshot store (OCI artifact), chunked incremental
  transfer; pool rebuilds ship deltas
- ⬜ P2P distribution between nodes (Spegel-style); publish
  pool-update→all-nodes-ready at 10/50/100 nodes
- ⬜ `prefetch: full | lazy` pool setting (serve forks from partially
  fetched snapshots)
- ⬜ Node cache eviction policy for bounded NVMe

## 4. Benchmark program + honest comparison

- ⬜ `bench/` harness: claim→first-exec P50/P99, fork→first-exec P50/P99
  (end-to-end, not just the KVM restore syscall), exec round-trip, sustained
  claims/sec, density curves, pool-rebuild propagation
- ⬜ CI runs on pinned bare-metal hardware per release → `BENCHMARKS.md`
  with methodology
- ⬜ Comparison table regenerated from in-repo scripts against E2B
  self-hosted, Daytona OSS, Agent Sandbox + Kata warm pools on the same
  hardware — reproducible by anyone
- ⬜ Track exec hot-path latency (gRPC → vsock → spawn) with the same rigor
  as fork latency; it dominates agent tokens-to-completion

## 5. Talos + Hetzner reference platform

- ⬜ `docs/platforms/talos-hetzner.md`: machine-config fragments (/dev/kvm,
  modules, hugepages, CoW volume backend), minimal forkd privilege under
  Talos
- ⬜ Evaluate dm-thin / xfs-reflink as alternatives to the btrfs dependency
- ⬜ Hetzner BOM (3× AX nodes) with measured density and cost/sandbox-hour
- ⬜ Measure and publish the nested-virt penalty on EKS/GKE/AKS instead of
  hiding it

## 6. Density and scheduling

- ⬜ Bin-pack forks onto nodes already holding the snapshot; spread across
  failure domains only on request
- ⬜ NUMA pinning + hugepage backing; per-node max density config
- ⬜ Documented overcommit policy + saturation behavior
- ⬜ Pending-claims metric for Karpenter/cluster-autoscaler; static
  capacity-planning guide for bare metal

## 7. Ergonomics, UX, and compat

The DX gap against E2B/Daytona is the adoption bottleneck once the core is
verified. In rough order of leverage:

- ⬜ **MCP server interface** — expose sandboxes as an MCP tool server
  (create/exec/files/fork as tools); every MCP-speaking agent becomes a user
  with zero SDK integration. Candidate to pull forward as soon as the exec
  path is verified end-to-end.
- ⬜ Streaming exec (stdout/stderr), stdin, **PTY mode**, file transfer,
  port forwarding — the daily-driver agent-harness needs
- ⬜ Code-interpreter-compatible API shim (drop-in for LangChain/LlamaIndex
  sandbox integrations)
- ⬜ `kubectl sandbox` plugin (claim/exec/cp/port-forward for operators)
- ⬜ TypeScript SDK (currently does not exist; README labels it planned)
  + shared Python/TS conformance suite; README samples executed in CI
- ⬜ Agent Sandbox (k8s-sigs) CRD adapter — assess, decide, document either way
- ⬜ `make dev` one-command local story (kind + mock engine today; document
  KVM-passthrough path)
- ⬜ Helm chart (README previously implied one exists; it does not yet)

## 8. Observability

- ⬜ OpenTelemetry trace per claim/fork: controller decision → forkd gRPC →
  KVM restore → guest-ready → first exec
- ⬜ Metrics: pending-claims depth, snapshot distribution lag, orphan-sweep
  counts, per-pool claim error rates (some specced metrics are not yet
  exported — README lists only what is real)
- ⬜ Toggleable structured audit log of every exec/file op
