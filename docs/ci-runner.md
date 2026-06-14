# Bare-metal CI: the in-cluster self-hosted runner (issue #16)

This turns the maintainer's manual cluster verification into standing CI. A
self-hosted GitHub Actions runner runs INSIDE the single-node Talos KVM cluster
and, on every trusted change, drives the REAL-cluster husk end-to-end:

    claim -> activate -> exec -> fork -> run_code -> PTY -> crash-reap

against real KVM on a real Kubernetes cluster. The mock-engine kind e2e and the
KVM Firecracker job already run on GitHub-hosted runners; this is the missing
proof that the WHOLE stack (controller + husk pods + guest agent + the sandbox
HTTP/PTY API) works together on real hardware.

## Architecture

```
GitHub (paperclipinc/mitos, PUBLIC)
  |
  |  trusted trigger only (push to main / workflow_dispatch / labeled same-repo PR)
  v
self-hosted runner pod  (namespace mitos-ci, ServiceAccount mitos-ci-runner)
  |  unprivileged: only kubectls + speaks the sandbox HTTP/PTY API
  |
  |  (1) deploy-under-test: kubectl set image controller + husk-stub arg
  |  (2) run test/cluster-e2e/husk-e2e.sh against namespace mitos-e2e
  v
controller (namespace mitos)  ->  husk pods (namespace mitos-e2e, do the KVM)
  ^                                      |
  |  Python SDK over the pod network ----+  exec / run_code / fork / PTY
```

The runner itself does NOT need docker or KVM. The husk pods do the KVM (they
request the `mitos.run/kvm` device-plugin resource, the production pod-native
path). The runner only drives the cluster with `kubectl` and talks to each
claim's sandbox HTTP API over the in-cluster pod network via the mitos Python
SDK (`in_cluster=True`).

Images are built and pushed to `ghcr.io/paperclipinc/mitos-{controller,husk-stub,...}`
by the existing `docker-build` / publish pipeline on GitHub-hosted
`ubuntu-latest`. This job only DEPLOYS those images; it never builds them on the
runner (building untrusted Dockerfiles on a cluster-attached runner would be a
security hole).

### Files

| File | Purpose |
| --- | --- |
| `deploy/ci-runner/namespace.yaml` | `mitos-ci` namespace (the runner) |
| `deploy/ci-runner/e2e-namespace.yaml` | `mitos-e2e` namespace (the e2e blast radius) |
| `deploy/ci-runner/serviceaccount.yaml` | `mitos-ci-runner` ServiceAccount |
| `deploy/ci-runner/rbac.yaml` | least-privilege Roles + RoleBindings (+ a read-only nodes ClusterRole) |
| `deploy/ci-runner/deployment.yaml` | the ephemeral runner Deployment |
| `deploy/ci-runner/Dockerfile.ci-runner` | thin runner image (official base + kubectl/git/jq/python3/SDK) |
| `deploy/ci-runner/kustomization.yaml` | `kubectl apply -k` base (NO token Secret) |
| `.github/workflows/cluster-e2e.yaml` | the gated CI job |
| `test/cluster-e2e/husk-e2e.sh` | the real-cluster husk verification |

## Security model (why fork PRs are excluded)

The repo `paperclipinc/mitos` is PUBLIC. A self-hosted runner attached to the
cluster is a high-value target: anyone who can run code on it inherits the
runner's KVM-backed husk path and its cluster RBAC. A malicious fork pull
request could, if its code ran here, own the cluster. So the job is gated to
TRUSTED triggers ONLY. There are three independent layers:

1. **Trigger gating (the primary control).** `.github/workflows/cluster-e2e.yaml`
   runs only on:
   - `push` to `main` (already-reviewed, merged code), or
   - `workflow_dispatch` (a maintainer pressing the button), or
   - a `pull_request` that is BOTH from the same repo (NOT a fork) AND carries
     the maintainer-applied `ci-cluster` label.

   The job-level `if:` is:

   ```yaml
   if: >-
     github.event_name == 'push' ||
     github.event_name == 'workflow_dispatch' ||
     (
       github.event_name == 'pull_request' &&
       github.event.pull_request.head.repo.full_name == github.repository &&
       contains(github.event.pull_request.labels.*.name, 'ci-cluster')
     )
   ```

   A fork PR has `head.repo.full_name != github.repository`, so it fails the
   same-repo check; it also cannot carry a maintainer-only label without a
   maintainer's action. Either way the job is skipped. The default safe baseline
   is push-to-main + workflow_dispatch; the labeled-same-repo-PR path is OPT-IN.

2. **The `cluster` GitHub Environment (a second human gate).** The job declares
   `environment: cluster`. Configure that Environment in repo Settings to
   require a maintainer approval before any run proceeds, so even a trusted
   trigger waits for a human.

3. **Least-privilege RBAC (blast-radius containment).** Even if untrusted code
   somehow ran, the runner's ServiceAccount cannot own the cluster. It is bound
   to exactly two namespaces and one read-only cluster verb:
   - `mitos-e2e`: full lifecycle of the e2e objects (templates, pools, claims,
     forks), read/exec/delete pods, read pod logs, read the per-sandbox token
     Secrets (values never logged), read services/endpoints/events.
   - `mitos`: read + patch the controller Deployment (deploy-under-test), read
     ReplicaSets/Pods/Pod logs (rollout + diagnostics), read Secrets presence.
     It CANNOT create or delete Deployments, write Secrets, or touch the
     controller's own RBAC.
   - cluster-scoped: `get/list nodes` ONLY (nodes are cluster-scoped and the
     e2e checks for a KVM-capable node).

   There is deliberately NO `ClusterRole` granting writes and NO `cluster-admin`.

4. **Ephemeral runner.** The runner is registered with `--ephemeral`: it runs
   one job, exits, and the Deployment restarts it, re-registering a fresh
   runner. No job can leave state for the next, and the pod is unprivileged
   (`runAsNonRoot`, all capabilities dropped, no host mounts, no KVM).

### Trade-off note: deploy-under-test in the live `mitos` namespace

The job patches the controller image in the live `mitos` namespace and restores
it on teardown (`if: always()`). This grants the runner `patch`/`update` on the
controller Deployment, which is more than read-only. It is bounded to the
controller Deployment object only (no Secret writes, no create/delete), and the
`cluster` Environment approval + trigger gating keep it maintainer-controlled.
If you prefer zero write access to the production control plane, deploy a
dedicated test controller instance in a separate namespace and re-point the
e2e + the RBAC `mitos-ci-runner-deploy` Role at it; that removes the production
patch grant at the cost of a second controller to maintain.

## One-time maintainer setup

All steps assume your `kubectl` context points at the cluster and you are an
admin of the GitHub repo.

### 1. Build and push the runner image

The runner image is the official `actions-runner` plus `kubectl`, `git`, `jq`,
`python3`, and the mitos Python SDK. Build it on a trusted machine and push to
GHCR:

```bash
# Resolve and pin the base digest first (edit Dockerfile.ci-runner FROM line):
docker buildx imagetools inspect ghcr.io/actions/actions-runner:2.321.0

docker build -f deploy/ci-runner/Dockerfile.ci-runner \
  -t ghcr.io/paperclipinc/mitos-ci-runner:0.1.0 .
docker push ghcr.io/paperclipinc/mitos-ci-runner:0.1.0
```

Then pin `deploy/ci-runner/deployment.yaml`'s `image:` to the pushed digest
(`ghcr.io/paperclipinc/mitos-ci-runner@sha256:...`) so a moving tag cannot swap
the runner binary.

### 2. Create the runner registration Secret

The runner needs either a repo runner-registration token OR a fine-grained PAT.
A fine-grained PAT is recommended because the official image can refresh
short-lived registration tokens from it across ephemeral restarts.

