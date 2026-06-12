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

## 0. Default execution surface: the unprivileged husk pod (issue #18)

Pod-native execution is now the DEFAULT (the controller runs
`--enable-husk-pods` by default; `--enable-raw-forkd` and `--mock` select the
fork-per-claim fallback). This is a deliberate change to the per-sandbox
execution surface. This section is the FULL re-derivation for the
unprivileged-stub escape surface (issue #18); it re-derives the surface boundary
by boundary, states which CI-proven mechanism backs each claim, and names every
residual. It does not contradict the per-row sections below; it reconciles with
them (the networking egress row in section 4, the encryption/key-custody row in
section 5, the sandbox-API row in section 3) and points at each.

The build-vs-run split is the core idea: a SNAPSHOT is BUILT once per node by a
privileged process and RUN many times by unprivileged pods.

- **Old default (raw-forkd, now the fallback behind `--enable-raw-forkd`).** A
  sandbox VM was forked by forkd: a root DaemonSet with `/dev/kvm`, an explicit
  capability set (`CAP_SYS_ADMIN`, `SYS_CHROOT`, and others; section 3), and a
  hostPath to the node data dir. The per-sandbox EXECUTION surface WAS that
  privileged process.
- **New default (husk pods).** A sandbox VM runs inside an UNPRIVILEGED husk pod:
  `privileged: false`, `allowPrivilegeEscalation: false`, drop ALL capabilities,
  `seccompProfile: RuntimeDefault`, `/dev/kvm` injected by the device plugin
  (not a hostPath or privilege), and the template snapshot mounted READ-ONLY. The
  two documented exceptions are `runAsNonRoot: false` (the device exception) and
  the read-only snapshot hostPath (surfaces 1 and 3 below).
- **forkd-the-builder stays privileged.** Building a template snapshot still
  needs `/dev/kvm` and the jailer, so forkd remains the privileged BUILDER on the
  KVM nodes (and the `--enable-raw-forkd` fork-per-claim engine). The privileged
  surface is now confined to the BUILD path, run once per node per template,
  rather than every sandbox execution.

### Unprivileged-stub escape surface (issue #18 re-derivation)

The honest framing up front: the per-sandbox EXECUTION surface is strictly
improved (an unprivileged, capability-dropped, restricted-minus-two,
pod-netns-governed container instead of a root process), while the INHERENT
microVM-host-escape risk (a KVM or host-kernel bug reachable from any
`/dev/kvm`-holder) is UNCHANGED, and forkd-the-builder remains a smaller
privileged control-plane surface (run once per node per template, not per
sandbox). "Provably better" is argued PER SURFACE below and tallied honestly; it
is NOT claimed globally, because the `/dev/kvm`-and-kernel axis is EQUAL, not
better.

**Surface 1: GUEST -> HUSK-STUB CONTAINER (the post-VMM-escape blast radius).**
A guest that breaks out of the microVM (a Firecracker or KVM escape) lands in the
process that hosts the VMM. Under the old default that process was forkd: ROOT,
with `/dev/kvm`, an explicit capability set including `CAP_SYS_ADMIN`, and a
hostPath to the node data dir (section 3). Under the husk default that process is
the husk stub inside an UNPRIVILEGED pod whose securityContext
(`internal/controller/huskpod.go`, proven in `internal/controller/huskpod_test.go`
at the object level and against the v1.31 PodSecurity admission plugin on the
`kind-e2e-husk` job, slice 4, conformance criterion 4) sets, each control
load-bearing:

- `privileged: false`,
- `allowPrivilegeEscalation: false` (no setuid path regains privilege),
- `capabilities.drop: [ALL]`, none added back,
- `seccompProfile: RuntimeDefault` at BOTH the pod and the container level,
- the ONLY host mount is the READ-ONLY snapshot dir plus the read-only kernel
  file (surface 3),
- the ONLY device is `/dev/kvm` (and `/dev/net/tun`) via the device plugin, not a
  hostPath or privilege (surface 4).

This securityContext satisfies EVERY PSA `restricted` control; the husk pod is
kept out of a `restricted` namespace by EXACTLY two documented exceptions, both
intrinsic to the model: the read-only snapshot hostPath, and `runAsNonRoot: false`
(uid 0 so Firecracker can open the injected `/dev/kvm` without `privileged`). The
SAME securityContext minus those two exceptions IS admitted into a restricted
namespace, and a genuinely privileged pod IS rejected in the same namespace
(PSA is enforcing); both are asserted on `kind-e2e-husk` (slice 4, section 6e of
`docs/husk-pods.md`).

This is the core "provably better" claim, and it is bounded to THIS surface: a
guest that escapes the microVM lands with NO root authority, NO Linux
capabilities, NO privilege-escalation path, NO broad host filesystem, only a
read-only base-image mount and the pod's own netns and cgroup, instead of forkd's
root with `CAP_SYS_ADMIN` and host data-dir access. The post-escape blast radius
is strictly smaller. What this does NOT change is whether the guest can reach the
host kernel through `/dev/kvm` in the first place (surface 4).

**Surface 2: the CONTROL CHANNEL (activation + secret delivery).** Activating a
husk pod delivers the tenant's claim-time env, secrets, and the per-sandbox
bearer token into the pod. The channel is mTLS, `RequireAndVerifyClientCert`,
authorized to the controller identity ONLY: `internal/husk.ServeTLS` plus
`AuthorizeControllerIdentity` accept a connection only when the VERIFIED mTLS peer
(read from `VerifiedChains`, never from a merely-presented cert) carries the
`pki.ControllerName` SAN, and a nil TLS config or nil authorize hook is refused
(fail-closed: an unauthenticated activate channel that delivers secrets is
rejected before any request is read). CI-proven: the KVM husk network-activation
phase asserts a WRONG-CA controller cert is REJECTED by the mTLS gate before any
secret is read (slice 2, section 6b of `docs/husk-pods.md`). So an in-cluster
actor cannot activate or hijack a husk pod, or inject secrets into one. Residual:
a compromised CONTROLLER can activate any husk pod and deliver secrets to it. The
controller is the trust anchor here, the same anchor as in the raw-forkd model
and the encryption key custody (section 5); this is not a regression, it is the
same boundary.

**Surface 3: the READ-ONLY SNAPSHOT HOSTPATH.** The node template snapshot is
mounted READ-ONLY into the husk pod (`huskpod.go`: the snapshot hostPath and the
kernel file are both `ReadOnly: true`). The husk stub RE-VERIFIES the snapshot ON
ACTIVATE, before it loads it, applying the SAME fail-closed gate as raw-forkd:
the stub decodes the mounted CAS manifest, binds it to the controller-passed
recorded digest (`husk.ActivateRequest.ExpectedDigest`, fed from the
NodeRegistry's forkd-reported `TemplateDigests`), re-hashes the loaded
mem+vmstate against it (a sha256 digest verify, #9), and runs
`internal/snapcompat.Check` against THIS node's detected environment (#32), all
in `internal/husk` `verifySnapshot` (the production verifier, shared with the
fork path via the `internal/cas` chunk/hash primitives so the two cannot drift).
Both checks fail closed: a snapshot tampered on the node disk after forkd's
build-time verification, or one incompatible with this node, is REFUSED on the
husk path too and never loaded into the VM (proven by `internal/husk`
`TestActivateVerifyRefusesTamperedSnapshot` and
`TestActivateVerifyRefusesIncompatibleSnapshot`). So the husk path is no longer a
verify gap relative to raw-forkd: it is the same digest + snapcompat gate.
Residual, stated honestly: all husk pods on a node share the SAME read-only
snapshot dir. This IS a shared read-only host mount and is one of the two
documented PSA-restricted exceptions (the hostPath, surface 1). It is acceptable
because (a) it is READ-ONLY: a husk pod cannot WRITE it, so it cannot tamper with
the base image another pod loads; (b) it is integrity-verified and
content-addressed, and the husk stub re-checks the digest + compatibility on EACH
activate before loading, so a tampered-on-disk or incompatible snapshot is
refused at activate time on the husk path; and (c) it is a
BASE IMAGE, not tenant data: tenant secrets are delivered post-restore over the
control channel (surface 2), never baked into the shared snapshot (section 6).
Cross-pod isolation of the mount is the read-only property, not a per-pod copy;
fully pod-native snapshot delivery (a CAS pull into the pod, removing the shared
hostPath) is a documented follow-up.

**Surface 4: the DEVICE `/dev/kvm`.** KVM access is injected by the device plugin
(`cmd/kvm-device-plugin`, `internal/deviceplugin`): the pod requests
`agentrun.dev/kvm` like any extended resource and the kubelet bind-mounts
`/dev/kvm` (and `/dev/net/tun`) on `Allocate`, so the pod sets NO `privileged:
true` and carries NO `/dev/kvm` hostPath. CI-proven: the `kind-e2e` job drives
the full advertise -> schedule -> inject path with a NON-privileged probe pod
(`privileged: false`, escalation false, drop ALL, read-only rootfs) and
`kubectl exec` confirms `/dev/kvm` is present inside the container, coming
entirely from `Allocate`, not from any privilege (section 5 of
`docs/husk-pods.md`). Residual, stated honestly: `/dev/kvm` IS exposed to the
pod, so a KVM-or-host-kernel escape from the VMM is STILL a host-escape vector.
The device plugin removes the PRIVILEGED requirement, NOT the `/dev/kvm` attack
surface itself; that surface is inherent to ANY Firecracker host and is UNCHANGED
between raw-forkd and husk. This is the axis on which the two models are EQUAL,
not better. The device-plugin DaemonSet has its own small surface
(`deploy/device-plugin/daemonset.yaml`): it runs as root because the kubelet
device-plugins dir is root-owned, but it is `privileged: false`,
`allowPrivilegeEscalation: false`, ALL capabilities dropped, and
`readOnlyRootFilesystem: true`; its only host access is the kubelet
device-plugins dir (to serve and register its socket) and a read-only `/dev`
(to `stat /dev/kvm`); it creates NO device nodes and starts NO VMs.

**Surface 5: the POD NETNS (egress).** In the husk default the VM's tap lives
inside the HUSK POD's network namespace, so the sandbox's traffic IS the pod's
traffic, governed by a Kubernetes `NetworkPolicy` (or Cilium) selecting the husk
pod (`podSelector` on `agentrun.dev/husk=true`); the bespoke host-nftables engine
is REDUNDANT here and is NOT installed for husk pods. In raw-forkd mode there is
no pod and the bespoke default-deny per-tap nftables allowlist plus the
controlled DNS proxy ARE the enforcement (section 4). Per-mode, not both: exactly
ONE layer governs a given sandbox, decided by the run mode (section 6d of
`docs/husk-pods.md`). CI-proven object-level: a `NetworkPolicy` with the husk
`podSelector` exists and SELECTS the husk pod on `kind-e2e-husk` (slice 4,
conformance criterion 3). Residual, stated honestly: the IN-VM enforcement of a
NetworkPolicy over the VM tap (the CNI actually dropping the VM's egress) is
proven only on a KVM-capable kubelet running the husk pod's VMM (a bare-metal
reference node); on the shared kind runner the nested VMM does not reliably come
up, so only the OBJECT-level attach is proven there. The raw-forkd nftables
egress IS CI-proven in-VM on KVM (section 4).

**Surface 6: the IN-POD SANDBOX API (exec and files).** After activation the husk
stub serves the SAME `internal/daemon.SandboxAPI` forkd serves, IN the pod, on
the sandbox port; the claim's `Status.Endpoint` is `podIP:sandboxPort`. Every
exec/files request is gated by the per-sandbox bearer token (32-byte
`crypto/rand`, constant-time compare, fail-closed: a sandbox with no registered
token rejects everything). The token is delivered to the stub over the mTLS
control channel (surface 2), never logged, never in argv. CI-proven: a tokened
HTTP exec reaches the guest over vsock and an UNTOKENED or WRONG-TOKEN request is
rejected with the token value absent from host-side logs (slice 2,
`internal/husk` `TestActivateServesTokenGatedSandboxAPI`, and the KVM
network-activation phase over the real endpoint; section 3 row below). Residual:
tokens are static per sandbox (no rotation or expiry); anyone with namespace-wide
Secret read can take them (section 3).

**Surface 7: the ENCRYPTION KEY (#31 PR2).** When `--enable-encryption` is on,
the per-template 256-bit key reaches the node ONLY over the mTLS control channel
(the `CreateTemplate`/`Fork` gRPC requests; the controller refuses to deliver the
key to a node whose connection is not mTLS, and forkd refuses to start encrypted
without its TLS flags), is held in node process memory while a container is open,
and is NEVER written to the node data disk (section 5: `RequestKeyProvider`,
key-not-on-disk proven by unit and envtest, key-never-logged enforced by grep in
CI). On the HUSK path the key reaches FORKD (the builder) ONLY, over the same
mTLS gRPC; forkd uses it to open the per-template LUKS container and the snapshot
is decrypted BELOW the page cache by forkd's `dm-crypt` mount. The husk pod mounts
that mount's PRE-DECRYPTED snapshot bytes read-only and NEVER receives the
encryption key: the key does not cross the controller-to-husk mTLS control channel
and is never present in the husk pod's address space. So a compromised husk pod
cannot exfiltrate the template key. Residual, stated honestly: the IN-MEMORY KEY
WINDOW on the FORKD process. While a container is open the key is necessarily in
forkd's process memory; a root attacker with a node-memory dump of FORKD while a
container is open recovers it. Zeroize-on-close is the current mitigation;
HSM/envelope custody is the follow-up.

**Surface 8: EVICTION and DRAIN (slice 4b).** A husk pod is an ordinary pod, so
it is subject to drain, eviction, preemption, and delete. A `policy/v1`
PodDisruptionBudget (`<pool>-husk`, `minAvailable = max(1, Replicas-1)`) BOUNDS
voluntary disruption to at most one warm slot at a time; a lost husk pod
re-pends the claim (Phase Pending, endpoint cleared) and the warm pool self-heals
a replacement; a `drainPolicy` governs an active sandbox (Kill re-pends,
Checkpoint snapshots the live VM first where the VMM still runs). CI-proven
object-level on `kind-e2e-husk` (slice 4b, section 6f of `docs/husk-pods.md`).
This is an AVAILABILITY surface, not a new ESCAPE surface: a drained or evicted
husk pod is gone, not escalated. The honest availability note vs the old model:
raw-forkd's VMs were not pods and did not feel drains, but they also had no
bounded, self-healing disruption story; the husk model trades that for ordinary,
self-healing pod disruption with a documented budget. The live-VM
Checkpoint-on-drain actually SURVIVING end to end is bare-metal work (it needs
the VMM running in the husk pod on a KVM-capable kubelet).

### Per-axis tally: old forkd vs husk pod

This compares the per-sandbox EXECUTION surface. forkd-the-builder (the privileged
snapshot builder, run once per node per template) is NOT a per-sandbox surface and
is discussed separately below the table.

| Axis | Old forkd (raw-forkd) | Husk pod | Verdict |
|---|---|---|---|
| Privilege | root, `privileged` dropped for an explicit cap list | `privileged: false`, `runAsNonRoot: false` (one of the two PSA-restricted exceptions, the `/dev/kvm` device one; the other is the read-only snapshot hostPath), no escalation | husk BETTER |
| Capabilities | explicit set incl. `CAP_SYS_ADMIN`, `SYS_CHROOT` | `drop: [ALL]`, none added | husk BETTER |
| Host FS access | hostPath to the node data dir (RW) | READ-ONLY node hostPaths only: the snapshot mount, the kernel file, and (when verify is enforced) the CAS manifest the stub checks the snapshot against; all read-only | husk BETTER |
| Device access (`/dev/kvm` + kernel) | `/dev/kvm` via hostPath | `/dev/kvm` via device plugin (no privilege) | EQUAL on the inherent KVM/kernel escape surface; husk removes only the privileged REQUIREMENT, not the device surface |
| Network governance | host-nftables in forkd's netns | pod netns governed by NetworkPolicy/Cilium (object-level proven; in-VM bare-metal) | husk BETTER (defense-in-depth pod netns); in-VM enforcement bare-metal-pending |
| Secret + key delivery | mTLS gRPC to forkd | tenant secrets + token over the mTLS control channel to the pod (controller-identity authz, never on disk); the per-template ENCRYPTION KEY never reaches the husk pod at all (it goes to forkd, which serves the pre-decrypted snapshot via dm-crypt) | EQUAL/BETTER (same mTLS anchor; enc key never enters the husk pod, in-memory-window residual is on forkd only) |

Honest conclusion: on the privilege, capabilities, host-FS, and network axes the
husk model is BETTER, with the residuals named (shared read-only snapshot mount,
in-memory key window, NetworkPolicy in-VM enforcement proven only on bare metal).
On the inherent `/dev/kvm`-and-kernel axis the two are EQUAL: a KVM or host-kernel
bug reachable from a `/dev/kvm`-holder is the same risk in both models, and the
device plugin removes the privileged requirement, NOT that attack surface. The
per-sandbox EXECUTION surface is therefore STRICTLY IMPROVED, while the inherent
microVM-host-escape risk is UNCHANGED. Separately, forkd-the-builder remains a
PRIVILEGED control-plane surface (root, `CAP_SYS_ADMIN`, `/dev/kvm`, the jailer),
but it is SMALLER than the old per-sandbox surface: it runs the BUILD path once
per node per template, not on every sandbox execution, so the privileged surface
is confined to the build path and amortized across all sandboxes a template
serves. Removing forkd's privilege entirely (a builder redesign) is out of scope.

Residual ledger (all accepted and tracked, see ROADMAP W1 #18):

- the SHARED READ-ONLY SNAPSHOT MOUNT across husk pods on a node (read-only,
  integrity-verified, non-tenant base image; fully pod-native CAS delivery is the
  follow-up);
- the `/dev/kvm` INHERENT host-escape surface (unchanged from raw-forkd; inherent
  to any Firecracker host);
- the IN-MEMORY ENC-KEY WINDOW while a container is open (HSM custody is the
  follow-up, #31);
- the FORKD-BUILDER PRIVILEGE (it stays the privileged builder; a builder
  redesign is out of scope);
- the IN-VM NetworkPolicy enforcement and the live-Checkpoint-on-drain survival,
  both proven only on a KVM-capable kubelet (a bare-metal reference node, #16).

The device-plugin surface itself is in section 3; the per-mode networking
reconciliation is in section 4; the encryption-key custody is in section 5.

## 1. Guest → host escape

The primary boundary is KVM hardware virtualization via Firecracker.

| Control | Status | Detail |
|---|---|---|
| Firecracker microVM (minimal device model) | **mitigated** | Each sandbox is a separate Firecracker process with its own KVM VM (`internal/fork/engine.go`). |
| Jailer (dedicated UID, chroot, cgroup, namespaces per VM) | **mitigated by design; capability set pending proof in the KVM CI jailer run (issue #2 Task 5)** | forkd launches every Firecracker process through the jailer (`internal/firecracker/jailer.go`, `client.go:startJailedVM`): a dedicated uid/gid per VM from `--uid-range` (default 64000-64999; uid 0 refused), a per-VM chroot under `--chroot-base` containing only the explicitly hard-linked kernel, rootfs, and snapshot files (a traversal guard refuses anything outside the data dir and the VM workspace), and cgroup v2 attachment. Caller-supplied ids are validated at the gRPC boundary (`internal/daemon/validate.go`, `[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}`) and the launch path independently refuses ids whose jailer directories would escape the chroot base, so ids cannot traverse into root-level filesystem operations. The shipped DaemonSet sets the jailer flags; forkd fails closed on misconfiguration (nonroot, chroot base on a different filesystem from the data dir, malformed uid range). Residuals, explicitly: the direct-exec dev path remains when `--jailer` is omitted (forkd logs a loud warning; standalone sandbox-server always runs unjailed); a VMM compromise now lands as a throwaway uid in an empty chroot instead of forkd's root, but hard-linked snapshot files inside the chroot remain readable to it. |
| Seccomp on the VMM process | **partial** | The jailer-launched VMM runs Firecracker's default production seccomp filters; Firecracker installs them on all VMM threads unless explicitly disabled, and we never pass `--no-seccomp` or a custom filter. We do not verify or customize the filter level; that stays out of scope until the jailer path is proven in KVM CI. |
| CVE posture / version pinning | **partial** | CI pins Firecracker v1.15.0; there is no documented update policy or advisory tracking. |
| Guest agent as attack surface | **partial** | Agent speaks a small JSON protocol over vsock only (`guest/agent/main.go`); host side treats responses as data. A 10MB line-buffer cap exists. No fuzzing of the protocol yet. |
| Workspace tar transfer (W4 hydrate/dehydrate) | **mitigated** | The guest agent serves `tar_dir`/`untar_dir` over the same vsock channel (`guest/agent/tardir.go`); these are NOT exposed on the tenant-facing HTTP sandbox API and are called only by the controller workspace lifecycle. UntarDir (host tar bytes into the guest) rejects absolute, `..`, and out-of-target members with an anchored-separator prefix check, and refuses every non-regular member (symlinks, hardlinks, devices) before any write, so a crafted workspace revision cannot write outside `/workspace` or escape through a symlink. TarDir is allowlisted to `/workspace` only and does not follow symlinks out of it. The dehydrate excludes credential paths (`.ssh`, `.aws`, `.netrc`, `.git-credentials`, `.config/gh`, `.npmrc`) and secrets live only in the guest's in-memory env, never on disk under `/workspace`. Both directions enforce a 64MiB `MaxTarBytes` cap with a per-file `io.LimitReader`. Residuals: a guest already running as root could hardlink an on-disk file into `/workspace` to capture its bytes into a revision (not a cross-VM escape; secrets are in-memory so unaffected); there is no per-transfer member-count cap yet (a low-severity local DoS against a compromised sandbox), to be addressed with the streaming-tar slice. |

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

PER-MODE ENFORCEMENT (reconciled with the husk default, section 0 surface 5):
exactly ONE egress layer governs a given sandbox, decided by the run mode, never
both. In the HUSK default (the shipped default), the VM's tap lives in the husk
POD's network namespace and a Kubernetes `NetworkPolicy`/Cilium selecting the
husk pod (`podSelector` on `agentrun.dev/husk=true`) is the governing layer; the
bespoke host-nftables engine below is NOT installed for husk pods. The rows below
describe the RAW-FORKD mode (`--enable-raw-forkd`, `--mock`), where there is no
pod and this host-nftables engine IS the enforcement. The NetworkPolicy attach is
CI-proven object-level on `kind-e2e-husk` (slice 4); the IN-VM enforcement of the
NetworkPolicy over the VM tap is proven only on a KVM-capable kubelet (a
bare-metal reference node), the documented residual. The raw-forkd nftables rows
below are CI-proven in-VM on KVM. See `docs/husk-pods.md` section 6d.

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
| K8s NetworkPolicy | **per-mode (section 0 surface 5)** | In RAW-FORKD mode sandboxes are not pods: NetworkPolicy does not govern them and our nftables egress layer is ours and documented as ours. In the HUSK default the VM's tap is in the husk pod's netns, so a NetworkPolicy/Cilium selecting `agentrun.dev/husk=true` IS the governing egress layer (no bespoke nftables for husk pods); the attach is CI-proven object-level on `kind-e2e-husk` (slice 4), the IN-VM enforcement over the VM tap is the bare-metal residual. Exactly one layer governs a given sandbox, never both. |

## 5. Snapshot integrity and supply chain

Snapshots are executable memory images; loading one is equivalent to running
arbitrary code at sandbox privilege.

| Control | Status | Detail |
|---|---|---|
| Content addressing (digest in CRD status) | **mitigated** | Every template snapshot is content-addressed in a CAS store the moment it is built: its sha256 manifest digest is recorded to `<dataDir>/templates/<id>/manifest.digest`, pinned in the store, reported through forkd `GetCapacity`/`CreateTemplate`, and written to `SandboxPoolStatus.TemplateDigest` so the snapshot identity is visible in `kubectl get sandboxpool -o yaml`. The digest is a content address and is safe to log. |
| Verify-on-load | **mitigated** | forkd verifies a snapshot's on-disk bytes against the recorded digest before it is forked, and refuses on mismatch. To keep the fork hot path cheap, verification is verify-once-at-registration: at build time (trusted, marker written without re-hash) or at first use after a restart (lazy re-hash), recorded by a `verified` marker that Fork only stats. The dev-mode escape `--allow-unverified-snapshots` downgrades a failed verification to a loud one-time warning. Residual: verification is at registration, not per fork, so tampering AFTER a snapshot is verified is not re-detected until the marker is cleared; external snapshot import is not yet supported. |
| Publish authorization | **mitigated** | Snapshots are produced only by forkd's own `CreateTemplate`, which is reachable solely over the mTLS-gated gRPC surface from the controller (PR #41). Externally supplied snapshots are not accepted, so the publish surface is exactly that authenticated `CreateTemplate` call. External snapshot import is future work. |
| Compatibility verification (no unsafe restore) | **mitigated** | The same load gate also runs the snapshot compatibility contract (`internal/snapcompat.Check`, issue #32) after the digest verify and before any Firecracker launch. The manifest records the producing environment (snapshot format version, Firecracker version, CPU model, kernel, config hash) as part of the content-addressed digest, so these fields cannot be tampered with or downgraded without changing the digest and failing the verify-on-load step above. A benign mismatch (a snapshot legitimately built under a different Firecracker version, a different CPU model, or an unsupported format version) fails closed: the restore is refused with an actionable error rather than crashing or silently corrupting a guest. The dev-mode escape `--allow-incompatible-snapshots` downgrades a refusal to a loud warning. Kernel mismatch is informational. Residual: cross-CPU-model restore via Firecracker CPU templates and live cross-Firecracker-version restore are out of scope (the contract refuses them today). |
| Encryption at rest + crypto-shredding (#31) | **mitigated** | Behind `--enable-encryption` (default off) each scope (a template now; a workspace when #21 lands) gets its own LUKS2 container (`internal/storecrypt`) backed by a sparse image; the snapshot and volumes are built inside the mounted, decrypted container, so the bytes at rest in `<scope>.img` are ciphertext, not the plaintext snapshot. dm-crypt sits below the page cache, so the mem mmap CoW restore reads decrypted pages and CoW page sharing across forks is preserved (no per-fork decryption copy). Erasure is crypto-shredding: `luksErase` wipes the LUKS keyslots and the image is removed at template delete, after which the ciphertext is unrecoverable even with the key. The key reaches cryptsetup only on stdin (`--key-file -`), never in argv or any log; `storecrypt.Key` redacts itself on any format. Proven in KVM CI on real cryptsetup: the marker is absent in the raw image but present in the decrypted mount (ciphertext at rest), reopen+read returns it intact (decrypt/restore works), and after shred a reopen with the original key fails and the image is gone (unrecoverable). Key custody (PR2): the controller generates a per-template 256-bit key with `crypto/rand`, stores it in a `<template>-enc-key` Kubernetes Secret owner-referenced to the `SandboxTemplate` (so GC of the template GCs the Secret), and delivers the key to forkd in the mTLS-protected `CreateTemplate` and `Fork` gRPC requests. forkd holds the key in process memory only via `RequestKeyProvider` and NEVER writes it to the node data disk; encryption enabled with no delivered key fails closed. The mTLS channel is now ENFORCED, not merely used: forkd refuses to start with `--enable-encryption` unless its TLS cert/key/CA flags are set, and the controller refuses to deliver the key to a node whose connection is not mTLS (it fails the encrypted build/fork for that node rather than transmit the key in cleartext), so the key cannot travel an insecure gRPC channel. The key is never logged anywhere in the key-custody code path (enforced by grep in CI). Proven by envtest and unit tests: Secret lifecycle (create + idempotent read + GC via owner ref), key-over-RPC, key-not-on-disk, fail-closed, and key-never-logged. See docs/encryption.md. HUSK DEFAULT (section 0 surface 7): the same mTLS-only delivery and node-memory-only custody apply on the husk path: the key reaches the node over the mTLS control channel, is held in node memory while a container is open, and is never written to the node disk; the in-memory-key window is the named residual (HSM custody is the #31 follow-up). Residuals, explicitly: (1) etcd-at-rest-encryption trust: the Secret data is plaintext in etcd unless the cluster operator configures KMS-backed EncryptionConfiguration; that is the operator's responsibility and is stated as an assumption. (2) Controller trust: a compromised controller can read the Secret and deliver the key to any forkd; the cluster admin boundary is the trust anchor. (3) Node-memory dump while open: while a container is open the key is necessarily in forkd process memory; a root attacker with a memory dump recovers it; zeroize-on-close is the current mitigation, full HSM custody is the follow-up. (4) TEARDOWN BOUNDARY: the controller does not yet send a DeleteTemplate RPC on SandboxTemplate deletion, so the node-side container is GC'd by node data dir lifecycle rather than a controller-driven crypto-shred; tracked as follow-up. Out of scope for now: KMS/HSM envelope encryption, key rotation/re-encryption, per-workspace scope (#21), encrypting the CAS chunk store. |

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
