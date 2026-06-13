# Talos + Hetzner AX: bare-metal provisioning runbook

This document is the end-to-end guide for running the mitos operator on
Talos Linux atop Hetzner AX dedicated servers (Robot fleet). It covers hardware
selection, OS install, machine configuration, operator deployment, KVM
readiness checks, and capacity planning pointers.

## Verification status

| Item | Status | Notes |
|------|--------|-------|
| Deploy manifests valid (kubeconform + kustomize) | CI-VERIFIED | `manifests` job in `.github/workflows/ci.yaml` |
| Talos worker-kvm patch validates (`talosctl validate --mode metal`) | CI-VERIFIED | `talos-validate` job in `.github/workflows/ci.yaml` |
| Talos control-plane patch validates | CI-VERIFIED | same job |
| kustomize base resolves (`kubectl kustomize deploy/`) | CI-VERIFIED | `manifests` job |
| Firecracker snapshot/restore on `/dev/kvm` | HARDWARE-REQUIRED | requires AX dedicated; not in shared CI |
| Per-node sandbox density | HARDWARE-REQUIRED (TARGET) | no measured numbers yet; see §Capacity planning |
| Cost per sandbox-hour | HARDWARE-REQUIRED (TARGET) | no measured numbers yet; see §Capacity planning |
| Fork-to-first-exec latency on AX hardware | HARDWARE-REQUIRED (TARGET) | shared-CI numbers in BENCHMARKS.md are not representative |
| End-to-end smoke test on a live cluster | HARDWARE-REQUIRED | steps in §Smoke test below |

---

## 1. Hardware BOM (reference example)

The following is a **reference example**, NOT a measured benchmark. Actual
density, cost, and throughput numbers are targets (see §Capacity planning);
they will be filled in when measured on the pinned reference node.

| Role | Count | Example model | Key requirement |
|------|-------|---------------|----------------|
| Control plane | 1-3 | AX41-NVMe or similar | any AX with 16 GB+ RAM |
| KVM workers | 1+ | AX41-NVMe, AX52, AX102 | CPU virtualization extensions; NVMe for snapshots |

Hardware requirements:

- **CPU**: must expose hardware virtualization (`VT-x` or `AMD-V`) to the OS.
  All current Hetzner AX dedicated servers with AMD EPYC or Intel Xeon CPUs
  meet this. Confirm with `egrep 'vmx|svm' /proc/cpuinfo` on the rescue system
  before provisioning.
- **NVMe**: Firecracker snapshots, the content-addressed chunk store, and the
  jailer chroot base all live on the forkd data volume
  (`/var/lib/mitos`). NVMe latency is on the critical path for fork
  restore time. Rotational disk is not recommended for worker nodes.
- **RAM**: forkd pins the template snapshot in memory (one copy per template
  shared across all forks via `MAP_PRIVATE` CoW). Plan for the template
  working set plus per-fork dirty pages. See §Capacity planning.
- **Network**: the robots use Hetzner's private vSwitch for cluster-internal
  traffic; a /24 or larger RFC1918 range is typical.

---

## 2. Why Hetzner Cloud will NOT work

Firecracker requires `/dev/kvm`, which means hardware virtualization extensions
must be directly visible to the guest OS. Hetzner Cloud uses a hypervisor layer
that does NOT pass through `/dev/kvm`:

- Cloud instances run inside a hypervisor. Nested KVM would be needed and is
  not offered.
- Hetzner Cloud uses gVisor-style isolation in some configurations, which does
  not emulate the KVM character device.
- The forkd DaemonSet has `nodeSelector: mitos.run/kvm: "true"` and mounts
  `/dev/kvm` as a `hostPath: CharDevice`. If `/dev/kvm` is absent, the pod
  stays `Pending`; if it mounts but KVM does not actually work, Firecracker
  fails at the `PUT /machine-config` step with `EHWPOISON` or an
  `open(/dev/kvm): permission denied` error.

**Use Hetzner Robot (dedicated/bare-metal) servers of the AX class.** The AX
series exposes CPU virtualization extensions directly to the OS. Talos is the
supported OS for this fleet.

---

## 3. Installing Talos on dedicated servers

### 3a. Get the servers into the rescue system

