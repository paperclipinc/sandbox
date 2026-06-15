# ADR 0003: the /dev/kvm device-plugin PSA exception

Status: accepted (2026-06-15)
Issue: #18 (W1 husk pods), #30 (residual ADRs). Related: docs/threat-model.md
section 0 (surfaces 1, 3, 4) and section 3 (device-plugin row); docs/husk-pods.md
section 5 and 6e; ROADMAP.md compliance & observability addendum ("PSA
`restricted` with exactly one documented /dev/kvm exception"); the code is
`internal/controller/huskpod.go`, `cmd/kvm-device-plugin`,
`internal/deviceplugin`.

## Context

The default per-sandbox runner is the husk pod (`cmd/husk-stub`): an UNPRIVILEGED
pod that runs exactly one Firecracker microVM. To run a VM, Firecracker must open
`/dev/kvm`. The naive way to grant that is `privileged: true` or a `/dev/kvm`
hostPath, but both make the pod privileged-class and break the goal of running
the tenant execution surface under PodSecurity Admission (PSA) `restricted`.

A husk pod's securityContext (`internal/controller/huskpod.go`, proven in
`internal/controller/huskpod_test.go` and against the v1.31 PodSecurity admission
plugin on the `kind-e2e-husk` job) sets every PSA `restricted` control:
`privileged: false`, `allowPrivilegeEscalation: false`, `capabilities.drop:
[ALL]` with none added back, and `seccompProfile: RuntimeDefault` at both the pod
and container level. The decision to record here is HOW the pod still reaches
`/dev/kvm` without becoming privileged, and exactly which PSA-restricted controls
it must waive to do so.

This is a compliance-sensitive decision: the compliance addendum (ROADMAP.md)
permits the claim "PSA `restricted` with exactly one documented /dev/kvm
exception". That phrasing has to be precise, because the husk pod actually takes
TWO documented exceptions, and a claim of "exactly one" would be inaccurate. This
ADR records the exact exception set so the claim language and the threat model
agree.

## Decision: inject /dev/kvm via a device plugin and take exactly two documented PSA-restricted exceptions

KVM access is injected by a device plugin (`cmd/kvm-device-plugin`,
`internal/deviceplugin`): the husk pod requests the extended resource
`mitos.run/kvm` like any other, and the kubelet bind-mounts `/dev/kvm` (and
`/dev/net/tun`) on `Allocate`. The pod therefore sets NO `privileged: true` and
carries NO `/dev/kvm` hostPath. CI-proven on the `kind-e2e` job: the full
advertise -> schedule -> inject path runs with a NON-privileged probe pod
(`privileged: false`, escalation false, drop ALL, read-only rootfs) and `kubectl
exec` confirms `/dev/kvm` is present inside the container, coming entirely from
`Allocate`, not from any privilege (docs/husk-pods.md section 5).

The husk pod is kept out of a PSA `restricted` namespace by EXACTLY TWO
documented exceptions, both intrinsic to running a microVM:

1. `runAsNonRoot: false` (uid 0 inside the pod), so Firecracker can open the
   injected `/dev/kvm` without `privileged`. This is the device exception.
2. The read-only snapshot hostPath (the node template snapshot mem/vmstate and
   the kernel file, both mounted `ReadOnly: true`), because the snapshot is a
   node-built base image delivered by mount today (docs/threat-model.md section 0
   surface 3).

The SAME securityContext minus those two exceptions IS admitted into a
`restricted` namespace, and a genuinely privileged pod IS rejected in the same
namespace; both are asserted on `kind-e2e-husk` (docs/husk-pods.md section 6e),
so PSA is enforcing, not advisory, in the test.

The device-plugin DaemonSet (`deploy/device-plugin/daemonset.yaml`) is itself
held to a tight surface: it runs as root only because the kubelet
device-plugins dir is root-owned, but it is `privileged: false`,
`allowPrivilegeEscalation: false`, ALL capabilities dropped, and
`readOnlyRootFilesystem: true`; its only host access is the kubelet
device-plugins dir (to serve and register its socket) and a read-only `/dev` (to
`stat /dev/kvm`). It creates no device nodes and starts no VMs
(docs/threat-model.md section 3 device-plugin row).

## Why not privileged or a hostPath

- `privileged: true` grants ALL capabilities and disables the seccomp and
  device-cgroup confinement; a guest that escapes the microVM into the husk
  process would land with full privilege. That is precisely the old raw-forkd
  posture this model replaces (ADR 0005, docs/threat-model.md section 0 surface
  1). The device plugin removes the privileged REQUIREMENT.
- A `/dev/kvm` hostPath would also waive a PSA control and additionally hand the
  pod a writable host device path; the device plugin scopes the grant to the one
  bind-mounted device via the kubelet `Allocate` path, with the pod requesting it
  as an ordinary extended resource (so the scheduler sees it and gates on it).

## Consequences

- The compliance claim language is bounded by this exception set. The honest
  claim is "PSA `restricted` with exactly the documented `/dev/kvm` device-plugin
  exception", and the threat model and docs/compliance-claims.md must keep the
  exception COUNT accurate: it is two controls (`runAsNonRoot: false` and the
  read-only snapshot hostPath), both driven by the one device-access need. A
  blanket "PSA restricted, no exceptions" claim is FALSE and forbidden.
- The device plugin removes the privileged requirement, NOT the `/dev/kvm` attack
  surface. A KVM or host-kernel bug reachable from any `/dev/kvm` holder is still
  a host-escape vector, and it is the SAME risk in the husk and raw-forkd models
  (docs/threat-model.md section 0 surface 4, the "EQUAL" axis). This ADR does not
  claim to close that surface; it claims to remove the PRIVILEGE around it.
- The read-only snapshot hostPath exception is itself a residual: all husk pods
  on a node share that read-only mount, and fully pod-native CAS snapshot
  delivery (which would remove the hostPath exception, leaving only the
  `runAsNonRoot` device exception) is a tracked follow-up (docs/threat-model.md
  section 0 surface 3, ROADMAP W1 #18).
- Operators sizing or auditing a cluster reason about an extended resource
  (`mitos.run/kvm`) advertised by the device-plugin DaemonSet on labelled KVM
  nodes, not about privileged pods.
