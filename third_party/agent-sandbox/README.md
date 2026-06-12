# Vendored upstream: sigs.k8s.io/agent-sandbox

These are vendored upstream artifacts from the SIG agent-sandbox project,
copied VERBATIM from the module cache for `sigs.k8s.io/agent-sandbox@v0.4.6`.
They are NOT our work; the upstream Apache 2.0 `LICENSE` is preserved alongside
them. We do NOT edit these manifests or tests: applying them unchanged is the
whole point of the conformance harness (issue #19). If you need to update them,
re-vendor from a newer pinned upstream version (see Updating below).

We vendor these so the facade (`cmd/facade` + `internal/facade`, group
`agents.x-k8s.io`) can:

- install the upstream CRDs in envtest and on a kind cluster;
- apply the upstream example `Sandbox` manifests UNCHANGED against the facade and
  assert object-level conformance (the bridged husk-backed claim, the mirrored
  status, replicas 0, deletion);
- map every upstream `test/e2e` conformance test and every vendored example to a
  status row in the conformance matrix.

See docs/adr/0001-facade-and-naming.md and docs/facade-conformance.md.

## Version

- Module: `sigs.k8s.io/agent-sandbox`
- Version: `v0.4.6` (pinned)
- API: group `agents.x-k8s.io`, version `v1alpha1` (core `Sandbox`); group
  `extensions.agents.x-k8s.io`, version `v1alpha1` (the extension kinds:
  `SandboxWarmPool`, `SandboxTemplate`, `SandboxClaim`).

## Contents

- `crds/`: the upstream CRDs (copied from the upstream `k8s/crds/`):
  - `agents.x-k8s.io_sandboxes.yaml`: the core `Sandbox` CRD (installed by the
    facade envtest suite and the facade-conformance kind job).
  - `extensions.agents.x-k8s.io_sandboxclaims.yaml`,
    `extensions.agents.x-k8s.io_sandboxwarmpools.yaml`,
    `extensions.agents.x-k8s.io_sandboxtemplates.yaml`: the extension CRDs,
    vendored for the later extension-mapping slices.
- `examples/`: the full upstream `examples/` tree, including the core `Sandbox`
  example manifests (`hello-world-sandbox/hello-world.yaml`,
  `sandbox-ksa/sandbox.yaml`, `hermes-agent/sandbox.yaml`,
  `python-runtime-sandbox/sandbox-python-kind.yaml`, `aio-sandbox/aio-sandbox.yaml`,
  and others). The facade envtest applies every core `Sandbox` example here
  unchanged and asserts the bridged claim.
- `extensions/examples/`: the upstream extension example manifests
  (`SandboxWarmPool`, `SandboxTemplate`, `SandboxClaim`), for the later
  extension-mapping slices.
- `test/e2e/`: the upstream Go conformance tests (`*_test.go`) plus the
  `framework/` package, vendored as the conformance REFERENCE. The matrix in
  docs/facade-conformance.md maps each `test/e2e/*_test.go` to a status. These
  files are NOT compiled in this repo: `go.mod` here declares a SEPARATE nested
  module (the upstream module path) so the parent module's `go build` /
  `go vet` / `go test ./...` skip this subtree. They import upstream-internal
  packages and require a running sandbox to pass, so they are read, not built.
- `go.mod`: the nested-module marker described above. Do NOT add it to a
  `go.work` or run `go mod tidy` against it.
- `LICENSE`: the upstream Apache 2.0 license, preserved.

## Updating

Bump the pinned version above, then re-copy from the module cache:

```bash
UP="$(go env GOMODCACHE)/sigs.k8s.io/agent-sandbox@<version>"
cp -R "$UP/examples" examples
cp -R "$UP/extensions/examples" extensions/examples
cp -R "$UP/test" test
cp "$UP/k8s/crds/"*.yaml crds/
chmod -R u+w examples extensions test crds
```

(The module cache is read-only, hence the `chmod`.) Re-run the facade envtest
suite and update the conformance matrix in docs/facade-conformance.md. The
conformance approach pins the latest two upstream minors; the second minor is a
follow-up if only one is wired. See docs/facade-conformance.md.
