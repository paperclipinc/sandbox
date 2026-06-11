# agents.x-k8s.io facade conformance approach

This document records how the `agents.x-k8s.io` conformance facade (issue #19,
`cmd/facade` + `internal/facade`) is held to the upstream SIG agent-sandbox API,
what is proven today, and what is deferred. See ADR 0001
(docs/adr/0001-facade-and-naming.md) for the facade design and the toolchain
decision.

## Principle: no silent divergence

We implement the upstream API (`agents.x-k8s.io/v1alpha1 Sandbox`); we do not
fork or shadow it. The drift guard is the upstream conformance suite, not our
own assertions. Every behavioral difference between the facade and an upstream
reference implementation is either (a) eliminated, or (b) written down here as a
justified, documented exception. There is no third option: an undocumented
divergence is a bug.

## The harness (a later slice)

The conformance harness itself is deferred to a later #19 slice. The planned
shape:

- Vendor the upstream `examples/` and `test/e2e` from `sigs.k8s.io/agent-sandbox`
  into the repo (the foundation slice already vendors the CRDs and a couple of
  example manifests under `third_party/agent-sandbox/`; the harness slice adds
  the e2e suite).
- Run those upstream manifests and e2e tests in CI against the facade,
  UNCHANGED: the acceptance criterion is that their manifests apply and behave
  identically to an upstream reference, with only the documented exceptions
  below.
- Pin the upstream version matrix to their latest two minor releases, so we
  track the API as it moves and catch drift early. The foundation slice pins
  v0.4.6 (the CRDs + examples vendored under `third_party/agent-sandbox/`).

## Documented exceptions (justified, not silent)

These are the known, intentional differences. Each is a target for the harness
slice to either close or keep as a recorded exception.

1. podTemplate fidelity. The facade reconciles the upstream
   `spec.podTemplate.spec.containers[*].env` onto our run path. Other
   podTemplate fields (image, resources, volumes, security context, init
   containers, multiple containers) are NOT yet honored per-Sandbox: the husk
   pool pins image and resources at pool-build time, and a Sandbox binds to a
   pool via the `agentrun.dev/pool` bridge annotation. Closing this requires the
   pool/template extension mappings (a later slice).

2. pause/resume semantics. Upstream hibernation is a disk roundtrip (the pod is
   torn down and its state persisted to a volume, then rebuilt). Ours is a
   memory snapshot/restore on the fork engine: the VM state is captured and
   restored from a snapshot, which is a different mechanism and is expected to
   be substantially faster (memory restore vs disk rebuild). This is a
   behavioral difference, NOT a behavioral regression: the observable lifecycle
   (paused -> resumed, state preserved) is equivalent. The speed claim is a
   TARGET, not asserted here: it will be measured in `bench/facade/` in a later
   slice against an upstream reference. No public number is stated until that
   benchmark exists (the no-unverified-claims rule).

3. extension kinds. `SandboxWarmPool` -> our pool and `SandboxClaim`
   (the upstream extension) -> our fork-from-snapshot are NOT yet mapped. The
   facade today maps only the core `Sandbox`. The extension mappings are a later
   slice.

4. stable identity / storage contract. Stable hostname/identity and the full
   storage (`volumeClaimTemplates`) contract fidelity are a later slice.

## What is PROVEN now

In `internal/facade` (envtest, installs the vendored upstream Sandbox CRD plus
our agentrun.dev CRDs):

- An upstream `Sandbox` (replicas 1, the default) drives the facade to create
  our husk-backed `SandboxClaim`, bound to the pool named by the
  `agentrun.dev/pool` bridge annotation, or the configured `--default-pool` when
  unset.
- The claim is owner-referenced to the Sandbox (GC + the watch back-link), and
  the Sandbox podTemplate container env is mirrored onto the claim env.
- Driving our claim to phase `Ready` mirrors a Ready `True` condition,
  `status.replicas` 1, and a derived `status.serviceFQDN` into the upstream
  Sandbox status.
- `replicas` 0 terminates our claim and reports Ready `False`, replicas 0.
- Deleting the Sandbox leaves our claim owner-referenced for the apiserver
  garbage collector to reap.

## What is OPEN

- The full upstream conformance e2e harness vendored and run in CI against the
  facade (their manifests unchanged, identical behavior), with the latest-two-
  minors version matrix.
- pause/resume as a memory snapshot/restore, and the milliseconds-vs-
  hibernation comparison in `bench/facade/`.
- The `SandboxWarmPool` -> our pool and `SandboxClaim` -> our fork-from-snapshot
  extension mappings.
- Full podTemplate fidelity, stable hostname/identity, and the storage contract.
