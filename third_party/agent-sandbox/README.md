# Vendored upstream: sigs.k8s.io/agent-sandbox

These are vendored upstream artifacts from the SIG agent-sandbox project,
copied verbatim from the module cache for `sigs.k8s.io/agent-sandbox@v0.4.6`.
They are NOT our work; the upstream Apache 2.0 `LICENSE` is preserved alongside
them.

We vendor these so the facade (issue #19, `cmd/facade` + `internal/facade`,
group `agents.x-k8s.io`) can install the upstream CRDs in envtest today and so
the conformance harness slice can run the upstream example manifests against the
facade later. See docs/adr/0001-facade-and-naming.md and
docs/facade-conformance.md.

## Version

- Module: `sigs.k8s.io/agent-sandbox`
- Version: `v0.4.6`
- API: group `agents.x-k8s.io`, version `v1alpha1` (core `Sandbox`); group
  `extensions.agents.x-k8s.io`, version `v1alpha1` (the extension kinds).

## Contents

- `crds/agents.x-k8s.io_sandboxes.yaml`: the core `Sandbox` CRD (installed by
  the facade envtest suite).
- `crds/extensions.agents.x-k8s.io_sandboxclaims.yaml`,
  `crds/extensions.agents.x-k8s.io_sandboxwarmpools.yaml`,
  `crds/extensions.agents.x-k8s.io_sandboxtemplates.yaml`: the extension CRDs,
  vendored for the later extension-mapping and conformance slices.
- `examples/hello-world.yaml`: a minimal upstream `Sandbox` manifest.
- `examples/sandboxwarmpool.yaml`: an upstream `SandboxWarmPool` example, for
  the later warm-pool mapping slice.
- `LICENSE`: the upstream Apache 2.0 license, preserved.

## Updating

Bump the version above, re-copy from
`$(go env GOMODCACHE)/sigs.k8s.io/agent-sandbox@<version>/` (the `k8s/crds/`
directory and `examples/`), and re-run the facade envtest suite. The conformance
approach pins the latest two upstream minors; see docs/facade-conformance.md.
