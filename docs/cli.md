# agentrun CLI

`agentrun` is the command-line interface for snapshot-fork sandboxes. It drives
the sandbox lifecycle (create, exec, file IO, fork, terminate, list) against a
Kubernetes cluster, and brings a local kind dev cluster up or down for a
one-command local-dev loop.

```bash
go build -o agentrun ./cmd/agentrun/
```

## Command reference

```
agentrun run <command> [--pool P] [--timeout N]   create a sandbox, run the
                                                  command, terminate, exit with
                                                  the command's exit code
agentrun sandbox create [--pool P]                create a sandbox, print its id
agentrun sandbox ls [-n namespace] [-A]           list sandboxes
agentrun sandbox exec <id> <command...>           run a command in a sandbox
agentrun sandbox fork <id> [--replicas N]         fork a sandbox, print new ids
agentrun sandbox terminate <id>                   destroy a sandbox
agentrun dev up [--skip-cluster-create]           bring a local kind dev cluster
                                                  up with a mock control plane
agentrun dev down                                 delete the local kind dev cluster
```

Global flags `--namespace`/`-n` and `--pool` may appear before the subcommand.
`agentrun run` exits with the executed command's exit code so it chains in shell
pipelines.

## Backends

### Cluster backend (kubeconfig)

For every `run` and `sandbox` verb, `agentrun` resolves a Kubernetes connection
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

`agentrun dev up` brings up a local kind cluster running a MOCK control plane so
the full claim path completes without KVM:

1. `kind create cluster` (tolerating an already-existing cluster; skipped with
   `--skip-cluster-create` to target a cluster you already stood up).
2. `kubectl apply -f deploy/crds/` (the CRDs).
3. `kubectl apply -k deploy/dev/` (the dev overlay).

The dev overlay (`deploy/dev/`) runs:

- the **controller** with `--mock --disable-pki-bootstrap`, so it dials forkd over
  insecure gRPC (no control plane CA or TLS Secrets).
- a **forkd** DaemonSet with `--mock` and no TLS flags, using the no-KVM mock fork
  engine. It mounts no `/dev/kvm` and carries no `agentrun.dev/kvm` nodeSelector,
  so it schedules on the plain kind node.
- a default `SandboxTemplate` + `SandboxPool` named `dev-default` in the `default`
  namespace.

The controller discovers the mock forkd by its `app.kubernetes.io/component:
forkd` pod label, builds the `dev-default` pool snapshot over insecure gRPC, and a
claim forks via the mock engine and reaches `Ready`:

```bash
agentrun dev up
agentrun sandbox create --pool dev-default   # prints the sandbox id, Ready
agentrun sandbox ls
agentrun sandbox terminate <id>
agentrun dev down
```

The dev manifests reference the `agent-run-controller:ci` and `agent-run-forkd:ci`
image tags with `imagePullPolicy: IfNotPresent`. Build and load them before
`agentrun dev up` (CI does this automatically):

```bash
docker build -f Dockerfile.controller -t agent-run-controller:ci .
docker build -f Dockerfile.forkd -t agent-run-forkd:ci .
kind load docker-image agent-run-controller:ci --name agentrun-dev
kind load docker-image agent-run-forkd:ci --name agentrun-dev
```

## Mock-engine limitation

The dev cluster uses the mock fork engine, which has NO guest VM. A claim
reconciles to `Ready` and the control-plane dispatch works, but a real in-VM
`exec` is not exercised on the dev cluster. To run real sandboxes locally you need
a node with `/dev/kvm` and the production manifests (`deploy/controller/` +
`deploy/daemon/`) with the `agentrun.dev/kvm=true` node label.

## What is proven

PROVEN in CI:

- command dispatch for `run` and every `sandbox` verb;
- the cluster `SandboxClaim` claim path with token-scoped exec;
- `agentrun dev up` orchestration (CRDs + mock controller + mock forkd + pool);
- `sandbox ls` over the control plane;
- on the dev mock cluster on kind: `sandbox create` reaches `Ready`, `sandbox ls`
  shows it, and `sandbox terminate` removes it.

The mock-engine exec limitation above is the one gap: real in-VM `exec` is proven
by the KVM CI of the API, not by the kind dev smoke.

## Follow-ups

- workspace verbs (`agentrun ws log|diff|revert|branch`) pending Workspace
  ([#21](https://github.com/paperclipinc/sandbox/issues/21));
- `agentrun pool create|refresh` beyond what `dev up` needs;
- streaming exec / PTY (`exec_stream`) pending the Connect protocol
  ([#23](https://github.com/paperclipinc/sandbox/issues/23));
- a `curl | sh` installer and `get.agentrun.dev` distribution
  ([#37](https://github.com/paperclipinc/sandbox/issues/37));
- shell completions and a code-interpreter-compatible API shim.
