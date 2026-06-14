# mitos

Millisecond microVM sandbox forking for AI agents on Kubernetes.

`mitos` gives your agents isolated, forkable computers: Firecracker microVMs that restore from memory snapshots in milliseconds, fork into parallel attempts, and persist durable workspaces. Declarative CRDs (`mitos.run`) on your cluster, or fully hosted by us.

```bash
# Create a pool of pre-snapshotted Python sandboxes
kubectl apply -f examples/python-pool.yaml

# Claim one (forks from a warm snapshot)
kubectl apply -f examples/claim.yaml

# Fork it 3 times to try parallel approaches
kubectl apply -f examples/fork.yaml
```

```python
from mitos import AgentRun

c = AgentRun()                                   # kubeconfig or in-cluster; autodetected

# One-liner: a lazy default pool is created for the image if you have none.
sb = c.sandbox("python", ready=True)             # claims a warm sandbox, waits Ready
result = sb.exec("python -c 'import numpy as np; print(np.mean([1,2,3,4,5]))'")
print(result.stdout)                             # 3.0

# Fork the running sandbox to try two approaches against shared warmed state.
# Live fork runs on the raw-forkd engine path today; wiring it on the husk
# pod-native default is a tracked follow-up (#18).
fork_a, fork_b = sb.fork(2)
fork_a.exec("python -c \"open('/workspace/plan_a.txt','w').write('conservative')\"")
fork_b.exec("python -c \"open('/workspace/plan_b.txt','w').write('aggressive')\"")

sb.terminate()
```

`c.sandbox("python")` lazily creates a default pool `mitos-default-python` (a SandboxTemplate plus a SandboxPool) if you have none; pass `pool="my-pool"` to use an existing pool, which never creates anything. Errors raise `AgentRunError(code, cause, remediation)`.

Beyond `exec`, the same `Sandbox` gives agents a stateful code interpreter, streaming output, and an interactive terminal:

```python
# Streaming exec: callbacks fire per chunk; the ExecResult still carries the aggregate.
sb.exec("pip install rich", on_stdout=lambda b: print(b.decode(), end=""))

# Stateful code interpreter: state persists across run_code calls for the sandbox lifetime.
ex = sb.run_code("import pandas as pd; df = pd.DataFrame({'x':[1,2,3]}); df.describe()")
print(ex.text)            # the REPL's last value, rendered
for r in ex.results:      # rich multi-MIME display artifacts (tables, images, ...)
    print(r.mime)
# run_code returns a KernelUnavailable error until the kernel ships in the husk base image.

# Detach a long-running process and keep working.
sb.exec_background("python train.py > /workspace/train.log 2>&1")
```

The async client (`AsyncAgentRun`) mirrors these for the hot paths and adds `create_pty()` for an interactive terminal over WebSocket. The TypeScript SDK (`@mitos/sdk`) exposes the same surface.

Streaming exec (`/v1/exec/stream`) and the interactive PTY (`/v1/pty`) require the raw-forkd path or a husk template snapshot rebuilt with the current guest agent: the agent baked into today's husk template snapshot predates the vsock streaming/PTY frame protocol, so on the husk default the stream and the PTY WebSocket close early. Blocking exec (`/v1/exec`) is unaffected and works on the husk default. Rebuilding the husk template guest agent at the current commit is a tracked follow-up ([#24](https://github.com/paperclipinc/mitos/issues/24)).

## Why

Agent harnesses need fast, isolated environments where agents can read and write files, install packages, and run untrusted code. Every existing option forces a trade you should not have to make: speed without ownership, isolation without forking, Kubernetes-nativeness without warm starts, durability as someone else's proprietary cloud.

Our goal is the pareto frontier of agent runtimes: for the axes that matter (cold-start latency, fork semantics, hardware isolation, durable state, Kubernetes integration, data ownership, open source, cost at rest), no alternative should beat `mitos` on one axis without losing on another that you also care about. Where we are not there yet, the [roadmap](ROADMAP.md) says so explicitly.

Two ways to run it:

- **Self-hosted**: any Kubernetes cluster with KVM nodes. Your data never leaves your infrastructure. Bare metal (Hetzner + Talos is the reference platform) is a first-class target.
- **Hosted**: a managed service operated by us, same engine and same API, for teams that want milliseconds without managing nodes.