In Hetzner Robot, activate the rescue system (Linux x86-64) for each server
and reset it. SSH into the rescue system.

### 3b. Write the Talos installer image to the primary disk

Download the Talos bare-metal installer and write it to the system disk. The
disk is typically `/dev/sda` or `/dev/nvme0n1`.

```bash
# On the rescue system for each node.
# Check the actual disk with: lsblk
TALOS_VERSION=v1.7.6   # pin the version you intend to run
curl -Lo /tmp/talos-amd64.raw.xz \
  https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/metal-amd64.raw.xz
xz -dc /tmp/talos-amd64.raw.xz | dd of=/dev/nvme0n1 bs=4M status=progress oflag=sync
```

After the write completes, reboot the server out of rescue mode. It will PXE
or disk-boot into Talos installer mode and wait for a machine config to be
applied.

Alternatively, if your Hetzner environment supports PXE, you can boot the
Talos iPXE chainload URL directly without writing to disk; see the Talos
documentation for the `metal` platform PXE chain.

### 3c. Generate the cluster configs

From a workstation that has `talosctl` installed:

```bash
# Replace MY_CLUSTER and CONTROL_PLANE_IP with your values.
talosctl gen config my-cluster https://<CONTROL_PLANE_IP>:6443 --output-dir _out
```

This writes `_out/controlplane.yaml`, `_out/worker.yaml`, and
`_out/talosconfig`. Keep `_out/talosconfig` safe; it is the cluster admin
credential.

### 3d. Apply the KVM worker patch

The file `deploy/talos/worker-kvm.yaml` is a strategic-merge patch that adds:

- kernel modules `kvm`, `kvm_intel`, `kvm_amd`, `vhost_vsock`, `tun`
- node label `mitos.run/kvm: "true"` (required by the forkd DaemonSet
  `nodeSelector`)
- a dedicated data partition at `/var/lib/mitos` on the second NVMe
  (adjust `machine.disks[0].device` in the patch if the disk path differs on
  your hardware)

See `deploy/talos/README.md` for the rationale behind each piece.

Merge the patch onto the generated worker config:

```bash
# Merge the KVM worker patch.
talosctl machineconfig patch _out/worker.yaml \
  --patch @deploy/talos/worker-kvm.yaml \
  -o _out/worker-kvm.yaml

# Optional: merge the control-plane label patch.
talosctl machineconfig patch _out/controlplane.yaml \
  --patch @deploy/talos/controlplane.yaml \
  -o _out/controlplane-merged.yaml
```

Alternatively, pass the patch at generation time:

```bash
talosctl gen config my-cluster https://<CONTROL_PLANE_IP>:6443 \
  --config-patch-worker @deploy/talos/worker-kvm.yaml \
  --output-dir _out
```

Validate the merged worker config before applying:

```bash
talosctl validate --config _out/worker-kvm.yaml --mode metal
```

This is the same command the `talos-validate` CI job runs; a non-zero exit
means a field is malformed.

### 3e. Apply the configs to the booted nodes

Each server is now listening on its maintenance IP (displayed on the Hetzner
Robot console after the Talos boot).

```bash
# Apply to the control-plane node (use controlplane-merged.yaml if you applied
# the label patch; otherwise use the generated controlplane.yaml).
talosctl apply-config --insecure \
  --nodes <CONTROL_PLANE_IP> \
  --file _out/controlplane-merged.yaml

# Apply to each KVM worker.
talosctl apply-config --insecure \
  --nodes <WORKER_IP> \
  --file _out/worker-kvm.yaml
```

After the configs are applied, bootstrap etcd on the first control-plane node:

```bash
talosctl --talosconfig _out/talosconfig \
  --nodes <CONTROL_PLANE_IP> bootstrap
```

Wait for the cluster to come up:

```bash
talosctl --talosconfig _out/talosconfig \
  --nodes <CONTROL_PLANE_IP> health --wait-timeout=10m
```

Then retrieve the kubeconfig:

```bash
talosctl --talosconfig _out/talosconfig \
  --nodes <CONTROL_PLANE_IP> kubeconfig ./kubeconfig
export KUBECONFIG=./kubeconfig
kubectl get nodes
```

---

## 4. Verifying KVM readiness on a worker node

