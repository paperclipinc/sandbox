# sandbox

Millisecond sandbox forking for AI agents on Kubernetes.

`sandbox` gives your agents isolated, forkable computers: Firecracker microVMs that restore from memory snapshots in milliseconds, fork into parallel attempts, and persist durable workspaces. Declarative CRDs on your cluster, or fully hosted by us.

```bash
# Create a pool of pre-snapshotted Python sandboxes
kubectl apply -f examples/python-pool.yaml

# Claim one (forks from a warm snapshot)
kubectl apply -f examples/claim.yaml

# Fork it 3 times to try parallel approaches
kubectl apply -f examples/fork.yaml
```

```python
from agent_run import Sandbox

sandbox = Sandbox.create("python-agent-pool")
result = sandbox.exec("import numpy as np; print(np.mean([1,2,3,4,5]))")
print(result.stdout)  # 3.0

# Fork the sandbox to try two approaches in parallel
fork_a, fork_b = sandbox.fork(2)
fork_a.exec("open('plan_a.txt', 'w').write('conservative approach')")
fork_b.exec("open('plan_b.txt', 'w').write('aggressive approach')")
```

## Why

Agent harnesses need fast, isolated environments where agents can read and write files, install packages, and run untrusted code. Every existing option forces a trade you should not have to make: speed without ownership, isolation without forking, Kubernetes-nativeness without warm starts, durability as someone else's proprietary cloud.

Our goal is the pareto frontier of agent runtimes: for the axes that matter (cold-start latency, fork semantics, hardware isolation, durable state, Kubernetes integration, data ownership, open source, cost at rest), no alternative should beat `sandbox` on one axis without losing on another that you also care about. Where we are not there yet, the [roadmap](ROADMAP.md) says so explicitly.

Two ways to run it:

- **Self-hosted**: any Kubernetes cluster with KVM nodes. Your data never leaves your infrastructure. Bare metal (Hetzner + Talos is the reference platform) is a first-class target.
- **Hosted**: a managed service operated by us, same engine and same API, for teams that want milliseconds without managing nodes.

## Features

