# ADR 0001: the agents.x-k8s.io facade and the our-vs-their naming collision

Status: accepted (2026-06-12)
Issue: #19 (W2 conformance facade). Related: docs/api/v2-spec.md, docs/facade-conformance.md.

## Context

The SIG agent-sandbox project defines an upstream Kubernetes API for agent
sandboxes: group `agents.x-k8s.io`, version `v1alpha1`, kind `Sandbox` (with
extension kinds `SandboxClaim`, `SandboxWarmPool`, `SandboxTemplate` in
`extensions.agents.x-k8s.io`). Issue #19 requires us to present sandboxes
through that exact API on our fork engine: "implement their API, do not fork or
shadow it."

We already run our own engine through our own CRDs in group `mitos.run`
(`SandboxTemplate`, `SandboxPool`, `SandboxClaim`, `SandboxFork`). Two questions
had to be settled:

1. How do we make the upstream API available to our controller without forking
   or shadowing it, given our toolchain and lint constraints.
2. How do we handle the fact that both groups use the noun "Sandbox" (and
   "SandboxClaim"), which do not collide at the apiserver but confuse readers.

## Decision 1: implement agents.x-k8s.io/v1alpha1 via a separate facade, vendoring their types

We implement the upstream API as a facade: a controller-runtime controller
(`internal/facade`) that watches the upstream `agents.x-k8s.io/v1alpha1 Sandbox`
and reconciles each onto our husk-backed run path (a `SandboxClaim` in our
`mitos.run` group, bound to one of our pools). The facade runs as a SEPARATE
binary (`cmd/facade`) with its own manager, so it is opt-in and does not
entangle our core controller (`cmd/controller`).

The single bridge annotation `mitos.run/pool` on an upstream Sandbox selects
which of our pools (the warm-pool source) fulfils it; when unset, the facade
uses its configured `--default-pool`. All our extras (pools, warm pools,
templates, fork policies, budgets) stay in our `mitos.run` group and are
never grafted onto the upstream API surface.

### Toolchain path: vendored types after a go 1.26 bump (the faithful path)

The faithful way to implement their exact API is to vendor their Go types
(`sigs.k8s.io/agent-sandbox`) rather than re-declare the CRD by hand. Every
published version of that module declares `go 1.26`, so vendoring forces our
toolchain from go 1.24 to go 1.26. The historical risk (the "go-version-vs-lint
wall": a dependency requiring a higher go than golangci-lint was built with
fails the lint job) made this conditional.

We executed the decision rule in Task 1 and recorded the evidence:

- `go get sigs.k8s.io/agent-sandbox@v0.4.6` + `go mod edit -go=1.26` + `go mod
  tidy` succeeded; the transitive bump (k8s.io 0.32 -> 0.35,
  controller-runtime 0.19 -> 0.23) built clean on both `go build ./...` and
  `GOOS=linux go build ./...`.
- `golangci-lint run` AND `GOOS=linux golangci-lint run` both passed on the
  go 1.26 module with golangci-lint v1.64.8 (built with go 1.26.3). The
  go-version-vs-lint wall did NOT trip.
- The full existing test suite (controller envtest + units) stayed green on the
  upgraded dependencies.

Because both lint runs and both builds passed, we KEPT the vendor + bump (the
faithful path) and did NOT fall back to re-declaring the CRD by hand.

Consequences of keeping the bump:

- The go directive resolves to `1.26.2`, not `1.26.0`: the upstream module
  declares `go 1.26.2`, and Go's minimum-version selection propagates that
  floor, so `go mod tidy` rejects a lower directive while the dependency is
  present. `setup-go` in CI and the `golang:1.26-bookworm` base images use
  `1.26`, which covers `1.26.2`.
- The CI `go-lint` job is pinned to golangci-lint `v1.64.8` (the last v1 line):
  it is built with go 1.26, it reads our existing v1-format `.golangci.yml`
  (the current `latest` is golangci-lint v2, whose config format differs), and
  it is the exact binary the bump was verified against locally.