After the workers join the cluster, verify each worker is ready to run forkd.

### Check `/dev/kvm` is present

```bash
# Run a debug pod on the target worker.
kubectl debug node/<WORKER_NODE_NAME> -it \
  --image=busybox -- /bin/sh
# Inside the pod:
ls -la /host/dev/kvm
```

Expected: `crw-rw---- ... /host/dev/kvm` (character device).

### Check the required kernel modules are loaded

```bash
talosctl --talosconfig _out/talosconfig \
  --nodes <WORKER_IP> read /proc/modules | grep -E 'kvm|vhost_vsock|tun'
```

Expected: `kvm`, `kvm_intel` (Intel CPU) or `kvm_amd` (AMD CPU), `vhost_vsock`,
and `tun` all appear. The absent vendor module (`kvm_intel` on an AMD box or
vice versa) will not appear; that is expected.

### Check the `mitos.run/kvm=true` label

```bash
kubectl get node <WORKER_NODE_NAME> --show-labels | grep 'mitos.run/kvm'
```

The label is stamped by the `machine.nodeLabels` field in
`deploy/talos/worker-kvm.yaml`. The forkd DaemonSet will not schedule without
it.

### Check vsock availability

```bash
talosctl --talosconfig _out/talosconfig \
  --nodes <WORKER_IP> read /proc/modules | grep vhost_vsock
```

And confirm the forkd pod (once scheduled) logs no `vhost_vsock` errors on
startup.

---

## 5. Deploying the operator

### 5a. Apply the kustomize base

```bash
kubectl apply -k deploy/
```

This installs:

- the four CRDs (`SandboxTemplate`, `SandboxPool`, `SandboxClaim`, `SandboxFork`)
- the `mitos` namespace
- the `mitos-controller` ServiceAccount, ClusterRole, and ClusterRoleBinding
- the controller Deployment (two replicas, SA `mitos-controller`, probes on
  `:8081`, PKI enabled)
- the forkd DaemonSet (schedules only onto `mitos.run/kvm=true` nodes)

The DaemonSet stays `Pending` until KVM workers join and are labeled.

### 5b. PKI / mTLS bootstrap

The controller's `EnsurePKI` routine runs at startup and creates three Secrets
in the `mitos` namespace if they do not already exist:

- `mitos-ca`: the cluster CA key and certificate
- `mitos-forkd-tls`: a per-node forkd TLS certificate signed by the CA
- `mitos-controller-tls`: the controller's client certificate

The forkd DaemonSet mounts `mitos-forkd-tls` (cert + key) and `mitos-ca`
(CA cert only; the CA private key never reaches forkd). The controller uses its
own cert to dial forkd over mTLS on port 9090.

**No manual PKI steps are needed.** Verify bootstrap:

```bash
kubectl -n mitos get secret mitos-ca mitos-forkd-tls mitos-controller-tls
```

All three should be present a few seconds after the controller pod reaches
`Running`.

### 5c. Label the KVM nodes (if not already labeled by Talos)

Talos stamps `mitos.run/kvm=true` via `machine.nodeLabels` in the worker
patch, so the label should already be set. If it is missing (e.g. a node added
before the patch was applied):

```bash
kubectl label node <WORKER_NODE_NAME> mitos.run/kvm=true
```

### 5d. Verify forkd is running

```bash
kubectl -n mitos rollout status daemonset/mitos-forkd
kubectl -n mitos get pods -l app.kubernetes.io/component=forkd -o wide
```

Each pod should reach `Running` and pass its readiness probe (`GET /healthz` on
`:9091`).

---

## 6. Smoke test: create a SandboxPool, claim, and exec

### 6a. Create a SandboxTemplate and SandboxPool

Create a minimal template using the busybox OCI image. The controller's
`Engine.CreateTemplate` pulls the image, boots it in a microVM, runs
`template.Spec.Init` inside the VM, and snapshots.

```yaml
# sandbox-template.yaml
apiVersion: mitos.run/v1alpha1
kind: SandboxTemplate
metadata:
  name: busybox-basic
  namespace: mitos
spec:
  image: busybox:stable
  init: /bin/true
---
apiVersion: mitos.run/v1alpha1
kind: SandboxPool
metadata:
  name: busybox-pool
  namespace: mitos
spec:
  templateRef:
    name: busybox-basic
  size: 2
```

