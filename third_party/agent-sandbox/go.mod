// This go.mod marks the vendored upstream agent-sandbox artifacts as a SEPARATE
// nested module so the Go toolchain in the parent module (go build / go vet /
// go test ./...) does NOT try to compile them. The vendored test/e2e Go files
// are kept verbatim as the conformance REFERENCE (the matrix in
// docs/facade-conformance.md maps to these upstream tests); they are not
// compiled in this repo. They import sigs.k8s.io/agent-sandbox/... (the upstream
// module path), so this nested module declares that same path. We do not list
// the upstream require graph here on purpose: the reference is read, not built.
//
// Do NOT add this module to a go.work or run go mod tidy against it; the
// upstream artifacts under this directory are vendored verbatim and must not be
// edited (apply-unchanged is the conformance point). See README.md and
// docs/facade-conformance.md.
module sigs.k8s.io/agent-sandbox

go 1.26.2
