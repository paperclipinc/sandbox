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
- Sandbox claims fork from pre-built memory snapshots via Firecracker CoW restore; the mechanism targets sub-10ms forks, and the reproducible benchmark program that backs every public number is in progress ([#15](https://github.com/paperclipinc/sandbox/issues/15))
- Pre-snapshotted pools: no cold start on claim
- CoW memory sharing across forks: you pay for unique pages, not for copies

**Forkable**
- Fork any running sandbox into N independent copies (`SandboxFork`)
- Per-volume fork policies: `Fresh`, `Share`, `Clone`, `Snapshot` (API defined; backends in progress, [#11](https://github.com/paperclipinc/sandbox/issues/11))
- Each fork gets its own KVM-isolated microVM
- Live forks of secret-holding sandboxes are rejected unless explicitly opted in; forks never silently inherit credentials

**Kubernetes-native**
- Declarative CRDs: `SandboxPool`, `SandboxClaim`, `SandboxFork`
- Templates with volume topology and fork behavior
- RBAC and namespace scoping today; pod-native execution (quotas, NetworkPolicy, scheduler truth) is the husk-pod workstream ([#18](https://github.com/paperclipinc/sandbox/issues/18))
- Works with Prometheus and standard k8s tooling

**Secure**
- Hardware isolation: a dedicated kernel per sandbox (KVM/Firecracker)
- Credentials injected at claim time over vsock, never baked into snapshots; undeliverable secrets fail the claim instead of lying about readiness
- Honest threat model with per-boundary status: [docs/threat-model.md](docs/threat-model.md). No external security review has happened yet, and the document says exactly what is open (jailer, mTLS, API auth)

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

```bash
go run ./cmd/sandbox-server --mock --addr :8080

cd sdk/python && pip install -e .
python -c "
from agent_run.direct import SandboxServer
s = SandboxServer('http://localhost:8080')
s.create_template('python')
sb = s.fork('python')
print(sb.exec('echo hello').stdout)
"
```

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

Python today (`sdk/python`, pre-PyPI: `pip install -e .`), TypeScript planned, MCP server planned so any MCP-speaking agent can use sandboxes with zero SDK integration ([#27](https://github.com/paperclipinc/sandbox/issues/27)). The target developer surface (three-line common path, git-shaped workspace verbs, capability-budgeted self-service for agents) is specified in [docs/api/v2-spec.md](docs/api/v2-spec.md).

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

Prometheus metrics exposed by forkd at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `agentrun_fork_duration_seconds` | histogram | Fork latency as measured by forkd |
| `agentrun_active_sandboxes` | gauge | Currently running sandboxes per node |
| `agentrun_memory_shared_bytes` | gauge | CoW shared memory across forks |
| `agentrun_memory_unique_bytes` | gauge | Per-fork unique memory at fork time |

End-to-end claim traces (controller decision to first exec) are on the observability roadmap ([#29](https://github.com/paperclipinc/sandbox/issues/29)).

## Project status

Early development, pre-1.0. The control plane is real end-to-end (claim to running sandbox, proven in CI against mock engines and real Firecracker VMs); volume backends, guest networking with egress policy, pod-native execution, and the durable Workspace are in active development in roadmap order. [ROADMAP.md](ROADMAP.md) is the single source for what is done, in progress, and gated; the operating rule is that this repository never describes a system that does not exist.

Contributions welcome: see [CONTRIBUTING.md](CONTRIBUTING.md) and [CLAUDE.md](CLAUDE.md) for conventions, [SECURITY.md](SECURITY.md) for vulnerability reporting.

## License

Apache 2.0