```bash
kubectl apply -f sandbox-template.yaml
kubectl -n mitos get sandboxpool busybox-pool -w
```

Wait for `readySnapshots: 2` (or the configured `size`) in the pool status.

### 6b. Create a SandboxClaim

```yaml
# sandbox-claim.yaml
apiVersion: mitos.run/v1alpha1
kind: SandboxClaim
metadata:
  name: smoke-test
  namespace: mitos
spec:
  poolRef:
    name: busybox-pool
```

```bash
kubectl apply -f sandbox-claim.yaml
kubectl -n mitos get sandboxclaim smoke-test -w
```

Wait for `.status.phase: Ready`.

### 6c. Run an exec via the CLI or SDK

Using the `mitos` CLI (see `docs/cli.md`):

```bash
mitos sandbox exec smoke-test -- /bin/echo hello
```

Expected output: `hello` (the command runs inside the forked microVM on the KVM
worker node).

For Python SDK usage:

```python
from mitos import SandboxClient
client = SandboxClient(kubeconfig="./kubeconfig", namespace="mitos")
result = client.exec("smoke-test", ["/bin/echo", "hello"])
print(result.stdout)  # hello
```

### 6d. Clean up

```bash
kubectl -n mitos delete sandboxclaim smoke-test
kubectl -n mitos delete sandboxpool busybox-pool
kubectl -n mitos delete sandboxtemplate busybox-basic
```

---

## 7. Capacity planning pointers

> **No density or cost numbers are stated here.** Per the project's
> no-unverified-claims rule, every number must be reproducible from `bench/`
> on pinned hardware. The items below are TARGETS; they will be updated when
> measured on the Hetzner AX reference node (ROADMAP section 4, open item:
> "bare-metal reference numbers on the Hetzner + Talos reference node").

The variables that drive per-node density:

- **Template working set size**: the Firecracker snapshot is mapped with
  `MAP_PRIVATE` and shared across all forks of that template. The shared pages
  are counted ONCE by the CoW-aware metering (see `docs/metering.md` and
  `GET /v1/metering`). Larger templates eat more RAM per node.
- **Per-fork dirty-page rate**: each fork accrues unique pages as the VM runs.
  Idle forks stay near zero unique pages; active forks diverge. The
  `mitos_cow_memory_savings_bytes` metric exposes savings vs naive
  accounting; `mitos_memory_unique_bytes` per fork shows individual divergence.
- **NVMe throughput**: fork restore time depends on reading the snapshot from
  disk. The bench harness (`cmd/bench` in `fork-exec` mode) measures
  fork-to-first-exec distribution; see `BENCHMARKS.md` for methodology and
  current shared-CI-class numbers. Bare-metal AX numbers are a target in
  ROADMAP section 4.
- **forkd data volume**: all snapshots, the chunk store, and jailer chroot
  bases live under `/var/lib/mitos` (the data partition in
  `deploy/talos/worker-kvm.yaml`). The CoW disk accounting (`mitos_metered_disk_bytes`)
  exposes the effective disk footprint per node.

References:

- `BENCHMARKS.md`: methodology and current CI-class numbers
- `docs/metering.md`: CoW-aware memory and disk accounting
- `bench/README.md`: how to reproduce the bench harness on your hardware
- ROADMAP section 4: open item for pinned bare-metal measurements

---

## 8. Cross-references

| Topic | Document |
|-------|---------|
| Talos machine config patches and rationale | `deploy/talos/README.md` |
| Deploy manifests (kustomize base) | `deploy/kustomization.yaml` |
| CLI reference (`mitos sandbox`, `mitos dev`) | `docs/cli.md` |
| Guest networking and egress allowlists | `docs/networking.md` |
| Encryption at rest (`--enable-encryption`) | `docs/encryption.md` |
| CoW-aware metering (`GET /v1/metering`) | `docs/metering.md` |
| Benchmark methodology and results | `BENCHMARKS.md`, `bench/README.md` |
| Threat model and security surface | `docs/threat-model.md` |
| Snapshot version-compatibility contract | `docs/snapshot-format.md` |
