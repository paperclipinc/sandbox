# sandbox

Sub-millisecond sandbox forking for AI agents on Kubernetes.

`sandbox` gives your agents isolated, forkable compute environments with declarative volume management — backed by Firecracker microVMs and native k8s CRDs.

```bash
# Create a pool of 10 pre-snapshotted Python sandboxes
kubectl apply -f examples/python-pool.yaml

# Claim one (~0.8ms fork from snapshot)
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

Agent harnesses need fast, isolated environments where agents can read/write files, install packages, and run code. The existing options:

- **SaaS platforms** (E2B, Modal, Daytona) — fast but not self-hosted, your data leaves your infra
- **Zeroboot** — sub-ms forking but no k8s integration, no volume management, prototype stage
- **Agent Sandbox (k8s-sigs)** — k8s-native CRDs but no snapshot/fork story, 1-3s cold start
- **Raw Firecracker/Kata** — powerful but you build everything yourself

`sandbox` combines sub-millisecond Firecracker forking with k8s-native CRDs and per-volume fork policies. Self-hosted on any cluster with KVM support.

## Features

**Fast**
- ~0.8-2ms sandbox fork via Firecracker CoW memory mapping
- Pre-snapshotted pools — no cold start on claim
- CoW memory sharing — ~265KB per fork, not ~128MB

**Forkable**
- Fork any running sandbox into N independent copies
- Per-volume fork policies: `Fresh`, `Share`, `Clone`, `Snapshot`
- Each fork gets its own KVM-isolated microVM

**Kubernetes-native**
- Declarative CRDs: `SandboxPool`, `SandboxClaim`, `SandboxFork`
- Templates with volume topology and fork behavior
- RBAC, namespaces, resource quotas, NetworkPolicy
- Works with Karpenter, Prometheus, any k8s tooling

**Secure**
- Hardware isolation — dedicated kernel per sandbox (KVM/Firecracker)
- Default-deny network egress, configurable allowlists
- Credentials injected at claim time, not baked into snapshots
- No host filesystem access

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  k8s Control Plane                                          │
│                                                             │
│  SandboxTemplate ──► SandboxPool ──► SandboxClaim           │
│        │                  │               │                 │
│        │            (manages snapshot     (fork from         │
│        │             lifecycle)            snapshot)         │
│        ▼                  │               │                 │
│  SandboxFork              │               │                 │
│  (fork running sandbox)   │               │                 │
│                           │               │                 │
│  ┌────────────────────────┼───────────────┼──────────────┐  │
│  │  controller (Deployment)                              │  │
│  │  - reconciles CRDs                                    │  │
│  │  - manages pool lifecycle                             │  │
│  │  - handles volume fork policies                       │  │
│  │  - reports metrics                                    │  │
│  └───────────────────────────────────────────────────────┘  │
└──────────────┬──────────────────────────────────────────────┘
               │ gRPC
┌──────────────▼──────────────────────────────────────────────┐
│  Nodes (KVM-capable)                                        │
│                                                             │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  forkd (DaemonSet, one per node)                      │  │
│  │                                                       │  │
│  │  On startup:                                          │  │
│  │    - Validate /dev/kvm                                │  │
│  │    - Load snapshot templates from local storage        │  │
│  │    - Report capacity to controller                    │  │
│  │                                                       │  │
│  │  On fork request (~0.8ms):                            │  │
│  │    1. mmap(MAP_PRIVATE) snapshot memory                │  │
│  │    2. KVM_CREATE_VM + restore CPU state                │  │
│  │    3. Attach volumes per fork policy                   │  │
│  │    4. Return sandbox endpoint                         │  │
│  │                                                       │  │
│  │  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐                    │  │
│  │  │ VM  │ │ VM  │ │ VM  │ │ VM  │  ← KVM-isolated     │  │
│  │  │fork1│ │fork2│ │fork3│ │fork4│    sandboxes         │  │
│  │  └─────┘ └─────┘ └─────┘ └─────┘                    │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

## Quick start

### Prerequisites

- Kubernetes cluster with KVM-capable nodes (see [Node setup](#node-setup))
- kubectl configured
- Helm 3

### Install

```bash
helm repo add paperclip https://paperclipinc.github.io/sandbox
helm install sandbox paperclip/sandbox
```

Or from source:

```bash
kubectl apply -f deploy/crds/
kubectl apply -f deploy/controller/
kubectl apply -f deploy/daemon/
```

### Create a sandbox pool

```yaml
# pool.yaml
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
    - name: shared-data
      source:
        s3:
          bucket: my-datasets
          region: us-east-1
      readOnly: true
      forkPolicy: Share
  networkPolicy:
    egress: deny
    allow:
      - "api.openai.com:443"
      - "api.anthropic.com:443"
---
apiVersion: agentrun.dev/v1alpha1
kind: SandboxPool
metadata:
  name: python-agent-pool
spec:
  templateRef:
    name: python-agent
  replicas: 10
  snapshotAfter: Ready
```

```bash
kubectl apply -f pool.yaml

# Watch pool status
kubectl get sandboxpool python-agent-pool
# NAME                READY   SNAPSHOTS   AGE
# python-agent-pool   10      10          2m
```

### Claim a sandbox

```yaml
# claim.yaml
apiVersion: agentrun.dev/v1alpha1
kind: SandboxClaim
metadata:
  name: agent-session-1
spec:
  poolRef:
    name: python-agent-pool
  env:
    - name: SESSION_ID
      value: "abc-123"
  secrets:
    - name: openai-key
      secretRef:
        name: agent-secrets
        key: OPENAI_API_KEY
