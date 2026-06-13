# mitos CLI

`mitos` is the command-line interface for snapshot-fork sandboxes. It drives
the sandbox lifecycle (create, exec, file IO, fork, terminate, list) against a
Kubernetes cluster, and brings a local kind dev cluster up or down for a
one-command local-dev loop.

```bash
go build -o mitos ./cmd/mitos/
```

## Command reference

```
mitos run <command> [--pool P] [--timeout N]   create a sandbox, run the
                                                  command, terminate, exit with
                                                  the command's exit code
mitos sandbox create [--pool P]                create a sandbox, print its id
mitos sandbox ls [-n namespace] [-A]           list sandboxes
mitos sandbox exec <id> <command...>           run a command in a sandbox
mitos sandbox fork <id> [--replicas N]         fork a sandbox, print new ids
mitos sandbox terminate <id>                   destroy a sandbox
mitos dev up [--skip-cluster-create]           bring a local kind dev cluster
                                                  up with a mock control plane
mitos dev down                                 delete the local kind dev cluster
```

Global flags `--namespace`/`-n` and `--pool` may appear before the subcommand.
`mitos run` exits with the executed command's exit code so it chains in shell
pipelines.

## Backends

### Cluster backend (kubeconfig)

For every `run` and `sandbox` verb, `mitos` resolves a Kubernetes connection
from the standard kubeconfig (`KUBECONFIG`, `--kubeconfig`, or in-cluster). It
then:

- `sandbox create` creates a `SandboxClaim` referencing the pool and waits for it
  to reach the `Ready` phase, then prints the claim name as the sandbox id.
- `sandbox exec` / file IO reads the per-sandbox bearer token from the claim's
  Secret at request time and calls the claim's HTTP sandbox API. The token value
  is held in memory only for the request and is never logged; it is redacted from
  any error string.
- `sandbox fork` creates a `SandboxFork` and waits for the requested number of
  forks to be `Ready`.
- `sandbox ls` lists `SandboxClaim`s (a namespace with `-n`, all namespaces with
  `-A`, or the backend default otherwise).
- `sandbox terminate` deletes the `SandboxClaim`, which the controller reaps.

This is the same `SandboxClaim` path the controller and forkd implement; the CLI
is a thin client over the CRDs plus the token-scoped HTTP exec.

### Dev mock-mode local cluster

`mitos dev up` brings up a local kind cluster running a MOCK control plane so
the full claim path completes without KVM:

1. `kind create cluster` (tolerating an already-existing cluster; skipped with
   `--skip-cluster-create` to target a cluster you already stood up).
2. `kubectl apply -f deploy/crds/` (the CRDs).
3. `kubectl apply -k deploy/dev/` (the dev overlay).

The dev overlay (`deploy/dev/`) runs:

- the **controller** with `--mock --disable-pki-bootstrap`, so it dials forkd over
  insecure gRPC (no control plane CA or TLS Secrets).
- a **forkd** DaemonSet with `--mock` and no TLS flags, using the no-KVM mock fork
  engine. It mounts no `/dev/kvm` and carries no `mitos.run/kvm` nodeSelector,
  so it schedules on the plain kind node.
- a default `SandboxTemplate` + `SandboxPool` named `dev-default` in the `default`
  namespace.

The controller discovers the mock forkd by its `app.kubernetes.io/component:
forkd` pod label, builds the `dev-default` pool snapshot over insecure gRPC, and a
claim forks via the mock engine and reaches `Ready`:

```bash
mitos dev up
mitos sandbox create --pool dev-default   # prints the sandbox id, Ready
mitos sandbox ls
mitos sandbox terminate <id>
mitos dev down
```

The dev manifests reference the `mitos-controller:ci` and `mitos-forkd:ci`
image tags with `imagePullPolicy: IfNotPresent`. Build and load them before
`mitos dev up` (CI does this automatically):

```bash
docker build -f Dockerfile.controller -t mitos-controller:ci .
docker build -f Dockerfile.forkd -t mitos-forkd:ci .
kind load docker-image mitos-controller:ci --name mitos-dev
kind load docker-image mitos-forkd:ci --name mitos-dev
```

## Mock-engine limitation

The dev cluster uses the mock fork engine, which has NO guest VM. A claim
reconciles to `Ready` and the control-plane dispatch works, but a real in-VM
`exec` is not exercised on the dev cluster. To run real sandboxes locally you need
a node with `/dev/kvm` and the production manifests (`deploy/controller/` +
`deploy/daemon/`) with the `mitos.run/kvm=true` node label.

## What is proven

PROVEN in CI:

