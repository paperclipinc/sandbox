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
| forkd (`cmd/forkd`) | root DaemonSet pod with `/dev/kvm` and an explicit capability list (not `privileged`) | controller | controller, nodes |
| controller (`cmd/controller`) | cluster Deployment, CRD + Secrets RBAC | kube-apiserver | forkd |
| Snapshot artifacts | files under `/var/lib/agent-run` on each node | - | forkd executes them as memory images |

## 1. Guest → host escape

The primary boundary is KVM hardware virtualization via Firecracker.

| Control | Status | Detail |
|---|---|---|
| Firecracker microVM (minimal device model) | **mitigated** | Each sandbox is a separate Firecracker process with its own KVM VM (`internal/fork/engine.go`). |
| Jailer (dedicated UID, chroot, cgroup, namespaces per VM) | **mitigated by design; capability set pending proof in the KVM CI jailer run (issue #2 Task 5)** | forkd launches every Firecracker process through the jailer (`internal/firecracker/jailer.go`, `client.go:startJailedVM`): a dedicated uid/gid per VM from `--uid-range` (default 64000-64999; uid 0 refused), a per-VM chroot under `--chroot-base` containing only the explicitly hard-linked kernel, rootfs, and snapshot files (a traversal guard refuses anything outside the data dir and the VM workspace), and cgroup v2 attachment. Caller-supplied ids are validated at the gRPC boundary (`internal/daemon/validate.go`, `[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}`) and the launch path independently refuses ids whose jailer directories would escape the chroot base, so ids cannot traverse into root-level filesystem operations. The shipped DaemonSet sets the jailer flags; forkd fails closed on misconfiguration (nonroot, chroot base on a different filesystem from the data dir, malformed uid range). Residuals, explicitly: the direct-exec dev path remains when `--jailer` is omitted (forkd logs a loud warning; standalone sandbox-server always runs unjailed); a VMM compromise now lands as a throwaway uid in an empty chroot instead of forkd's root, but hard-linked snapshot files inside the chroot remain readable to it. |
| Seccomp on the VMM process | **partial** | The jailer-launched VMM runs Firecracker's default production seccomp filters; Firecracker installs them on all VMM threads unless explicitly disabled, and we never pass `--no-seccomp` or a custom filter. We do not verify or customize the filter level; that stays out of scope until the jailer path is proven in KVM CI. |
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

forkd is the highest-value target: root with capabilities, `/dev/kvm`,
hostPath `/var/lib/agent-run`, on every KVM node.

| Control | Status | Detail |
|---|---|---|
| controller ↔ forkd authn/authz (mTLS) | **mitigated when deployed as shipped** | The controller bootstraps an internal CA and per-identity leaf certificates as Secrets (`internal/pki`); forkd requires TLS 1.3 client certificates signed by that CA and authorizes only the `controller.agent-run` SAN via unary AND stream interceptors; per-identity EKUs prevent the forkd server cert acting as a client. Residuals, explicitly: programmatic insecure construction remains for tests and for deployments that omit the TLS flags (forkd logs a loud warning); no certificate rotation yet; the CA private key lives in a namespace Secret readable by namespace secret-readers. |
| Sandbox HTTP API (exec/files, :9091) | **mitigated** | Per-sandbox bearer tokens are minted at claim time (32-byte crypto/rand), compared in constant time, and fail closed: a sandbox with no registered token rejects everything. Tokens are delivered to clients via claim-owned Secrets, never logged and never in status. Residuals: tokens are static per sandbox (no rotation or expiry); anyone with namespace-wide Secret read can take them; standalone sandbox-server runs tokenless by explicit AllowTokenless design. |
| forkd capability minimization | **partial** | DaemonSet drops `privileged: true` for an explicit list (`deploy/daemon/daemonset.yaml`): root with ALL dropped plus `SYS_ADMIN`, `SYS_CHROOT` (chroot(2); not covered by `SYS_ADMIN`), `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETUID`, `SETGID`, `MKNOD` (each annotated with its jailer rationale; the set is pending proof in the KVM CI jailer run, issue #2 Task 5, and may be trimmed) and `NET_ADMIN` (reserved for tap devices, section 4). Residuals: `CAP_SYS_ADMIN` as root remains a wide grant; `/dev/kvm` still arrives via hostPath rather than a device plugin (W1); no kubelet credentials and no extra service account permissions, unchanged. |
| Blast radius documentation | **partial** | This document. A forkd compromise today = root in a capability-trimmed container with `CAP_SYS_ADMIN` (materially root-equivalent on the node until the W1 device-plugin work lands) plus ability to read every snapshot and secret passed to it. |

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
| Claim-time injection (not baked into snapshots) | **partial** | The design is right: pools snapshot before secrets exist; the controller resolves Secret refs at claim time (`sandboxclaim_controller.go:resolveSecrets`). Delivery into the guest is implemented over vsock post-restore (`internal/daemon/server.go:deliverConfig`); never via boot args, never via the FC API socket. Strict on real engines: if secrets cannot be delivered, the fork fails and the VM is reaped (a sandbox that reports Ready without its secrets is a lie). The mock engine skips delivery entirely; no guest exists. The same post-restore handshake also sends `NotifyForked` (32 bytes of host `crypto/rand` entropy plus a fork generation) before config; on a real engine a notify failure fails the fork and reaps the VM regardless of whether secrets were requested, because a guest that did not reseed shares CRNG state with its siblings. Entropy bytes are never logged by host or guest. Resolved secret values (`ForkRequest.Secrets`) now transit the mTLS-protected controller→forkd channel when deployed as shipped (§3); they remain plaintext on the wire only in flag-less dev deployments, where forkd warns loudly. |
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
