# Threat model

This document states what isolation `sandbox` provides today, what it intends
to provide, and the current status of every boundary. It is written against the
code in this repository, not the README. Statuses: **mitigated**, **partial**,
**open**.

**Honest summary for the current codebase: do not run untrusted code with this
project in production yet.** The KVM/Firecracker boundary is real, but almost
every defense-in-depth layer around it is open, and the control plane
(controller ↔ forkd) is not yet wired end-to-end. No external security review
has happened.

## Components and trust

| Component | Runs as | Trusts | Trusted by |
|---|---|---|---|
| Guest workload | VM guest, untrusted | nothing | nobody |
| Guest agent (`guest/agent`) | PID 1 in guest, untrusted post-exec | nothing | forkd treats its output as data only |
| forkd (`cmd/forkd`) | privileged DaemonSet pod with `/dev/kvm` | controller | controller, nodes |
| controller (`cmd/controller`) | cluster Deployment, CRD + Secrets RBAC | kube-apiserver | forkd |
| Snapshot artifacts | files under `/var/lib/agent-run` on each node | - | forkd executes them as memory images |

## 1. Guest → host escape

The primary boundary is KVM hardware virtualization via Firecracker.

| Control | Status | Detail |
|---|---|---|
| Firecracker microVM (minimal device model) | **mitigated** | Each sandbox is a separate Firecracker process with its own KVM VM (`internal/fork/engine.go`). |
| Jailer (dedicated UID, chroot, cgroup, namespaces per VM) | **open; priority zero** | forkd execs the `firecracker` binary directly (`internal/firecracker/client.go:StartVM`). No jailer, no per-VM UID, no chroot. A VMM-process compromise lands as forkd's user inside a privileged pod. This must be fixed before any claim of production isolation. |
| Seccomp on the VMM process | **open** | Firecracker ships default seccomp; we do not configure or verify the level, and without the jailer the surrounding process has no confinement. |
| CVE posture / version pinning | **partial** | CI pins Firecracker v1.15.0; there is no documented update policy or advisory tracking. |
| Guest agent as attack surface | **partial** | Agent speaks a small JSON protocol over vsock only (`guest/agent/main.go`); host side treats responses as data. A 10MB line-buffer cap exists. No fuzzing of the protocol yet. |

## 2. Guest → guest

| Control | Status | Detail |
|---|---|---|
| Separate KVM VMs per sandbox | **mitigated** | No two sandboxes share a kernel. |
| CoW page sharing side channels | **open** | All forks of a snapshot share read-only pages via `mmap(MAP_PRIVATE)` of the same mem file. Flush+Reload-style attacks across forks of the *same tenant's* snapshot are in scope to document; cross-tenant page sharing must be prevented by never sharing snapshot files across trust boundaries. Not yet enforced anywhere. |
| KSM | **open** | We must mandate KSM off on hosts (we control the reference platform). Not yet documented in any platform guide or checked by forkd at startup. |
| CPU vulnerability mitigations | **open** | Reference hosts (bare metal) must run current microcode with mitigations on; forkd should refuse or warn on `/sys/devices/system/cpu/vulnerabilities` red flags. Not implemented. |

## 3. Sandbox / forkd → cluster

forkd is the highest-value target: privileged, `/dev/kvm`, hostPath
`/var/lib/agent-run`, on every KVM node.

| Control | Status | Detail |
|---|---|---|
| controller ↔ forkd authn/authz (mTLS) | **open** | The gRPC service is now registered and served on :9090 (`internal/daemon/grpc_service.go`) with NO authentication; the controller dials with `insecure.NewCredentials()` (`internal/controller/node_registry.go`). Any pod that can reach node:9090 can request VMs, terminate sandboxes, and trigger template builds. mTLS with workload identities is required before any multi-tenant or production use. mTLS with SPIFFE-style identities or at minimum a cluster-CA client cert is required before the gRPC path ships. |
| Sandbox HTTP API (exec/files, :9091) | **open** | `/v1/exec` et al. (`internal/daemon/sandbox_api.go`) have **no authentication**. Anyone who can reach the port can exec in any sandbox on the node. Same fix as above; additionally per-sandbox capability tokens issued at claim time. |
| forkd capability minimization | **open** | DaemonSet runs `privileged: true` (`deploy/daemon/daemonset.yaml`). Required instead: only `/dev/kvm` device access, `CAP_NET_ADMIN` for tap devices when networking lands, no kubelet credentials, no service account token unless needed. |
| Blast radius documentation | **partial** | This document. A forkd compromise today = root-equivalent on the node (privileged container) plus ability to read every snapshot and secret passed to it. |