- command dispatch for `run` and every `sandbox` verb;
- the cluster `SandboxClaim` claim path with token-scoped exec;
- `mitos dev up` orchestration (CRDs + mock controller + mock forkd + pool);
- `sandbox ls` over the control plane;
- on the dev mock cluster on kind: `sandbox create` reaches `Ready`, `sandbox ls`
  shows it, and `sandbox terminate` removes it.

The mock-engine exec limitation above is the one gap: real in-VM `exec` is proven
by the KVM CI of the API, not by the kind dev smoke.

## kubectl-sandbox operator plugin

`kubectl-sandbox` is a separate kubectl plugin for the OPERATOR persona: a
cluster admin who inspects and operates the sandbox objects already in the
cluster. Installed as `kubectl-sandbox` on `PATH`, it is invoked as
`kubectl sandbox <verb>` and reads the cluster connection from the standard
kubeconfig resolution.

```bash
go build -o /usr/local/bin/kubectl-sandbox ./cmd/kubectl-sandbox/
```

```
kubectl sandbox ls   [-n ns] [-A]            list SandboxClaims
kubectl sandbox ps   [name] [-n ns] [-A]     list SandboxForks (or one claim's forks)
kubectl sandbox tree [--pool P] [-n ns] [-A] render the fork/lineage DAG
kubectl sandbox top  [-n ns] [-A]            per-sandbox CoW-aware metering
kubectl sandbox logs <sandbox> [-n ns]       husk stub pod console for a claim
kubectl sandbox exec <sandbox> [-n ns] -- cmd run a command in a sandbox
```

### tree

`tree` walks the lineage DAG: each `SandboxClaim` is a root, and a `SandboxFork`
nests under whatever object its `spec.sourceRef` names (a claim OR another fork,
so a multi-level fork chain nests). Siblings sort by name; an orphan fork whose
source is out of scope is surfaced as its own root rather than dropped.
`--pool <name>` scopes to one pool via a transitive walk over the source refs.

### top

`top` shows per-sandbox CoW-aware metering pulled from each node's forkd
`GET /v1/metering` endpoint (operational data on the same access class as
`/metrics` and `/healthz`). The columns are HONEST about what they mean:

- `UNIQUE-MEM` is the marginal unique (private-dirty) memory a fork actually
  adds. It is NOT `memory.current`.
- `SHARED-MEM` is the shared-once template attribution: the page set every fork
  of a template maps copy-on-write, counted once per template at the node level
  (see `internal/metering`).
- `UNIQUE-DISK` is the backing storage the sandbox alone owns.

A sandbox with no metering datum (no endpoint, an unreachable forkd, or no
matching row) shows a dash in every metered cell, never a zero and never a
fabricated value.

### logs

`logs <sandbox>` prints the husk stub pod console for the claim (the
`mitos.run/husk` pod labeled `mitos.run/claim=<claim>`) via the Kubernetes
pod-logs API, then a one-line guest-console note. On a mock or no-VMM control
plane (kind) there is no husk pod or no live guest, so the stub console is
reported absent and the guest console states it needs a running sandbox: the
guest serial/vsock console streams only from a live VMM (the
[#18](https://github.com/paperclipinc/mitos/issues/18) boundary), not from this
read-only operator path.

### exec

`exec <sandbox> -- <cmd>` runs a command in the sandbox over the forkd HTTP
sandbox API, authenticating with the per-sandbox bearer token read from the
claim's `<claim>-sandbox-token` Secret: the SAME gate the SDK uses, never
bypassing auth. The token value is held only for the request, never logged, and
redacted from any error string. A claim that is not `Ready` (or has no endpoint,
or no token Secret) yields a clear, actionable error rather than a hang. The
in-sandbox command's exit code becomes the plugin's exit code so it chains in
shell pipelines.

On kind the mock engine has no guest VM, so `exec`/`top`/`logs` of a REAL running
sandbox are the KVM/bare-metal tail; the kind-e2e smoke proves `ls`/`ps`/`tree`
at the object level. `cp` and `port-forward` for operators remain the documented
ergonomics longtail.

## Follow-ups

- workspace verbs (`mitos ws log|diff|revert|branch`) pending Workspace
  ([#21](https://github.com/paperclipinc/mitos/issues/21));
- `mitos pool create|refresh` beyond what `dev up` needs;
- streaming exec / PTY (`exec_stream`) pending the Connect protocol
  ([#23](https://github.com/paperclipinc/mitos/issues/23));
- a `curl | sh` installer and `get.mitos.run` distribution
  ([#37](https://github.com/paperclipinc/mitos/issues/37));
- shell completions and a code-interpreter-compatible API shim.