```

```bash
kubectl apply -f claim.yaml

# Sandbox is ready in <2ms
kubectl get sandboxclaim agent-session-1
# NAME              STATUS   ENDPOINT            AGE
# agent-session-1   Ready    10.0.3.42:8080      1s
```

### Fork a running sandbox

```yaml
# fork.yaml
apiVersion: agentrun.dev/v1alpha1
kind: SandboxFork
metadata:
  name: parallel-attempt
spec:
  sourceRef:
    name: agent-session-1
  replicas: 3
```

```bash
kubectl apply -f fork.yaml

# 3 independent copies of the running sandbox
kubectl get sandboxfork parallel-attempt
# NAME                FORKS   STATUS   AGE
# parallel-attempt    3       Ready    1s
```

### Python SDK

```bash
pip install agent-run
```

```python
from agent_run import AgentRun

client = AgentRun()  # uses kubeconfig or in-cluster config

# Create from pool
sandbox = client.create("python-agent-pool")

# Execute code
result = sandbox.exec("print('hello')")
print(result.stdout)   # hello
print(result.exit_code) # 0

# Read/write files
sandbox.files.write("/workspace/data.json", '{"key": "value"}')
content = sandbox.files.read("/workspace/data.json")

# Fork
forks = sandbox.fork(3)
for i, fork in enumerate(forks):
    fork.exec(f"echo 'approach {i}' > /workspace/result.txt")

# Read results from each fork
for fork in forks:
    print(fork.files.read("/workspace/result.txt"))

# Clean up
sandbox.terminate()
for fork in forks:
    fork.terminate()
```

### TypeScript SDK

```bash
npm install agent-run
```

```typescript
import { AgentRun } from 'agent-run';

const client = new AgentRun();

const sandbox = await client.create('python-agent-pool');

const result = await sandbox.exec('print(1 + 1)');
console.log(result.stdout); // 2

const [forkA, forkB] = await sandbox.fork(2);
await forkA.exec("open('/workspace/a.txt', 'w').write('plan A')");
await forkB.exec("open('/workspace/b.txt', 'w').write('plan B')");

await sandbox.terminate();
```

## Concepts

### SandboxTemplate

Defines what a sandbox looks like: base image, init commands, resource limits, volume layout with fork policies, and network policy.

### SandboxPool

Maintains a pool of pre-snapshotted sandboxes. The controller boots sandboxes from the template, waits for them to be ready (init commands complete), snapshots them via Firecracker, and optionally scales down the source pods. On `SandboxClaim`, it forks from a snapshot in <2ms.

### SandboxClaim

"Give me a sandbox from this pool." The controller picks a snapshot, forks it on a node with capacity, applies volume fork policies, injects secrets, and returns an endpoint.

### SandboxFork

"Fork this running sandbox N times." Creates N independent copies of a live sandbox's memory, CPU state, and volumes (per fork policy). Each fork is a separate KVM VM.

### Fork policies

Each volume in a template has a `forkPolicy` that controls what happens when the sandbox is forked:

| Policy | What happens | When to use |
|--------|-------------|-------------|
| `Fresh` | New empty volume | Scratch space, temp dirs |
| `Share` | Re-mount same backing store | Shared datasets (S3/GCS), model weights |
| `Snapshot` | CoW snapshot (btrfs) | Working directory — cheap, instant, independent writes |
| `Clone` | Full copy via CSI VolumeSnapshot | Persistent state needing full independence |

## Node setup

`sandbox` requires nodes with KVM support (`/dev/kvm`).

### AWS (EKS)

Use `c8i`, `m8i`, or `r8i` instances with nested virtualization. EKS managed node groups silently drop `CpuOptions` — use self-managed ASGs:

```bash
# See deploy/eks/ for full setup scripts
bash deploy/eks/setup-kvm-nodes.sh
```

### GCP (GKE)

Use `n2`, `c2`, or `c3` machine types. Enable nested virtualization on the node pool.

### Azure (AKS)

Use `Dv3`, `Ev3`, or `Dsv3` series with nested virtualization enabled.

### Bare metal

Any Linux node with KVM support works. Label the node:

```bash
kubectl label node <name> agentrun.dev/kvm=true
```

## Monitoring

Prometheus metrics exposed by forkd at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `agentrun_fork_duration_seconds` | histogram | Fork latency (P50/P99) |
| `agentrun_active_sandboxes` | gauge | Currently running sandboxes per node |
| `agentrun_pool_ready_snapshots` | gauge | Available snapshots per pool |
| `agentrun_memory_shared_bytes` | gauge | CoW shared memory across forks |
| `agentrun_memory_unique_bytes` | gauge | Per-fork unique memory (dirty pages) |

## Comparison

| | sandbox | E2B | Zeroboot | Agent Sandbox | Daytona |
|---|---|---|---|---|---|
| Fork latency | ~0.8-2ms | ~150ms | ~0.8ms | ~1-3s | ~90ms |
| Isolation | KVM microVM | Firecracker | KVM microVM | gVisor/Kata | Docker |
| k8s-native | CRDs | SaaS API | DaemonSet | CRDs | SaaS |
| Self-hosted | any k8s | OSS option | bare metal | GKE-focused | enterprise |
| Volume fork policies | per-volume | no | no | no | no |
| On-demand fork | SandboxFork | no | no | no | no |
| Python/TS SDK | yes | yes | basic | yes | yes |
| Open source | Apache 2.0 | partial | Apache 2.0 | Apache 2.0 | partial |

## Project status

Early development. The core fork engine and CRDs are being built. Contributions welcome.

## License

Apache 2.0