## 4. Sandbox → network

| Control | Status | Detail |
|---|---|---|
| Egress default-deny | **open (currently trivially true)** | Restored VMs have no NIC attached at all; there is no guest networking implemented. Egress is "denied" by absence, not by policy. The README previously implied configurable allowlists; that feature does not exist yet. |
| Host-side enforcement | **open (design fixed)** | When networking lands, egress policy is enforced host-side (nftables or eBPF per tap device), never in-guest. The guest can never influence its own policy. |
| DNS-based allowlists | **open (design fixed)** | Names like `api.anthropic.com:443` are only meaningful with a resolver we control: forkd runs a per-node resolver, guests get only that resolver, allowlist rules pin resolved IPs with TTL-bounded validity. Without this, attacker-controlled DNS bypasses name-based rules. We commit to the controlled-resolver design; raw IP allowlists are the fallback. |
| K8s NetworkPolicy | **n/a; be honest** | Sandboxes are not pods. NetworkPolicy does not govern them. Our egress layer is ours and is documented as ours. |

## 5. Snapshot integrity and supply chain

Snapshots are executable memory images; loading one is equivalent to running
arbitrary code at sandbox privilege.

| Control | Status | Detail |
|---|---|---|
| Content addressing (digest in CRD status) | **open** | Snapshots are bare files under `/var/lib/agent-run/templates/<id>/snapshot/`; identity is the directory name. No digests anywhere. |
| Verify-on-load | **open** | forkd loads whatever is on disk. |
| Publish authorization | **open** | Anything that can write the hostPath can plant a snapshot. Required: snapshots recorded with digest + publisher identity in pool status; forkd refuses undigested artifacts (with an explicit dev-mode escape hatch). |

## 6. Secrets

| Control | Status | Detail |
|---|---|---|
| Claim-time injection (not baked into snapshots) | **partial** | The design is right: pools snapshot before secrets exist; the controller resolves Secret refs at claim time (`sandboxclaim_controller.go:resolveSecrets`). Delivery into the guest is implemented over vsock post-restore (`internal/daemon/server.go:deliverConfig`); never via boot args, never via the FC API socket. Strict on real engines: if secrets cannot be delivered, the fork fails and the VM is reaped (a sandbox that reports Ready without its secrets is a lie). The mock engine skips delivery entirely; no guest exists. Remaining gap: resolved secret values transit the controller→forkd gRPC channel in plaintext (`ForkRequest.Secrets`); acceptable only because that channel is already documented as unauthenticated/unencrypted (§3, open); the mTLS work (issue #4) closes both. |
| Live-fork secret inheritance | **mitigated (default-deny)** | Forks of secret-holding sandboxes are rejected by the fork controller without explicit `allowSecretInheritance: true`; opt-ins are recorded as an audit condition (`sandboxfork_controller.go`). Per-fork credential reissue remains the end state (open). See `docs/fork-correctness.md` §3. |
| Controller RBAC for Secrets | **partial** | ClusterRole grants cluster-wide `get,list` on all Secrets. Must be narrowed (label-selected or per-namespace Role aggregation) before multi-tenant use. |

## 7. Multi-tenancy statement

What a namespace boundary buys you **today**: RBAC on the CRDs, and nothing
else. Pools, claims, and forks are namespace-scoped objects, but:

- Snapshots on a node are a flat directory shared by all tenants; no
  per-namespace separation, no enforcement that a claim only forks snapshots
  its namespace published. **open**
- VMs of different namespaces share nodes, host kernel, and forkd. **open**
- `dedicatedNodes: true` pool option (hard tenant separation via node
  pools/taints) is planned, not implemented. **open**

Until the above are closed, treat the whole cluster as one trust domain.

## 8. What we explicitly do NOT claim

- No pod-scoped Kubernetes mechanism (NetworkPolicy, PodSecurity, pod quotas)
  applies to sandbox VMs. Where we provide an equivalent, it is documented as
  ours.
- No external security review has been performed. The README must continue to
  say so until one has.
- Side-channel resistance between forks of the same snapshot is not claimed.

## Review gate

An external security review is required before any 1.0 (or "production-ready")
claim. Tracking: not scheduled.

## Change discipline

Any PR that moves the security surface (new listener, new privilege, new
artifact type, new cross-component call) must update this file in the same PR.