The fallback path (had the lint wall tripped): revert the dep + the bump, stay
on go 1.24, and define `internal/facade/apis/v1alpha1 Sandbox` types by hand
from the vendored CRD YAML in `third_party/agent-sandbox/`. We did NOT take this
path; it is recorded here so the drift guard (the upstream conformance e2e, a
later slice) and any future toolchain regression have the documented
alternative.

Either path serves the same CRD: `agents.x-k8s.io/v1alpha1 Sandbox`.

### What the facade maps

- Sandbox with `replicas` 1 (the upstream default) -> create/ensure our
  `SandboxClaim` (same name + namespace, owner-referenced to the Sandbox for GC
  + the watch back-link), bound to the annotated/default pool.
- Our claim reaching phase `Ready` -> the upstream Sandbox `status` Ready
  condition `True`, `status.replicas` 1, and a derived `status.serviceFQDN`.
- Sandbox with `replicas` 0 -> terminate our claim; report Ready `False`,
  replicas 0.
- Sandbox deletion -> the owner reference garbage-collects our claim.
- `spec.podTemplate`: container env is reconciled onto the claim env. Other
  podTemplate fields (image, resources, volumes, security context) are a
  documented conformance exception for a later slice; the husk pool pins image
  and resources at pool-build time. See docs/facade-conformance.md.

## Decision 2: keep our nouns now, defer the rename to the API v2 migration

Our `SandboxTemplate`/`SandboxClaim`/`SandboxPool` (group `mitos.run`) and
their `Sandbox`/`SandboxClaim`/`SandboxWarmPool` (group `agents.x-k8s.io`) live
in DIFFERENT API groups, so they do NOT collide at the apiserver: a cluster can
serve both, and `kubectl get sandboxclaims.mitos.run` and
`kubectl get sandboxclaims.extensions.agents.x-k8s.io` are distinct resources.
The collision is purely cognitive: two unrelated `SandboxClaim` kinds confuse
operators and readers.

Per issue #19 and docs/api/v2-spec.md, the preferred resolution is to rename
OUR nouns so the word "Sandbox" unambiguously means the upstream object: our
forky resources become `ForkTemplate`/`ForkClaim`/`ForkPool` (and the v2 spec
further consolidates the noun set: pools prepare, sandboxes run, workspaces
persist, with `SandboxFork` folded into a Sandbox `source.fromSandbox` +
`replicas`).

Decision: DEFER the rename to the API v2 migration. Rationale:

- A rename is a breaking change for every CRD, manifest, SDK, and stored object.
  Doing it now (for the facade) and again at v2 (for the consolidation) is two
  breaking renames; doing it once, with v2, is one.
- The facade does NOT need the rename to function: it already uses the distinct
  `agents.x-k8s.io` group for the upstream surface and `mitos.run` for our
  own, so there is no apiserver ambiguity to fix for the facade itself today.
- v2 is where the noun set is reshaped anyway (v2-spec §5), so the rename lands
  naturally there as part of one migration with a documented upgrade path.

Until then, the cognitive collision is mitigated by always group-qualifying our
kinds in docs and by this ADR. The deferred-rename plan: execute
`ForkTemplate`/`ForkClaim`/`ForkPool` (or the v2-consolidated nouns) together
with the API v2 migration, with conversion webhooks / a documented upgrade path,
before 1.0.

## Status of the slice

PROVEN now (envtest, internal/facade): the upstream Sandbox -> our husk
run-path lifecycle (create -> Ready mirror -> replicas-0 termination ->
delete/GC linkage), with the bridge annotation and the default pool.

OPEN (later #19 slices): the full upstream conformance e2e harness run in CI;
pause/resume as a memory snapshot vs their disk-roundtrip hibernation (a
documented behavioral difference measured in bench/facade later, not asserted
here); the SandboxWarmPool -> our pool and SandboxClaim -> our
fork-from-snapshot extension mappings; full podTemplate fidelity; executing the
deferred rename with API v2.