**Fast**
- Sandbox claims fork from pre-built memory snapshots via Firecracker CoW restore; fork->first-exec is now measured reproducibly in CI (shared-runner-class, see [BENCHMARKS.md](BENCHMARKS.md)), and bare-metal reference numbers are pending the reference hardware ([#15](https://github.com/paperclipinc/sandbox/issues/15))
- Pre-snapshotted pools built from OCI images: `internal/ociroot` flattens an image into an ext4 rootfs and runs your `init` steps in the VM before snapshotting, so there is no cold start on claim ([#10](https://github.com/paperclipinc/sandbox/issues/10), see [docs/templates.md](docs/templates.md))
- CoW memory sharing across forks: you pay for unique pages, not for copies
- Content-addressed snapshot distribution: forks pull only the missing sha256 chunks from a holder node over mTLS, so pool rebuilds ship deltas not whole multi-GB images, with a snapshot version-compatibility contract that refuses to restore an incompatible snapshot ([#14](https://github.com/paperclipinc/sandbox/issues/14), [#32](https://github.com/paperclipinc/sandbox/issues/32), see [docs/snapshot-distribution.md](docs/snapshot-distribution.md))

**Forkable**
- Fork any running sandbox into N independent copies (`SandboxFork`)
- Per-volume fork policies ([#11](https://github.com/paperclipinc/sandbox/issues/11)): `Fresh` (new empty ext4) and `Snapshot` (reflink copy-on-write) are implemented end to end and CI-proven on a reflink-capable filesystem (Fresh round-trips a write; Snapshot forks are CoW-independent); `Share` and `Clone` are partial. External volume sources (S3/GCS/PVC/Git) are not yet materialized. See [docs/volumes.md](docs/volumes.md)
- Each fork gets its own KVM-isolated microVM
- Live forks of secret-holding sandboxes are rejected unless explicitly opted in; forks never silently inherit credentials

**Kubernetes-native**
- Declarative CRDs: `SandboxPool`, `SandboxClaim`, `SandboxFork`
- Templates with volume topology and fork behavior
- Capacity-aware scheduling ([#17](https://github.com/paperclipinc/sandbox/issues/17)): CoW bin-packing onto warm snapshot-holders, an overcommit budget checked against CoW-aware memory accounting, and backpressure that pends claims with a typed `NoCapacity` condition rather than OOMing a node (see [docs/scheduling.md](docs/scheduling.md))
- RBAC and namespace scoping today; pod-native execution (quotas, NetworkPolicy, scheduler truth) is the husk-pod workstream ([#18](https://github.com/paperclipinc/sandbox/issues/18)). Sandboxes are not pods today
- Works with Prometheus and standard k8s tooling

**Secure**
- Hardware isolation: a dedicated kernel per sandbox (KVM/Firecracker). Production forks run under the Firecracker jailer (per-VM UID, chroot, cgroup); the dropped-capability set and the unjailed dev paths are tracked as threat-model residuals ([#2](https://github.com/paperclipinc/sandbox/issues/2), [docs/threat-model.md](docs/threat-model.md))
- Credentials injected at claim time over vsock, never baked into snapshots; undeliverable secrets fail the claim instead of lying about readiness
- Encryption at rest with crypto-shredding ([#31](https://github.com/paperclipinc/sandbox/issues/31), behind `--enable-encryption`): each template snapshot and its volumes are built inside a per-scope LUKS2 container so the bytes at rest are ciphertext; deletion wipes the LUKS keyslots, making the data unrecoverable. The per-template key is generated by the controller, held by forkd in memory only, and never logged. KMS/HSM envelope wrapping and per-workspace scope ([#21](https://github.com/paperclipinc/sandbox/issues/21)) are follow-ups. See [docs/encryption.md](docs/encryption.md)
- Default-deny guest networking ([#47](https://github.com/paperclipinc/sandbox/issues/47), opt-in per node): host-side nftables egress allowlists by literal IP:port AND by name through a controlled per-node DNS resolver; the guest cannot spoof or influence enforcement. See [docs/networking.md](docs/networking.md)
- CoW-aware metering ([#33](https://github.com/paperclipinc/sandbox/issues/33)): the shared template page set is counted once, not once per fork, so the billing and scheduling primitive reflects the honest physical footprint. See [docs/metering.md](docs/metering.md)
- Honest threat model with per-boundary status: [docs/threat-model.md](docs/threat-model.md). No external security review has happened yet, and the document says exactly what is open

**Operable**
- Node and controller Prometheus metrics, a per-claim OpenTelemetry trace (`--otlp-endpoint`), and a toggleable structured audit log of every exec/file op (`--audit-log`) that records command, path, and byte counts but never content or secrets ([#29](https://github.com/paperclipinc/sandbox/issues/29), see [docs/observability.md](docs/observability.md))
- `kubectl sandbox` plugin (`ls` / `ps`) for operators
- Failure and GC semantics: claim TTLs (`maxLifetime`, `idleTimeout`), orphan-VM sweeps, and controller-restart reconciliation of the desired set are implemented and CI-proven; forkd crash reaping and saturation queuing are open ([#12](https://github.com/paperclipinc/sandbox/issues/12), see [docs/failure-gc.md](docs/failure-gc.md))

**Surfaces**
- Python SDK (`sdk/python`) and TypeScript SDK (`@agentrun/sdk`)
- `agentrun` CLI with one-command local dev (`agentrun dev up`) and an MCP server (`agentrun-mcp`) that exposes sandboxes as MCP tools
- Bare metal is a first-class target: Talos + Hetzner is the reference platform ([#16](https://github.com/paperclipinc/sandbox/issues/16), see [docs/platforms/talos-hetzner.md](docs/platforms/talos-hetzner.md))

## Architecture

```
+-------------------------------------------------------------+
|  k8s Control Plane                                          |
|                                                             |
|  SandboxTemplate --> SandboxPool --> SandboxClaim           |
|        |                  |               |                 |
|        |            (manages snapshot     (fork from        |
|        |             lifecycle)            snapshot)        |
|        v                  |               |                 |
|  SandboxFork              |               |                 |
|  (fork running sandbox)   v               v                 |
|  +-------------------------------------------------------+ |
|  |  controller (Deployment)                              | |
|  |  reconciles CRDs, picks nodes, calls forkd over gRPC  | |
|  +-------------------------------------------------------+ |
+--------------+----------------------------------------------+
               | gRPC
+--------------v----------------------------------------------+
|  KVM-capable nodes                                          |
|  +-------------------------------------------------------+  |
|  |  forkd (DaemonSet, one per node)                      |  |
|  |  - builds template snapshots (boot, init, snapshot)   |  |
|  |  - forks: a fresh Firecracker process restores the    |  |
|  |    snapshot; memory is CoW-shared across forks        |  |
|  |  - serves exec/files over HTTP, bridged to the guest  |  |
|  |    agent via vsock                                    |  |
|  |  +-----+ +-----+ +-----+ +-----+                      |  |
|  |  | VM  | | VM  | | VM  | | VM  |  <- one KVM microVM  |  |
|  |  +-----+ +-----+ +-----+ +-----+     per sandbox      |  |
|  +-------------------------------------------------------+  |
+-------------------------------------------------------------+
```

Sandboxes are not pods. Pod-scoped Kubernetes mechanisms do not govern sandbox VMs today; where we provide an equivalent, it is documented as ours, and the husk-pod workstream closes the gap properly.

## Quick start

### Local development (no KVM required)

One command brings up a local kind cluster running a mock control plane, then the
`agentrun` CLI drives the full claim path:

```bash
go build -o agentrun ./cmd/agentrun/

# build + load the dev images the overlay references, then bring up the cluster
docker build -f Dockerfile.controller -t agent-run-controller:ci .
docker build -f Dockerfile.forkd -t agent-run-forkd:ci .
kind create cluster --name agentrun-dev --config hack/kind-config.yaml
kind load docker-image agent-run-controller:ci --name agentrun-dev
kind load docker-image agent-run-forkd:ci --name agentrun-dev

./agentrun dev up --skip-cluster-create
./agentrun sandbox create --pool dev-default   # reaches Ready on the mock engine
./agentrun sandbox ls
./agentrun run echo hello --pool dev-default
./agentrun dev down
```

The local dev cluster uses the mock fork engine (no KVM): claims reconcile to
`Ready` and control-plane dispatch works, but a real in-VM `exec` needs a node
with `/dev/kvm`. See [docs/cli.md](docs/cli.md) for the full command reference,
the backends, and what is proven. For the no-cluster REST loop, run
`go run ./cmd/sandbox-server --mock --addr :8080` and use the Python SDK
(`sdk/python`).

### On a cluster

```bash
kubectl apply -f deploy/crds/
kubectl apply -f deploy/controller/
kubectl apply -f deploy/daemon/
```

A Helm chart is planned ([#37](https://github.com/paperclipinc/sandbox/issues/37) tracks release machinery). Nodes need `/dev/kvm` and the label `agentrun.dev/kvm=true`; the controller discovers forkd pods automatically.

### Create a pool, claim, fork

```yaml
apiVersion: agentrun.dev/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-agent
spec:
  image: python:3.12-slim
  init:
    - "pip install numpy pandas requests"
  resources:
    cpu: "1"
    memory: "512Mi"
  volumes:
    - name: workspace
      size: 5Gi
      forkPolicy: Snapshot
---
apiVersion: agentrun.dev/v1alpha1
kind: SandboxPool
metadata:
  name: python-agent-pool
spec:
  templateRef:
    name: python-agent
  replicas: 10
---
apiVersion: agentrun.dev/v1alpha1
kind: SandboxClaim
metadata:
  name: agent-session-1
spec:
  poolRef:
    name: python-agent-pool
  secrets:
    - name: anthropic-key
      secretRef:
        name: agent-secrets
        key: ANTHROPIC_API_KEY
---
apiVersion: agentrun.dev/v1alpha1
kind: SandboxFork
metadata:
  name: parallel-attempt
spec:
  sourceRef:
    name: agent-session-1
  replicas: 3
  allowSecretInheritance: true   # forks duplicate memory; opt in knowingly
```

## SDKs

Two SDKs ship today, both speaking the same `forkd` / `sandbox-server` HTTP wire protocol:

- **Python** (`sdk/python`, pre-PyPI: `pip install -e .`): direct mode against a standalone `sandbox-server` and cluster mode over the CRDs.
- **TypeScript** (`sdk/typescript`, `@agentrun/sdk`): the same direct and cluster modes for Node 18+. The wire protocol is exercised against a mock server by 31 conformance tests and the package is type-checked and packed in CI (`typescript-sdk` job); real in-VM exec is proven by the KVM CI of the underlying API; npm publication is a release follow-up. Parity table in [sdk/typescript/README.md](sdk/typescript/README.md).

Beyond the SDKs:

- the `agentrun` CLI (`agentrun run`, `agentrun sandbox create|ls|exec|fork|terminate`, and `agentrun dev up|down` for one-command local dev on a mock control plane) ([#26](https://github.com/paperclipinc/sandbox/issues/26), see [docs/cli.md](docs/cli.md));
- an MCP server (`agentrun-mcp`) that exposes sandboxes as MCP tools so any MCP-speaking agent can use them with zero SDK integration ([#27](https://github.com/paperclipinc/sandbox/issues/27), see [docs/mcp.md](docs/mcp.md)).

The target developer surface (three-line common path, git-shaped workspace verbs, capability-budgeted self-service for agents) is specified in [docs/api/v2-spec.md](docs/api/v2-spec.md).

## Comparison

The honest version: a numbers table belongs here only when our benchmark harness can regenerate it against the actual competitors on the same hardware, with scripts in this repo so anyone can reproduce or refute it. That harness is [#15](https://github.com/paperclipinc/sandbox/issues/15). Until then, the qualitative pareto map across every alternative we know of:

| | sandbox | E2B | Modal | Daytona | Morph | Cloudflare | Agent Sandbox (k8s-sigs) | Kata/KubeVirt | raw Firecracker |
|---|---|---|---|---|---|---|---|---|---|
| Hardware isolation per session | KVM microVM | microVM | gVisor | container/VM | microVM | V8 isolate | Kata option | KVM | KVM |
| Snapshot fork of running state | yes, core primitive | snapshot/resume | memory snapshots | no | yes (Infinibranch) | no | no | no | build it yourself |
| Warm-pool millisecond claims | yes (design center) | warm pools | warm pools | workspaces | yes | instant isolates | 1-3s cold | seconds | build it yourself |
| Durable forkable workspaces | Workspace CRD (in design, [#21](https://github.com/paperclipinc/sandbox/issues/21)) | no | volumes | workspaces | yes, proprietary | no | PVCs | PVCs | no |
| Kubernetes-native API | CRDs | SaaS API | SaaS API | SaaS/OSS | SaaS API | SaaS API | CRDs | CRDs | no |
| Self-hostable | yes, any KVM cluster | partial OSS | no | OSS core | no | no | yes | yes | yes |
| Hosted option | planned (same engine) | yes | yes | yes | yes | yes | no | no | no |
| Your data stays on your infra | yes (self-hosted) | no | no | partial | no | no | yes | yes | yes |
| Open source | Apache 2.0 | partial | no | partial | no | no | Apache 2.0 | Apache 2.0 | Apache 2.0 |

Positioning, in one sentence per rival class: SaaS runtimes (E2B, Modal, Daytona, Cloudflare) are fast but your agents' code, data, and credentials run on someone else's infrastructure with no self-host path at equivalent capability; Morph built the right state model (branch/restore) as a proprietary cloud, and our Workspace primitive targets the same semantics open source at fork(2) speeds; Agent Sandbox (k8s-sigs) is winning the Kubernetes API standard without a snapshot-fork engine, which is why we are building a conformance facade to be its fastest backend ([#19](https://github.com/paperclipinc/sandbox/issues/19)) rather than fighting it; Kata, KubeVirt, and raw Firecracker give you the isolation primitive and leave the pool, fork, distribution, and agent-API layers as your problem.

If an alternative beats us on an axis you care about and we have no roadmap line that closes it, that is a bug in our strategy: open an issue.

## Monitoring

Node-level Prometheus metrics exposed by forkd at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `agentrun_fork_duration_seconds` | histogram | Fork latency as measured by forkd |
| `agentrun_active_sandboxes` | gauge | Currently running sandboxes per node |
| `agentrun_memory_shared_bytes` | gauge | CoW shared memory across forks (counted once per template) |
| `agentrun_memory_unique_bytes` | gauge | Per-fork unique (private-dirty) memory |
| `agentrun_cow_memory_savings_bytes` | gauge | Bytes the CoW model reveals are not consumed per fork (naive minus CoW-aware) |
| `agentrun_metered_disk_bytes` | gauge | CoW-aware metered disk for reflink (Snapshot) volumes |

Controller-level Prometheus metrics exposed by the controller at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `agentrun_claim_pending_total` | counter | Times a claim requeued for no node with a ready snapshot |
| `agentrun_orphan_sweeps_total` | counter | Orphan sandbox VMs terminated by the garbage collector |
| `agentrun_claim_errors_total{pool,reason}` | counter | Terminal claim failures by pool and reason |
| `agentrun_pool_ready_snapshots{pool}` | gauge | Ready snapshots per pool at the last reconcile |

Tracing: a per-claim OpenTelemetry trace (controller decision to forkd gRPC to KVM restore) exports to an OTLP collector when both the controller and forkd run with `--otlp-endpoint`; trace ids propagate across the gRPC boundary and spans carry no secrets. Audit log: forkd records a toggleable structured JSON line per exec or file operation with `--audit-log` (a path or `stderr`), logging command/path and byte counts but never content or secrets. Operators can list sandboxes and forks with the `kubectl-sandbox` plugin (`kubectl sandbox ls` / `ps`). forkd also serves the CoW-aware metering report on the operational `GET /v1/metering` endpoint. See [docs/observability.md](docs/observability.md) for the full trace model, the audit record shape, the metric catalogue, and plugin install, and [docs/metering.md](docs/metering.md) for the CoW accounting rules. End-to-end trace ids stamped across pods, logs, and Hubble remain on the roadmap ([#29](https://github.com/paperclipinc/sandbox/issues/29)).

## Documentation

Per-topic docs in [`docs/`](docs/):

- Templates and the OCI image to rootfs build: [docs/templates.md](docs/templates.md)
- Volume fork policies: [docs/volumes.md](docs/volumes.md)
- Snapshot format and version-compatibility: [docs/snapshot-format.md](docs/snapshot-format.md)
- Snapshot distribution (content-addressed transfer): [docs/snapshot-distribution.md](docs/snapshot-distribution.md)
- Guest networking and egress: [docs/networking.md](docs/networking.md)
- Encryption at rest and crypto-shredding: [docs/encryption.md](docs/encryption.md)
- CoW-aware metering: [docs/metering.md](docs/metering.md)
- Density and scheduling: [docs/scheduling.md](docs/scheduling.md)
- Observability (traces, metrics, audit, plugin): [docs/observability.md](docs/observability.md)
- Failure and GC semantics: [docs/failure-gc.md](docs/failure-gc.md)
- Fork-engine correctness: [docs/fork-correctness.md](docs/fork-correctness.md)
- Threat model: [docs/threat-model.md](docs/threat-model.md)
- `agentrun` CLI: [docs/cli.md](docs/cli.md)
- MCP server: [docs/mcp.md](docs/mcp.md)
- Talos + Hetzner reference platform: [docs/platforms/talos-hetzner.md](docs/platforms/talos-hetzner.md)
- Target API surface (v2 spec): [docs/api/v2-spec.md](docs/api/v2-spec.md)
- Benchmark methodology: [BENCHMARKS.md](BENCHMARKS.md)

## Project status

Early development, pre-1.0. The control plane is real end-to-end (claim to running sandbox, proven in CI against mock engines and real Firecracker VMs). Implemented and CI-proven: OCI image to rootfs template builds, mTLS plus per-sandbox token auth, the jailer chroot/uid mechanics (with the dropped-capability set and dev paths tracked as residuals), RNG reseed and clock resync on fork (the remaining fork-correctness items are tracked in [#3](https://github.com/paperclipinc/sandbox/issues/3)), `Fresh` and `Snapshot` volume policies, content-addressed snapshot integrity, version-compatibility, and peer-to-peer distribution, default-deny IP and name-based egress, CoW-aware metering, capacity-aware scheduling, encryption at rest with crypto-shredding, the observability surface, the MCP server, the CLI, and the Python and TypeScript SDKs.

Documented follow-ups and targets, not yet shipped: KMS/HSM key wrapping and per-workspace encryption scope ([#21](https://github.com/paperclipinc/sandbox/issues/21)), measured bare-metal density and latency numbers ([#15](https://github.com/paperclipinc/sandbox/issues/15)), pod-native execution via husk pods ([#18](https://github.com/paperclipinc/sandbox/issues/18)), pluggable engine tiers ([#43](https://github.com/paperclipinc/sandbox/issues/43)), the durable Workspace ([#21](https://github.com/paperclipinc/sandbox/issues/21)), and the remaining failure/GC items ([#12](https://github.com/paperclipinc/sandbox/issues/12)). Sandboxes are not pods today, and pod-scoped Kubernetes policies do not govern them until husk pods land. [ROADMAP.md](ROADMAP.md) is the single source for what is done, in progress, and gated; the operating rule is that this repository never describes a system that does not exist.

Contributions welcome: see [CONTRIBUTING.md](CONTRIBUTING.md) and [CLAUDE.md](CLAUDE.md) for conventions, [SECURITY.md](SECURITY.md) for vulnerability reporting.

## License

Apache 2.0