Create a fine-grained PAT at GitHub Settings -> Developer settings ->
Fine-grained tokens, scoped to the `paperclipinc/mitos` repo with:

- Repository permissions: `Administration: Read and write` (required to
  register/remove self-hosted runners), `Actions: Read and write`.

Then create the Secret IN the cluster (the token value lives ONLY here, never in
the repo):

```bash
kubectl create namespace mitos-ci --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret -n mitos-ci generic mitos-ci-runner-token \
  --from-literal=token=ghp_your_fine_grained_pat_here
```

To rotate: re-create the Secret and restart the Deployment
(`kubectl -n mitos-ci rollout restart deployment/mitos-ci-runner`).

### 3. Apply the runner manifests

```bash
kubectl apply -k deploy/ci-runner/
```

This creates the `mitos-ci` and `mitos-e2e` namespaces, the ServiceAccount, the
least-privilege RBAC, and the ephemeral runner Deployment. It does NOT create
the token Secret (you did that in step 2).

### 4. Verify the runner registered

```bash
kubectl -n mitos-ci get pods
kubectl -n mitos-ci logs deployment/mitos-ci-runner
```

Then in the GitHub UI: repo Settings -> Actions -> Runners. A runner named
`mitos-cluster-...` with labels `self-hosted, mitos-cluster, kvm` should appear
as Idle. (Because it is ephemeral, it appears while idle and is replaced after
each job.)

### 5. Configure the `cluster` Environment (recommended)

Repo Settings -> Environments -> New environment -> `cluster`. Add yourself (or
the maintainers) as required reviewers so every cluster e2e run waits for a
human approval.

### 6. Trigger a run

- Automatic: merge to `main`.
- Manual: repo Actions -> "Cluster e2e (self-hosted)" -> Run workflow.

### 7. (Optional) the labeled same-repo PR path

To run the cluster e2e on a same-repo (non-fork) PR before merge, a maintainer
applies the `ci-cluster` label to the PR. Create the label once:

```bash
gh label create ci-cluster \
  --description "Run the self-hosted cluster e2e on this same-repo PR" \
  --color B60205
```

Fork PRs can NEVER use this path: the `if:` requires
`head.repo.full_name == github.repository`, which a fork fails, and a fork
contributor cannot apply a label.

## What the e2e asserts

`test/cluster-e2e/husk-e2e.sh` (run against `mitos-e2e`) checks, with bounded
waits and a cleanup trap, each printing a `PASS:` / `FAIL:` line:

0. a node labeled `mitos.run/kvm=true` is present.
1. a `SandboxPool` warms at least one dormant husk pod.
2. a `SandboxClaim` activates to Ready and `exec("echo ...")` returns the
   expected stdout with exit 0.
3. `fork(2)` produces two independent sandboxes (a marker written in one is not
   visible in the other).
4. `run_code` returns a result, OR a clean `KernelUnavailable` (the husk OCI
   base may lack the code-interpreter kernel; per issue #16 a clean
   `KernelUnavailable` is ACCEPTED as a pass and does NOT fail the suite). A
   non-`KernelUnavailable` kernel error IS a failure.
5. a PTY allocates and echoes its input.
6. (best effort) deleting the claimed husk pod re-pends the claim (the
   eviction / self-heal path). Inconclusive is non-fatal; a clear regression is
   surfaced.

On any failure the script dumps pool/claim/pod state and recent husk pod logs.
Secret VALUES are never logged (CLAUDE.md secrets rule).

## Failure behavior

- The workflow has a 25-minute job timeout, so a hung activation cannot pin the
  cluster.
- Teardown runs `if: always()`: it deletes the e2e pool/claims/forks and
  RESTORES the controller image and args, even if the run failed or was
  cancelled. The e2e script also cleans its own objects via an EXIT trap.
- The runner is ephemeral, so a crashed job leaves no residual runner state; the
  Deployment brings up a fresh one.
- `concurrency: cluster-e2e` with `cancel-in-progress` ensures the single-node
  cluster is never driven by two e2e jobs at once.