## Features

**Fast**
- Sandbox claims fork from pre-built memory snapshots via Firecracker CoW restore; fork->first-exec is measured reproducibly in CI (shared-runner-class, see [BENCHMARKS.md](BENCHMARKS.md)). On the bare-metal reference node (Hetzner dedicated i7-6700, Talos Linux kernel 6.18, Firecracker v1.15, the default husk path) a first reference run measured **warm-claim activate P50 ~27 ms** (N=11; the time the controller records to load the snapshot, run the fork-correctness handshake, and reach guest-ready, integrity gate enforced), **~6-16 ms snapshot restore**, and **~3 MiB marginal memory per forked sandbox** via CoW page sharing. The activate figure is engine time, not the claim->Ready wall clock (~0.5-1.8 s here, reconcile-bound). Every number is reproducible from [`bench/husk-activate-latency.sh`](bench/husk-activate-latency.sh) and [bench/results](bench/results). The competitor-comparison harness on matched hardware is [#15](https://github.com/paperclipinc/mitos/issues/15)
- Pre-snapshotted pools built from OCI images: `internal/ociroot` flattens an image into an ext4 rootfs and runs your `init` steps in the VM before snapshotting, so there is no cold start on claim ([#10](https://github.com/paperclipinc/mitos/issues/10), see [docs/templates.md](docs/templates.md))
- CoW memory sharing across forks: you pay for unique pages, not for copies
- Content-addressed snapshot distribution: forks pull only the missing sha256 chunks from a holder node over mTLS, so pool rebuilds ship deltas not whole multi-GB images, with a snapshot version-compatibility contract that refuses to restore an incompatible snapshot ([#14](https://github.com/paperclipinc/mitos/issues/14), [#32](https://github.com/paperclipinc/mitos/issues/32), see [docs/snapshot-distribution.md](docs/snapshot-distribution.md))

**Forkable**
- Fork any running sandbox into N independent copies (`SandboxFork`). This works today on the raw-forkd engine path, where the source VM is owned by forkd's in-process engine. On the husk pod-native default the source VM is owned by the husk pod's stub rather than forkd's engine, so live fork is not yet wired there; it is a tracked follow-up of the husk migration ([#18](https://github.com/paperclipinc/mitos/issues/18))
- Per-volume fork policies ([#11](https://github.com/paperclipinc/mitos/issues/11)): `Fresh` (new empty ext4) and `Snapshot` (reflink copy-on-write) are implemented end to end and CI-proven on a reflink-capable filesystem (Fresh round-trips a write; Snapshot forks are CoW-independent); `Share` and `Clone` are partial. External volume sources (S3/GCS/PVC/Git) are not yet materialized. See [docs/volumes.md](docs/volumes.md)
- Each fork gets its own KVM-isolated microVM
- Live forks of secret-holding sandboxes are rejected unless explicitly opted in; forks never silently inherit credentials

**Kubernetes-native**
- Declarative CRDs: `SandboxPool`, `SandboxClaim`, `SandboxFork`
- Templates with volume topology and fork behavior
- Capacity-aware scheduling ([#17](https://github.com/paperclipinc/mitos/issues/17)): CoW bin-packing onto warm snapshot-holders, an overcommit budget checked against CoW-aware memory accounting, a `MaxSandboxes` host-DoS ceiling enforced with an atomic slot reservation, and backpressure that pends claims with a typed `NoCapacity` condition rather than OOMing a node (see [docs/scheduling.md](docs/scheduling.md))
- Demand-driven warm-pool autoscaling: `SandboxPool.spec.autoscale` (`minWarm`/`maxWarm`/`targetSpare`) scales the dormant husk-pod count to `clamp(inUse + targetSpare, minWarm, maxWarm)` from live claim demand, with an anti-thrash scale-down cooldown; a fixed pool is just `minWarm == replicas`
- RBAC and namespace scoping; pod-native execution via unprivileged, PSA-restricted husk pods is the DEFAULT ([#18](https://github.com/paperclipinc/mitos/issues/18)): the per-sandbox VM runs inside an unprivileged pod (`/dev/kvm` from a device plugin, not `privileged`), so its CPU/memory requests are scheduler truth and PSA governs the pod. The sandbox itself is the VM, not the pod
- Works with Prometheus and standard k8s tooling

**Secure**
- Hardware isolation: a dedicated kernel per sandbox (KVM/Firecracker). The default husk path runs each VM in its own unprivileged, PSA-restricted pod, which IS the per-VM isolation boundary (one VM per pod, so an in-pod jailer was evaluated and declined as redundant; see [#18](https://github.com/paperclipinc/mitos/issues/18) and [docs/threat-model.md](docs/threat-model.md)); raw-forkd, the fallback, runs forks under the Firecracker jailer (per-VM UID, chroot, cgroup), with its dropped-capability set tracked as a threat-model residual ([#2](https://github.com/paperclipinc/mitos/issues/2))
- Credentials injected at claim time over vsock, never baked into snapshots; undeliverable secrets fail the claim instead of lying about readiness
- Encryption at rest with crypto-shredding ([#31](https://github.com/paperclipinc/mitos/issues/31), behind `--enable-encryption`): each template snapshot and its volumes are built inside a per-scope LUKS2 container so the bytes at rest are ciphertext; deletion wipes the LUKS keyslots, making the data unrecoverable. The per-template data key is generated by the controller, held by forkd in memory only, and never logged. KMS envelope wrapping of that key is implemented ([#31](https://github.com/paperclipinc/mitos/issues/31), fail-closed, plaintext keys never persisted to disk); HSM-backed keys and per-workspace encryption scope ([#21](https://github.com/paperclipinc/mitos/issues/21)) are follow-ups. See [docs/encryption.md](docs/encryption.md)
- Default-deny guest networking ([#47](https://github.com/paperclipinc/mitos/issues/47), opt-in per node): host-side nftables egress allowlists by literal IP:port AND by name through a controlled per-node DNS resolver; the guest cannot spoof or influence enforcement. See [docs/networking.md](docs/networking.md)
- CoW-aware metering ([#33](https://github.com/paperclipinc/mitos/issues/33)): the shared template page set is counted once, not once per fork, so the billing and scheduling primitive reflects the honest physical footprint. See [docs/metering.md](docs/metering.md)
- Honest threat model with per-boundary status: [docs/threat-model.md](docs/threat-model.md). No external security review has happened yet, and the document says exactly what is open

**Operable**
- Node and controller Prometheus metrics, a per-claim OpenTelemetry trace (`--otlp-endpoint`), and a toggleable structured audit log of every exec/file op (`--audit-log`) that records command, path, and byte counts but never content or secrets ([#29](https://github.com/paperclipinc/mitos/issues/29), see [docs/observability.md](docs/observability.md))
- `kubectl sandbox` plugin (`ls` / `ps`) for operators
- Failure and GC semantics: claim TTLs (`maxLifetime`, `idleTimeout`), orphan-VM sweeps, controller-restart reconciliation of the desired set, forkd crash reaping (running VMs re-adopted or reaped via an on-disk journal, PID-recycle and TOCTOU guarded), node-loss handling tied to a forkd liveness probe, and saturation backpressure (a typed `NoCapacity` condition with bounded backoff, then a clean capacity-exhaustion failure) are implemented and CI-proven ([#12](https://github.com/paperclipinc/mitos/issues/12), see [docs/failure-gc.md](docs/failure-gc.md))

**Agent-runtime DX**
- Streaming exec: incremental stdout/stderr and background processes over the sandbox API, with a streaming-callback exec in both SDKs ([#24](https://github.com/paperclipinc/mitos/issues/24)). Works on the raw-forkd path or a husk template rebuilt with the current guest agent; on the husk default the stream closes early because the template's baked agent predates the streaming frame protocol (rebuild tracked, #24). Blocking exec is unaffected and works on the husk default
- Code interpreter: `run_code` with a stateful kernel and rich multi-MIME results, in both SDKs and the MCP server. It fails closed with a `KernelUnavailable` error until the kernel ships in the husk cluster base image (a tracked follow-up below)
- Interactive PTY: a token-gated bidirectional WebSocket terminal (`sandbox.pty`), guest-side PTY shell pumped over vsock ([#24](https://github.com/paperclipinc/mitos/issues/24)). Same husk caveat as streaming exec: it requires the raw-forkd path or a husk template rebuilt with the current agent; on the husk default the WebSocket closes early until that agent rebuild (tracked, #24)
- LLM-legible errors: every failure carries `{code, cause, remediation}` ([#28](https://github.com/paperclipinc/mitos/issues/28)), parsed by both SDKs into a structured `AgentRunError`

**Surfaces**
- Python SDK (`sdk/python`) and TypeScript SDK (`@mitos/sdk`), both with a one-liner `sandbox(image)`, a lazy default pool, `from_name` reconnect, streaming exec, and (Python) an async client
- `mitos` CLI with one-command local dev (`mitos dev up`) and an MCP server (`mitos-mcp`) that exposes sandboxes as MCP tools
- Bare metal is a first-class target: Talos + Hetzner is the reference platform ([#16](https://github.com/paperclipinc/mitos/issues/16), see [docs/platforms/talos-hetzner.md](docs/platforms/talos-hetzner.md))

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
`mitos` CLI drives the full claim path:

```bash
go build -o mitos ./cmd/mitos/

# build + load the dev images the overlay references, then bring up the cluster
docker build -f Dockerfile.controller -t mitos-controller:ci .
docker build -f Dockerfile.forkd -t mitos-forkd:ci .
kind create cluster --name mitos-dev --config hack/kind-config.yaml
kind load docker-image mitos-controller:ci --name mitos-dev
kind load docker-image mitos-forkd:ci --name mitos-dev

./mitos dev up --skip-cluster-create
./mitos sandbox create --pool dev-default   # reaches Ready on the mock engine
./mitos sandbox ls
./mitos run echo hello --pool dev-default
./mitos dev down
```

The local dev cluster uses the mock fork engine (no KVM): claims reconcile to
`Ready` and control-plane dispatch works, but a real in-VM `exec` needs a node
with `/dev/kvm`. See [docs/cli.md](docs/cli.md) for the full command reference,
the backends, and what is proven. For the no-cluster REST loop, run
`go run ./cmd/sandbox-server --mock --addr :8080` and use the Python SDK
(`sdk/python`).

### On a cluster

```bash
kubectl apply -k deploy/
```

The self-contained kustomize base installs the CRDs, the controller in the default husk mode, the forkd builder DaemonSet, the `/dev/kvm` device plugin, and the PKI bootstrap, and applies on a real KVM node with no manual patches. A Helm chart is planned ([#37](https://github.com/paperclipinc/mitos/issues/37) tracks release machinery). Nodes need `/dev/kvm` and the label `mitos.run/kvm=true`; the controller discovers forkd pods automatically.

### Create a pool, claim, fork

```yaml
apiVersion: mitos.run/v1alpha1
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
apiVersion: mitos.run/v1alpha1
kind: SandboxPool
metadata:
  name: python-agent-pool
spec:
  templateRef:
    name: python-agent
  replicas: 10
---
apiVersion: mitos.run/v1alpha1
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
apiVersion: mitos.run/v1alpha1
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

- **Python** (`sdk/python`, pre-PyPI: `pip install -e .`): direct mode against a standalone `sandbox-server`, cluster mode over the CRDs, a one-liner `AgentRun().sandbox("python")` with a lazy default pool, `fork()` of a running sandbox, `from_name()` reconnect, `wait_until_ready()`, streaming exec callbacks, and an `AsyncAgentRun` for the hot paths; structured `AgentRunError` carrying `code`/`cause`/`remediation` parsed from the server envelope.
- **TypeScript** (`sdk/typescript`, `@mitos/sdk`): the same direct and cluster modes for Node 18+, the same one-liner `sandbox(image)` and `fromName` reconnect, and a server-envelope-aware `AgentRunError`. The wire protocol is exercised against a mock server by the conformance tests and the package is type-checked and packed in CI (`typescript-sdk` job); real in-VM exec is proven by the KVM CI of the underlying API; npm publication is a release follow-up. Parity table in [sdk/typescript/README.md](sdk/typescript/README.md).

Beyond the SDKs:

- the `mitos` CLI (`mitos run`, `mitos sandbox create|ls|exec|fork|terminate`, and `mitos dev up|down` for one-command local dev on a mock control plane) ([#26](https://github.com/paperclipinc/mitos/issues/26), see [docs/cli.md](docs/cli.md));
- an MCP server (`mitos-mcp`) that exposes sandboxes as MCP tools so any MCP-speaking agent can use them with zero SDK integration ([#27](https://github.com/paperclipinc/mitos/issues/27), see [docs/mcp.md](docs/mcp.md)).

The target developer surface (three-line common path, git-shaped workspace verbs, capability-budgeted self-service for agents) is specified in [docs/api/v2-spec.md](docs/api/v2-spec.md).

## Comparison

The honest version: a numbers table belongs here only when our benchmark harness can regenerate it against the actual competitors on the same hardware, with scripts in this repo so anyone can reproduce or refute it. That harness is [#15](https://github.com/paperclipinc/mitos/issues/15). Until then, the qualitative pareto map across every alternative we know of:

### Where mitos sits on latency

The differentiator is not a single fastest-number claim. `mitos` is, as far as we know, the only open-source, self-hostable, Kubernetes-native runtime whose engine does N-way live copy-on-write fork of a running microVM, and it does so with a **warm-claim activate in the tens-of-ms class** (P50 ~27 ms on the bare-metal reference node; reproducible from [`bench/husk-activate-latency.sh`](bench/husk-activate-latency.sh), full method and node spec in [BENCHMARKS.md](BENCHMARKS.md) and [bench/results](bench/results)). Honest scope: the N-way live CoW fork is the raw-forkd engine path today, where forkd's in-process engine owns the running VM. On the husk pod-native default (the source VM is owned by the husk pod's stub, not forkd's engine) live fork is not yet wired; wiring it on the husk default is in progress and tracked ([#18](https://github.com/paperclipinc/mitos/issues/18)). The warm-claim activate, blocking exec, and `run_code` fail-closed are verified on the husk default on a real KVM cluster.

For context only, a few latency figures other vendors publish. These are **their published numbers, for different operations, on different hardware, measured with different methodology**; they are NOT measured by us and this is NOT a head-to-head claim. We do not assert we beat any of them on any specific operation, because we have not run the matched-hardware comparison (that is [#15](https://github.com/paperclipinc/mitos/issues/15)).

| runtime | published figure (theirs, not ours) | operation they describe |
|---|---|---|
| mitos (ours, measured) | ~27 ms P50 | warm-claim activate (snapshot load + fork-correctness handshake + guest-ready) on the bare-metal reference node |
| E2B | ~150 ms | sandbox create |
| Daytona | sub-90 ms | create from snapshot |
| Modal | sub-second | sandbox create |
| CodeSandbox SDK | ~863 ms / ~495 ms | live fork / memory-resume |
| Fly Machines | < 1 s | machine start |

The rows measure different things on different hardware, so the table is orientation, not a leaderboard. What is comparable and real today is the qualitative pareto map below: the combination of open source, self-hostable, k8s-native, and live snapshot fork is the axis where `mitos` is alone.

| | mitos | E2B | Modal | Daytona | Morph | Cloudflare | Box (box.ascii.dev) | Agent Sandbox (k8s-sigs) | Kata/KubeVirt | raw Firecracker |
|---|---|---|---|---|---|---|---|---|---|---|
| Hardware isolation per session | KVM microVM | microVM | gVisor | container/VM | microVM | V8 isolate | VM | Kata option | KVM | KVM |
| Snapshot fork of running state | yes, core primitive | snapshot/resume | memory snapshots | no | yes (Infinibranch) | no | disk fork (`box fork`) | no | no | build it yourself |
| Warm-pool millisecond claims | yes (design center) | warm pools | warm pools | workspaces | yes | instant isolates | not published | 1-3s cold | seconds | build it yourself |
| Durable forkable workspaces | Workspace CRD (in design, [#21](https://github.com/paperclipinc/mitos/issues/21)) | no | volumes | workspaces | yes, proprietary | yes (disk) | no | PVCs | PVCs | no |
| Kubernetes-native API | CRDs | SaaS API | SaaS API | SaaS/OSS | SaaS API | SaaS API | agent-native CLI | CRDs | CRDs | no |
| Self-hostable | yes, any KVM cluster | partial OSS | no | OSS core | no | no | no | yes | yes | yes |
| Hosted option | planned (same engine) | yes | yes | yes | yes | yes | yes (only) | no | no | no |
| Your data stays on your infra | yes (self-hosted) | no | no | partial | no | no | no | yes | yes | yes |
| Open source | Apache 2.0 | partial | no | partial | no | no | no | Apache 2.0 | Apache 2.0 | Apache 2.0 |

Positioning, in one sentence per rival class: SaaS runtimes (E2B, Modal, Daytona, Cloudflare) are fast but your agents' code, data, and credentials run on someone else's infrastructure with no self-host path at equivalent capability; Morph built the right state model (branch/restore) as a proprietary cloud, and our Workspace primitive targets the same semantics open source at fork(2) speeds; Box (box.ascii.dev) is a hosted-only disk-fork sandbox SaaS with an agent-native CLI (`box prompt`, `box fork`) and a 60fps virtual desktop, which validates the agent-native direction we take with `mitos` and MCP; our differentiators stay the memory-snapshot fork (a `fork(2)`-speed restore of running state, not a disk roundtrip), self-host-first delivery, and CoW-aware per-page metering, and a streamed desktop for computer-use agents is a tracked use-case for us, not a commitment (Box publishes no latency benchmark, so we make no comparison claim there); Agent Sandbox (k8s-sigs) is winning the Kubernetes API standard without a snapshot-fork engine, which is why we are building a conformance facade to be its fastest backend ([#19](https://github.com/paperclipinc/mitos/issues/19)) rather than fighting it; Kata, KubeVirt, and raw Firecracker give you the isolation primitive and leave the pool, fork, distribution, and agent-API layers as your problem.

If an alternative beats us on an axis you care about and we have no roadmap line that closes it, that is a bug in our strategy: open an issue.

## Monitoring

Node-level Prometheus metrics exposed by forkd at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `mitos_fork_duration_seconds` | histogram | Fork latency as measured by forkd |
| `mitos_active_sandboxes` | gauge | Currently running sandboxes per node |
| `mitos_memory_shared_bytes` | gauge | CoW shared memory across forks (counted once per template) |
| `mitos_memory_unique_bytes` | gauge | Per-fork unique (private-dirty) memory |
| `mitos_cow_memory_savings_bytes` | gauge | Bytes the CoW model reveals are not consumed per fork (naive minus CoW-aware) |
| `mitos_metered_disk_bytes` | gauge | CoW-aware metered disk for reflink (Snapshot) volumes |

Controller-level Prometheus metrics exposed by the controller at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `mitos_claim_pending_total` | counter | Times a claim requeued for no node with a ready snapshot |
| `mitos_orphan_sweeps_total` | counter | Orphan sandbox VMs terminated by the garbage collector |
| `mitos_claim_errors_total{pool,reason}` | counter | Terminal claim failures by pool and reason |
| `mitos_pool_ready_snapshots{pool}` | gauge | Ready snapshots per pool at the last reconcile |

Tracing: a per-claim OpenTelemetry trace (controller decision to forkd gRPC to KVM restore) exports to an OTLP collector when both the controller and forkd run with `--otlp-endpoint`; trace ids propagate across the gRPC boundary and spans carry no secrets. Audit log: forkd records a toggleable structured JSON line per exec or file operation with `--audit-log` (a path or `stderr`), logging command/path and byte counts but never content or secrets. Operators can list sandboxes and forks with the `kubectl-sandbox` plugin (`kubectl sandbox ls` / `ps`). forkd also serves the CoW-aware metering report on the operational `GET /v1/metering` endpoint. See [docs/observability.md](docs/observability.md) for the full trace model, the audit record shape, the metric catalogue, and plugin install, and [docs/metering.md](docs/metering.md) for the CoW accounting rules. End-to-end trace ids stamped across pods, logs, and Hubble remain on the roadmap ([#29](https://github.com/paperclipinc/mitos/issues/29)).

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
- `mitos` CLI: [docs/cli.md](docs/cli.md)
- MCP server: [docs/mcp.md](docs/mcp.md)
- Talos + Hetzner reference platform: [docs/platforms/talos-hetzner.md](docs/platforms/talos-hetzner.md)
- Target API surface (v2 spec): [docs/api/v2-spec.md](docs/api/v2-spec.md)
- Benchmark methodology: [BENCHMARKS.md](BENCHMARKS.md)

## Project status

Early development, pre-1.0. Do not run untrusted code with this project in production yet, and note that there has been no external security review ([docs/threat-model.md](docs/threat-model.md)). The control plane is real end-to-end (claim to running sandbox, proven in CI against mock engines and real Firecracker VMs, and exercised on a bare-metal Talos KVM cluster). Implemented and CI-proven: pod-native execution via unprivileged, PSA-restricted husk pods as the DEFAULT (the per-sandbox VM runs inside an unprivileged pod with `/dev/kvm` from a device plugin, not `privileged`), with raw-forkd under the Firecracker jailer as the fallback; OCI image to rootfs template builds; per-activation copy-on-write rootfs (each activation gets its own reflink clone of the template rootfs); mTLS plus per-sandbox token auth; LLM-legible structured errors (`code`, `cause`, `remediation`); KMS envelope encryption with crypto-shredding; RNG reseed and clock resync on fork (remaining fork-correctness items in [#3](https://github.com/paperclipinc/mitos/issues/3)); `Fresh` and `Snapshot` volume policies; content-addressed snapshot integrity, version-compatibility, and peer-to-peer distribution; default-deny IP and name-based egress; CoW-aware metering; capacity-aware scheduling; the observability surface; the MCP server; the CLI; incremental streaming exec with background processes; a stateful code interpreter (`run_code` with rich multi-MIME results and structured errors); an interactive PTY over WebSocket; a one-liner SDK with a lazy default pool, an async Python client, and durable `from_name` handles; and the Python and TypeScript SDKs. Bare-metal warm-claim activate latency (P50 ~27 ms) is published and reproducible ([BENCHMARKS.md](BENCHMARKS.md)).

Husk-default scope, verified on a real KVM cluster: warm-claim activate, blocking exec (`/v1/exec` with correct stdout and exit code), `run_code` failing closed with a clean `KernelUnavailable` (the husk base image lacks the kernel), self-heal / re-pend, and pool warming plus demand autoscaling all work end to end on the husk default. The streaming exec, code interpreter, and interactive PTY listed above are implemented and CI-proven on the engine / raw-forkd path; on the husk default, live `SandboxFork` and the streaming exec / PTY surfaces are not yet wired (live fork because the husk pod's stub, not forkd's engine, owns the source VM, tracked in [#18](https://github.com/paperclipinc/mitos/issues/18); streaming exec and PTY because the guest agent baked into the husk template snapshot predates the vsock streaming/PTY frame protocol and needs a template rebuild at the current commit, tracked in [#24](https://github.com/paperclipinc/mitos/issues/24)).

Documented follow-ups and targets, not yet shipped: live fork on the husk pod-native default ([#18](https://github.com/paperclipinc/mitos/issues/18)); the husk template guest-agent rebuild that lights up streaming exec and PTY on the husk default ([#24](https://github.com/paperclipinc/mitos/issues/24)); preview URLs / inbound port exposure into the sandbox; the code-interpreter kernel in the husk cluster base image (`run_code` is fail-closed with a clear `KernelUnavailable` until then); the durable Workspace git verbs ([#21](https://github.com/paperclipinc/mitos/issues/21)); pluggable engine tiers ([#43](https://github.com/paperclipinc/mitos/issues/43)); multi-node snapshot distribution validation ([#3](https://github.com/paperclipinc/mitos/issues/3)); a Helm chart ([#37](https://github.com/paperclipinc/mitos/issues/37)); and the remaining failure/GC items ([#12](https://github.com/paperclipinc/mitos/issues/12)). The bare-metal self-hosted CI runner ([#16](https://github.com/paperclipinc/mitos/issues/16)) is built and CI-validated; arming it as standing CI is a maintainer apply step (the registration Secret plus the manifests). The sandbox is the VM, not the husk pod: the husk pod is the VM's unprivileged host, so pod-scoped Kubernetes policies (NetworkPolicy, ResourceQuota, PSA) govern the husk pod, not the workload inside the microVM. [ROADMAP.md](ROADMAP.md) is the single source for what is done, in progress, and gated; the operating rule is that this repository never describes a system that does not exist.

Contributions welcome: see [CONTRIBUTING.md](CONTRIBUTING.md) and [CLAUDE.md](CLAUDE.md) for conventions, [SECURITY.md](SECURITY.md) for vulnerability reporting.

## License

Apache 2.0
