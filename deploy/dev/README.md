# Dev overlay: mock control plane for `mitos dev up`

This directory is the local-dev control plane that `mitos dev up` applies to a
kind cluster. It runs a MOCK-mode control plane so the full claim path completes
without KVM:

- **controller** Deployment with `--mock --disable-pki-bootstrap`. `--mock` is a
  no-op on the controller today (mock mode lives in forkd), but it documents the
  intent; `--disable-pki-bootstrap` skips the control plane CA and TLS Secrets so
  the controller dials forkd over insecure gRPC.
- **forkd** DaemonSet with `--mock` and no TLS flags. It uses the no-KVM mock fork
  engine (`internal/fork/mock.go`), mounts no `/dev/kvm`, and carries no
  `mitos.run/kvm` nodeSelector so it schedules on the plain kind node. The
  controller discovers it by the `app.kubernetes.io/component: forkd` pod label,
  builds the pool snapshot over insecure gRPC, and claims fork via the mock engine
  and reach Ready.
- a default `SandboxTemplate` + `SandboxPool` (`dev-default`) in the `default`
  namespace so `mitos sandbox create --pool dev-default` has a pool to claim
  from. Pools, templates, and claims are namespaced and looked up in the claim's
  namespace; the CLI creates claims in `default`, so the dev pool lives there too.
  The control plane itself runs in the `mitos` namespace (the forkd discovery
  default).

## Mock-engine limitation

The mock engine has no guest VM, so a real in-VM `exec` is NOT exercised here.
The dev cluster proves the control-plane dispatch and the claim/fork path
(claim -> Ready) only. Real exec is proven by the KVM CI of the API. To run real
sandboxes locally you need a node with `/dev/kvm` and the production manifests
(`deploy/controller/`, `deploy/daemon/`) plus a KVM nodeSelector label.

## Images

The controller and forkd Deployments reference `mitos-controller:ci` and
`mitos-forkd:ci` with `imagePullPolicy: IfNotPresent`. These are the tags the
kind CI builds and `kind load docker-image`s into the cluster. For local use,
build and load the same tags before `mitos dev up`:

```bash
docker build -f Dockerfile.controller -t mitos-controller:ci .
docker build -f Dockerfile.forkd -t mitos-forkd:ci .
kind load docker-image mitos-controller:ci --name mitos-dev
kind load docker-image mitos-forkd:ci --name mitos-dev
```

## Apply

```bash
kubectl apply -f deploy/crds/
kubectl apply -k deploy/dev/
```

The CRDs are applied separately because kustomize refuses to reference files
outside `deploy/dev/` under its default load restrictor. `mitos dev up` runs
both steps for you.
