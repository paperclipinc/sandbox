# CLAUDE.md

## Project Overview

Snapshot-fork sandboxes for AI agents on Kubernetes. The system boots Firecracker microVMs, forks them via copy-on-write snapshots, and exposes the whole lifecycle through declarative CRDs (SandboxTemplate, SandboxPool, SandboxClaim, SandboxFork) in API group `mitos.run/v1alpha1`.

Components:

- **controller** (Deployment): reconciles the CRDs, selects nodes, drives forkd.
- **forkd** (DaemonSet): per-node fork daemon; gRPC on :9090 for the controller, HTTP sandbox API on :9091 for exec and file traffic.
- **guest agent** (PID 1 in the VM): speaks the vsock protocol for exec, files, env, and fork notifications.
- **sandbox-server** (standalone): the same engine behind a plain REST API, no Kubernetes required.
- **Python SDK** (`sdk/python`): client for both k8s mode and sandbox-server mode.

ROADMAP.md is the priority order for all work. docs/api/v2-spec.md is the target API.

## Operating Principles

These outrank convenience:

1. **No unverified claims.** Every public number must be reproducible from `bench/` or it does not get written.
2. **Security findings block features.** The threat model (docs/threat-model.md) must be updated in the same PR whenever the security surface moves.
3. **Honest Kubernetes semantics.** Sandboxes are not pods; never imply pod-scoped mechanisms (NetworkPolicy, ResourceQuota, PSA) govern them.
4. **Boring failure behavior.** Every component defines what happens on crash, node loss, slow etcd, and capacity exhaustion.
5. **Bare metal is a first-class target.**

## Commands

```bash
make build                # controller + forkd binaries
make test-unit            # fork, workspace, vsock unit tests
make test-controller      # envtest suite (needs setup-envtest)
make test-python          # Python SDK tests
make proto                # regenerate gRPC stubs from proto/forkd.proto
make generate manifests   # regenerate deepcopy + CRD YAML after api/ changes
```

- Direct controller tests: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
- Python tests directly: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/`
- Lint, BOTH invocations are required: `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`. The guest agent is linux-only and invisible to the darwin run.
- Cross-build check for the guest agent: `GOOS=linux GOARCH=amd64 go build ./guest/agent/`

## Architecture

- **controller** (`cmd/controller`, `internal/controller`): reconciles SandboxTemplate, SandboxPool, SandboxClaim, SandboxFork; tracks forkd nodes via the NodeRegistry fed by capacity heartbeats.
- **forkd** (`cmd/forkd`, `internal/daemon`): node daemon that owns VMs; gRPC service on :9090 (fork, prepare-pool, heartbeat), HTTP sandbox API on :9091 (exec, files, status).
- **fork engines** (`internal/fork`): the real engine (internal/fork/engine.go) drives Firecracker snapshot/restore and needs KVM; the mock engine (internal/fork/mock.go, KVMAvailable=false) is used by kind e2e and envtest.
- **firecracker client** (`internal/firecracker`): VM lifecycle over the Firecracker API socket.
- **guest agent** (`guest/agent`): PID 1 inside the VM; serves exec, file IO, and env over vsock (`internal/vsock` is the host side).
- **sandbox-server** (`cmd/sandbox-server`): standalone REST API on the same engine, no k8s.
- **Python SDK** (`sdk/python/mitos`): talks to forkd or sandbox-server.

Data paths:

- **Claim path**: controller selects a node from the NodeRegistry, calls forkd `Fork` over gRPC; the claim status endpoint is forkd's HTTP API on that node.
- **Exec path**: SDK -> forkd :9091 -> vsock -> guest agent.

## Coding Conventions

### Punctuation (strict)

Never use em (U+2014) or en (U+2013) dashes anywhere: source, comments, docstrings, Markdown, YAML, CRD descriptions, commit messages, PR descriptions, the GitHub repo description, and release notes. Use only `.` `,` `;` `:` as punctuation connectors. ASCII hyphen-minus (-) is fine for ranges and compound identifiers. If a third-party tool inserts one (release-please, Dependabot), rewrite it before merging.

### Go style

- Error wrapping: `fmt.Errorf("context: %w", err)`.
- Octal literals as `0o644`.
- gofmt and golangci-lint clean is a merge requirement.
- Test files are excluded from errcheck via .golangci.yml; production code is not.

### Secrets

Secret VALUES are never logged, never in error messages, never in condition messages, never written to host paths. Log keys and counts only. Runtime errors should carry actionable remediation text (the API v2 LLM-legible error rule, issue #28).

### Commits and branches

- Conventional commits: feat, fix, docs, ci, chore, refactor, test.
- Branch naming: feat/, fix/, chore/, docs/, ci/, refactor/.

### TDD

Write the failing test first. Every behavior change lands with its test in the same commit.

### git

- Stage explicit paths only; never `git add -A`.
- README claims follow the no-unverified-claims rule: every number it states must be reproducible from `bench/` or carry an explicit issue reference marking it as a target.

## CI Pipeline

Jobs:

- **go-test**: build, vet, full test suite; envtest assets installed in-job.
- **go-lint**: golangci-lint.
- **python-test**: SDK pytest.
- **docker-build**: controller and forkd images.
- **kind-e2e**: mock engine on kind (config hack/kind-config.yaml).
- **firecracker-test** (kvm-test.yaml): real Firecracker snapshot/restore plus guest agent exec over vsock on KVM runners.

All six are required checks on main; main requires branches to be up to date.

## Security Practices

- The forkd threat surface is documented in docs/threat-model.md with per-row status; keep it current in the same PR as any surface change.
- Fork-correctness hazards (RNG, clocks, secret inheritance) live in docs/fork-correctness.md.
- Security-sensitive paths require extra care and a named human reviewer before merge: `internal/fork`, `internal/firecracker`, `internal/daemon`, `guest/agent`, and future token/attenuation code.
- Sequencing gates: the fork-correctness suite and failure/GC semantics must be green in CI before any integration workstream ships to production tenants.

## Workflow Pointers

- ROADMAP.md is the priority order; GitHub issues #2-#37 map it.
- Plans live in docs/superpowers/plans/.
- Every PR needs: tests, docs updated in the same PR, a threat-model delta if the security surface moved, and a benchmark run if the hot path was touched.
