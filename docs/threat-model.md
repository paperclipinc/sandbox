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
| Guest agent (`guest/agent`) | PID 1 in guest, untrusted post-exec | nothing | forkd / husk stub treat its output as data only |
| husk pod stub (`cmd/husk-stub`), the DEFAULT runner | unprivileged pod, `/dev/kvm` via device plugin (not `privileged`), drop ALL caps, `seccomp: RuntimeDefault`, read-only snapshot mount | controller (mTLS control channel) | controller |
| forkd (`cmd/forkd`), the snapshot BUILDER and the raw-forkd fallback | root DaemonSet pod with `/dev/kvm` and an explicit capability list (not `privileged`) | controller | controller, nodes |
| controller (`cmd/controller`) | cluster Deployment, CRD + Secrets RBAC | kube-apiserver | forkd, husk pods |
| Snapshot artifacts | files under `/var/lib/agent-run` on each node | - | forkd builds them; husk pods mount and execute them as memory images |

## 0. Default execution surface: the unprivileged husk pod (issue #18, slice 3)

Pod-native execution is now the DEFAULT (the controller runs
`--enable-husk-pods` by default). This is a deliberate change to the threat
surface, recorded here; the FULL re-derivation for the unprivileged-stub escape
surface is a later slice (#18), not done in this change.

The build-vs-run split moves the per-sandbox EXECUTION surface from a privileged
process to an unprivileged one:

- **Old default (raw-forkd, now the fallback behind `--enable-raw-forkd`).** A
  sandbox VM was forked by forkd: a root DaemonSet with `/dev/kvm`, a broad
  capability set, and a hostPath to the node data dir. The per-sandbox execution
  surface was that privileged process.
- **New default (husk pods).** A sandbox VM runs inside an UNPRIVILEGED husk pod:
  `privileged: false`, `allowPrivilegeEscalation: false`, drop ALL capabilities,
  `seccompProfile: RuntimeDefault`, `/dev/kvm` injected by the device plugin
  (not a hostPath or privilege), and the template snapshot mounted READ-ONLY. The
  one documented exception is `runAsNonRoot: false` (section 1). A VMM escape
  from a husk pod lands in an unprivileged pod, not in forkd's root, and the pod
  is governed by ordinary pod mechanisms (the scheduler, cgroups, the pod netns).
- **forkd-the-builder stays privileged.** Building a template snapshot still
  needs `/dev/kvm` and the jailer, so forkd remains the privileged BUILDER on the
  KVM nodes (and the `--enable-raw-forkd` fork-per-claim engine). The privileged
  surface is now confined to the BUILD path, run once per node per template,
  rather than every sandbox execution.

NEW boundaries this default introduces, to be fully derived in the dedicated
slice: the unprivileged stub activating a VM in place (a new escape surface
distinct from forkd's), the mTLS control channel that delivers tenant secrets
into the pod (`internal/husk` `ServeTLS`, authorized to the controller
identity; covered for transport/auth in section 3 and `docs/husk-pods.md`
section 6b), and the read-only node snapshot mount shared across husk pods on a
node. The device-plugin surface itself is in section 3.

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
| Sandbox HTTP API (exec/files, :9091) | **mitigated** | Per-sandbox bearer tokens are minted at claim time (32-byte crypto/rand), compared in constant time, and fail closed: a sandbox with no registered token rejects everything. Tokens are delivered to clients via claim-owned Secrets, never logged and never in status. On the husk-pod path (#18, slice 2, `--enable-husk-pods`) the SAME `internal/daemon` `SandboxAPI` and bearer-token gate runs IN the pod: after activation the husk stub registers the activated VM + the per-sandbox token and serves the gated exec/files API on the sandbox port, so `Status.Endpoint` (podIP:port) is reachable only with the token. The token is delivered to the stub over the mTLS control channel (the same channel as the activate secrets; never logged, never in argv), so it never crosses an unauthenticated wire. Residuals: tokens are static per sandbox (no rotation or expiry); anyone with namespace-wide Secret read can take them; standalone sandbox-server runs tokenless by explicit AllowTokenless design. |
| forkd capability minimization | **partial** | DaemonSet drops `privileged: true` for an explicit list (`deploy/daemon/daemonset.yaml`): root with ALL dropped plus `SYS_ADMIN`, `SYS_CHROOT` (chroot(2); not covered by `SYS_ADMIN`), `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETUID`, `SETGID`, `MKNOD` (each annotated with its jailer rationale; the set is pending proof in the KVM CI jailer run, issue #2 Task 5, and may be trimmed) and `NET_ADMIN` (reserved for tap devices, section 4). Residuals: `CAP_SYS_ADMIN` as root remains a wide grant; `/dev/kvm` still arrives via hostPath rather than a device plugin (W1); no kubelet credentials and no extra service account permissions, unchanged. |
| Blast radius documentation | **partial** | This document. A forkd compromise today = root in a capability-trimmed container with `CAP_SYS_ADMIN` (materially root-equivalent on the node until the W1 device-plugin work lands) plus ability to read every snapshot and secret passed to it. |

## 4. Sandbox → network

See `docs/networking.md` for the full design (tap-per-sandbox, nftables
dispatch model, per-fork identity). Networking is opt-in: with forkd's
`--enable-networking` off, restored VMs have no NIC and egress is denied by
absence. With it on, each fork gets its own tap and a host-side default-deny
egress ruleset.

| Control | Status | Detail |
|---|---|---|
| Egress default-deny (IP:port) | **partial / mitigated** | Enforced host-side for literal IP:port allowlist entries. Each fork sits on its own tap with its own /30; a shared `inet` nftables table dispatches by inbound interface (the tap) into that sandbox's regular chain, which accepts established/related, the allowlisted `ip daddr/tcp dport` pairs (each pinned to the sandbox's `ip saddr` as anti-spoof), and ends in a terminal drop. The guest cannot influence the host ruleset and cannot spoof another sandbox's source address onto its own tap. Proven in KVM CI: one VM reaches an allowed destination and is blocked from a denied one, plus a two-sandbox `nft` install proving cross-tap isolation (one sandbox's drop never kills another's allowed traffic). |
| Host-side enforcement | **enforced** | Egress policy is rendered and applied host-side only (`nft` per tap), never in-guest. The guest agent never edits nftables; the guest's only network config is its own eth0 address. |
| DNS-based allowlists (name egress) | **partial / enforced** | Names like `api.anthropic.com:443` are now enforced through a controlled per-node resolver (`internal/dnsproxy`, #47, behind `--enable-dns-egress`). The guest's only resolver is the node resolver IP (`169.254.1.1`, written into the guest `/etc/resolv.conf`). The proxy resolves ONLY names on that sandbox's allowlist, and for each resolved A record pins `(ip . port)` into that sandbox's nftables timeout set; the guest can then reach exactly the address it resolved, for exactly the allowed ports, for `max(recordTTL, 30s)`. A name not on the allowlist gets REFUSED and nothing is pinned. Exact-match FQDNs only in v1; A/IPv4 only (AAAA returns empty NOERROR). Proven in KVM CI: a resolved allowlisted name:port is reachable while an unlisted name (refused), the right name on a wrong port, and an un-resolved direct IP are all blocked. Residual risks are the next four rows. Literal IP:port rules remain the statically enforced path. |
| Name egress: upstream-resolver trust | **open (documented)** | The controlled proxy forwards allowed queries to a configured upstream (`--dns-upstream`, default the host resolver or `1.1.1.1:53`) and pins whatever A records it returns. A malicious or compromised upstream can answer an allowlisted name with an attacker-controlled IP, which the proxy will then pin and the guest will reach. The trust boundary is the upstream resolver. Mitigations not yet in v1: DNSSEC validation, a pinned/known-good resolver set, response-IP sanity checks. |
| Name egress: bounded TTL window | **partial / mitigated** | A pinned `(ip . port)` stays reachable for `max(recordTTL, 30s)` after it is resolved, even if the name later stops resolving to that IP. The window is bounded by the record TTL (floored at 30s so a very short TTL cannot expire the pin before the guest connects) and the set's timeout, after which the element is evicted and the IP is no longer reachable unless re-resolved. There is no manual revocation of a live pin before its timeout. |
| Name egress: shared-CDN-IP caveat | **open (documented)** | Pinning is by IP after resolution, so if an allowlisted name and a denied name resolve to the SAME IP (a shared CDN or load-balancer address), resolving the allowlisted name pins that IP and makes it reachable on the allowed port, including for traffic the operator intended to deny that happens to share the address. The denied NAME is still refused at the resolver (it is never answered or pinned), but the IP it shares becomes reachable once the allowlisted name is resolved. This is inherent to IP-level enforcement of name policy. |
| Name egress: DoH/DoT and DNS tunneling | **mitigated** | A guest cannot bypass the controlled resolver. Only `udp/tcp 53` to the resolver IP is permitted by the egress chain, so a guest cannot reach an arbitrary external DoH/DoT server (its `IP:port` is not allowlisted and was never pinned). The resolver answers only A queries for allowlisted names and REFUSES every other qtype (AAAA returns empty NOERROR), so it cannot be used as a covert DNS tunnel: non-A/AAAA records are not forwarded and resolved IPs are constrained to the allowlist. |
| Name egress: source attribution | **enforced** | The proxy attributes each query to a sandbox by the query's source guest IP (each sandbox has a unique /30 from the identity allocator) and pins into THAT sandbox's set. A guest cannot grant itself another sandbox's reach by spoofing a source IP: the per-tap dispatch sends a tap's traffic only into its own chain, and every accept (including the dynamic pin-set accept) is `ip saddr`-pinned to the sandbox's guest IP, so a spoofed-source query cannot land a pin that the spoofing guest can use. A query whose source has no live tap mapping is REFUSED and pins nothing. |
| Layering: host netns vs per-VM netns | **host-netns today** | The tap and nftables ruleset live in forkd's (the host's) network namespace; isolation between sandboxes is by per-tap dispatch + per-/30 addressing + saddr anti-spoof, not by a kernel netns boundary per VM. Moving each VM into its own pod netns (husk pods, #18) adds a second, defense-in-depth layer and is where snapshot-fork-under-netns is resolved. Live-fork (`ForkRunning`) of a networked sandbox fails closed today (#18): a live fork would restore the source's baked NIC and collide on tap/MAC/IP. |
| K8s NetworkPolicy | **n/a; be honest** | Sandboxes are not pods. NetworkPolicy does not govern them. Our egress layer is ours and is documented as ours. |

## 5. Snapshot integrity and supply chain

Snapshots are executable memory images; loading one is equivalent to running
arbitrary code at sandbox privilege.

| Control | Status | Detail |
|---|---|---|
| Content addressing (digest in CRD status) | **mitigated** | Every template snapshot is content-addressed in a CAS store the moment it is built: its sha256 manifest digest is recorded to `<dataDir>/templates/<id>/manifest.digest`, pinned in the store, reported through forkd `GetCapacity`/`CreateTemplate`, and written to `SandboxPoolStatus.TemplateDigest` so the snapshot identity is visible in `kubectl get sandboxpool -o yaml`. The digest is a content address and is safe to log. |
| Verify-on-load | **mitigated** | forkd verifies a snapshot's on-disk bytes against the recorded digest before it is forked, and refuses on mismatch. To keep the fork hot path cheap, verification is verify-once-at-registration: at build time (trusted, marker written without re-hash) or at first use after a restart (lazy re-hash), recorded by a `verified` marker that Fork only stats. The dev-mode escape `--allow-unverified-snapshots` downgrades a failed verification to a loud one-time warning. Residual: verification is at registration, not per fork, so tampering AFTER a snapshot is verified is not re-detected until the marker is cleared; external snapshot import is not yet supported. |
| Publish authorization | **mitigated** | Snapshots are produced only by forkd's own `CreateTemplate`, which is reachable solely over the mTLS-gated gRPC surface from the controller (PR #41). Externally supplied snapshots are not accepted, so the publish surface is exactly that authenticated `CreateTemplate` call. External snapshot import is future work. |
| Compatibility verification (no unsafe restore) | **mitigated** | The same load gate also runs the snapshot compatibility contract (`internal/snapcompat.Check`, issue #32) after the digest verify and before any Firecracker launch. The manifest records the producing environment (snapshot format version, Firecracker version, CPU model, kernel, config hash) as part of the content-addressed digest, so these fields cannot be tampered with or downgraded without changing the digest and failing the verify-on-load step above. A benign mismatch (a snapshot legitimately built under a different Firecracker version, a different CPU model, or an unsupported format version) fails closed: the restore is refused with an actionable error rather than crashing or silently corrupting a guest. The dev-mode escape `--allow-incompatible-snapshots` downgrades a refusal to a loud warning. Kernel mismatch is informational. Residual: cross-CPU-model restore via Firecracker CPU templates and live cross-Firecracker-version restore are out of scope (the contract refuses them today). |
| Encryption at rest + crypto-shredding (#31) | **mitigated** | Behind `--enable-encryption` (default off) each scope (a template now; a workspace when #21 lands) gets its own LUKS2 container (`internal/storecrypt`) backed by a sparse image; the snapshot and volumes are built inside the mounted, decrypted container, so the bytes at rest in `<scope>.img` are ciphertext, not the plaintext snapshot. dm-crypt sits below the page cache, so the mem mmap CoW restore reads decrypted pages and CoW page sharing across forks is preserved (no per-fork decryption copy). Erasure is crypto-shredding: `luksErase` wipes the LUKS keyslots and the image is removed at template delete, after which the ciphertext is unrecoverable even with the key. The key reaches cryptsetup only on stdin (`--key-file -`), never in argv or any log; `storecrypt.Key` redacts itself on any format. Proven in KVM CI on real cryptsetup: the marker is absent in the raw image but present in the decrypted mount (ciphertext at rest), reopen+read returns it intact (decrypt/restore works), and after shred a reopen with the original key fails and the image is gone (unrecoverable). Key custody (PR2): the controller generates a per-template 256-bit key with `crypto/rand`, stores it in a `<template>-enc-key` Kubernetes Secret owner-referenced to the `SandboxTemplate` (so GC of the template GCs the Secret), and delivers the key to forkd in the mTLS-protected `CreateTemplate` and `Fork` gRPC requests. forkd holds the key in process memory only via `RequestKeyProvider` and NEVER writes it to the node data disk; encryption enabled with no delivered key fails closed. The mTLS channel is now ENFORCED, not merely used: forkd refuses to start with `--enable-encryption` unless its TLS cert/key/CA flags are set, and the controller refuses to deliver the key to a node whose connection is not mTLS (it fails the encrypted build/fork for that node rather than transmit the key in cleartext), so the key cannot travel an insecure gRPC channel. The key is never logged anywhere in the key-custody code path (enforced by grep in CI). Proven by envtest and unit tests: Secret lifecycle (create + idempotent read + GC via owner ref), key-over-RPC, key-not-on-disk, fail-closed, and key-never-logged. See docs/encryption.md. Residuals, explicitly: (1) etcd-at-rest-encryption trust: the Secret data is plaintext in etcd unless the cluster operator configures KMS-backed EncryptionConfiguration; that is the operator's responsibility and is stated as an assumption. (2) Controller trust: a compromised controller can read the Secret and deliver the key to any forkd; the cluster admin boundary is the trust anchor. (3) Node-memory dump while open: while a container is open the key is necessarily in forkd process memory; a root attacker with a memory dump recovers it; zeroize-on-close is the current mitigation, full HSM custody is the follow-up. (4) TEARDOWN BOUNDARY: the controller does not yet send a DeleteTemplate RPC on SandboxTemplate deletion, so the node-side container is GC'd by node data dir lifecycle rather than a controller-driven crypto-shred; tracked as follow-up. Out of scope for now: KMS/HSM envelope encryption, key rotation/re-encryption, per-workspace scope (#21), encrypting the CAS chunk store. |

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
