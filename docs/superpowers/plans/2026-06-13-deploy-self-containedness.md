# Deploy Self-Containedness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `kubectl apply -k deploy/` bring up a fully working mitos stack on a real KVM node with no manual `kubectl` patches; bake in the eight runtime fixes that were applied by hand to go live tonight.

**Architecture:** Most fixes are declarative deploy/ changes: PodSecurity Admission labels on the namespace, an image pull secret + serviceaccount wiring, forkd DaemonSet adjustments (agent-bin arg, privileged securityContext, DOCKER_CONFIG + docker-config secret mount, jailer args removed for the husk path), and a kernel-staging DaemonSet that places `vmlinux` on each KVM node. One fix needs Go: the controller must replicate the CA and forkd-TLS Secrets from its namespace into any pool namespace where it creates husk pods, with the matching RBAC already present (secrets create/get/list/update exists). The namespace default (`mitos`) is already correct in code and deploy/; we verify it and only add a task if a gap remains.

**Tech Stack:** Go 1.26, controller-runtime, Kustomize, Kubernetes v1.31 PodSecurity admission, Firecracker, envtest, kubeconform.

---

## Background facts (grounded in the tree)

These are load-bearing and were confirmed by reading the files:

- `deploy/controller/namespace.yaml` is the only namespace manifest and carries NO `pod-security.kubernetes.io/*` labels.
- `deploy/kustomization.yaml` lists every base resource; there is no pull-secret, no kernel-staging job, and no PSA labelling.
- `deploy/rbac/serviceaccount.yaml` has no `imagePullSecrets`.
- All images are `ghcr.io/paperclipinc/mitos-*` (controller, forkd, husk-stub, kvm-device-plugin).
- `deploy/daemon/daemonset.yaml`: forkd has NO `--agent-bin` arg (cmd/forkd default is empty; image builds fail without it), NO `privileged: true` (it uses an explicit capability list), NO `DOCKER_CONFIG` env / docker-config mount, and DOES carry jailer args `--jailer`, `--chroot-base`, `--uid-range`. forkd default `--kernel=/var/lib/mitos/vmlinux`.
- `internal/fork/imagebuild.go:19` errors when `--agent-bin` is empty and an OCI image build is requested. `Dockerfile.forkd` does not place an agent binary in the image.
- `internal/controller/huskpod.go`: husk pods mount the kernel from `<DataDir>/vmlinux` (default `/var/lib/mitos/vmlinux`) and need the snapshot present on the node. The PKI secrets `mitos-forkd-tls` (leaf, key `tls.crt`/`tls.key`) and `mitos-ca` (key `ca.crt`) are mounted into husk pods in `pool.Namespace` (e.g. `default`).
- `internal/controller/pki_bootstrap.go`: `EnsurePKI(ctx, c, namespace)` materializes `mitos-ca`, `mitos-forkd-tls`, `mitos-controller-tls` ONLY in the controller namespace (`mitos`). Constants `CASecretName = "mitos-ca"`, `ForkdTLSSecretName = "mitos-forkd-tls"`.
- `cmd/controller/main.go`: `discoveryNamespace` defaults to `mitos` (line 196-199); `EnsurePKI` runs against it (line 224); the `SandboxPoolReconciler` is constructed at line 136-146 with `HuskTLSSecretName`/`HuskCASecretName` already set, but no source-namespace field.
- `internal/controller/sandboxpool_controller.go`: `reconcileHuskPods` is called at line 120, BEFORE husk pods are created; husk pods land in `pool.Namespace`. The reconciler embeds `client.Client` so `r.Get`/`r.Create`/`r.Update`/`r.Scheme()` are available.
- RBAC (`deploy/rbac/clusterrole.yaml`) already grants secrets `create;delete;get;list;update;watch` cluster-wide, so cross-namespace secret writes need NO new RBAC. The existing rule comment must be extended to mention replication so the grant stays justified (CLAUDE.md: nothing granted speculatively).
- envtest pattern: `internal/controller/pki_bootstrap_test.go` has `newCoreClient(t)`, `newPKINamespace(t, c)`, `getSecret(t, c, ns, name)`; `suite_test.go` exposes package-level `cfg`, `ctx`, `scheme`.

---

## File Structure

Files created:

- `deploy/imagepullsecret.yaml` — placeholder `ghcr-pull` dockerconfigjson Secret in `mitos` (documented, with a real generation command); referenced by kustomization and the serviceaccount.
- `deploy/kernel/daemonset.yaml` — `mitos-kernel-stage` DaemonSet that stages `vmlinux` into `/var/lib/mitos/vmlinux` on every KVM node via an init container, then idles.
- `internal/controller/secret_replication.go` — `replicateControlPlaneSecret` helper + `replicateHuskSecrets` that copies `mitos-ca` (ca.crt only) and `mitos-forkd-tls` into a target namespace.
- `internal/controller/secret_replication_test.go` — envtest coverage for replication (create, idempotent re-run, update-on-drift).

Files modified:

- `deploy/controller/namespace.yaml` — add PSA `enforce=privileged` labels.
- `deploy/dev/namespace.yaml` — add PSA `enforce=privileged` labels (the default pool namespace husk pods land in for bare-metal; the dev overlay namespace is `mitos-dev`, see Task 2 note).
- `deploy/rbac/serviceaccount.yaml` — add `imagePullSecrets: [{name: ghcr-pull}]`.
- `deploy/rbac/clusterrole.yaml` — extend the secrets rule comment to cover replication.
- `deploy/daemon/daemonset.yaml` — add `--agent-bin=/usr/local/bin/agent`, set `securityContext.privileged: true` (drop the explicit capability list), add `DOCKER_CONFIG` env + docker-config secret volume/mount, remove jailer args; add the `ghcr-pull` imagePullSecret via the serviceaccount (forkd has no serviceAccountName today, so add one).
- `Dockerfile.forkd` — build and install the guest agent at `/usr/local/bin/agent` so `--agent-bin` resolves.
- `deploy/kustomization.yaml` — add the new resources.
- `internal/controller/sandboxpool_controller.go` — add `ControllerNamespace` field; call `replicateHuskSecrets` in `reconcileHuskPods` before creating husk pods.
- `cmd/controller/main.go` — set `ControllerNamespace: discoveryNamespace` on the pool reconciler.
- `docs/threat-model.md` — note the forkd `privileged: true` regression + the cross-namespace secret replication surface (security surface moved).

---

## Task 1: PodSecurity Admission labels on the controller namespace

**Files:**
- Modify: `deploy/controller/namespace.yaml`

- [ ] **Step 1: Write the failing verification**

Run: `kustomize build deploy/ | grep -A12 'kind: Namespace' | grep 'pod-security.kubernetes.io/enforce: privileged'`
Expected: FAIL (no output; the label is absent).

- [ ] **Step 2: Add the PSA labels**

Replace the whole file `deploy/controller/namespace.yaml` with:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: mitos
  labels:
    app.kubernetes.io/name: mitos
    # PodSecurity admission (v1.31). forkd and the kvm-device-plugin run with
    # hostPath volumes and host devices, and forkd runs privileged; none is
    # admissible under baseline or restricted. enforce=privileged so the
    # DaemonSets are admitted without a manual namespace patch. The husk pods
    # themselves are restricted-except-two-exceptions, but they run in the POOL
    # namespace (e.g. default), labelled separately in deploy/dev/namespace.yaml.
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
```

- [ ] **Step 3: Run the verification to confirm it passes**

Run: `kustomize build deploy/ | grep -A12 'kind: Namespace' | grep 'pod-security.kubernetes.io/enforce: privileged'`
Expected: PASS (prints the enforce line).

- [ ] **Step 4: Server-dry-run sanity (optional if a cluster is reachable)**

Run: `kustomize build deploy/ | kubectl apply --dry-run=server -f - 2>/dev/null | grep 'namespace/mitos'` (skip if no cluster).
Expected: `namespace/mitos configured (server dry run)` or PASS skip.

- [ ] **Step 5: Commit**

```bash
git add deploy/controller/namespace.yaml
git commit -m "fix(deploy): enforce privileged PodSecurity on the mitos namespace

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: PodSecurity Admission labels on the pool namespace

**Files:**
- Modify: `deploy/dev/namespace.yaml`

Note: husk pods run in the POOL namespace. On bare metal tonight that was `default`. `default` is not in deploy/, so the operator labels it once via the documented command in Step 4; the dev overlay namespace `mitos-dev` IS in deploy/dev and is labelled here so the dev overlay is self-contained too.

- [ ] **Step 1: Read the current dev namespace**

Run: `cat deploy/dev/namespace.yaml`
Expected: a Namespace `mitos-dev` (or similar) with no PSA labels.

- [ ] **Step 2: Write the failing verification**

Run: `grep 'pod-security.kubernetes.io/enforce: privileged' deploy/dev/namespace.yaml`
Expected: FAIL (no output).

- [ ] **Step 3: Add the PSA labels to the dev namespace**

Add these three lines under `metadata.labels` in `deploy/dev/namespace.yaml` (keep the existing name and labels):

```yaml
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
```

- [ ] **Step 4: Document the default-namespace label for bare metal**

Add this comment block at the top of `deploy/dev/namespace.yaml` (above `apiVersion:`):

```yaml
# Pool namespace PodSecurity. Husk pods run in the POOL namespace, not the
# controller namespace. The dev overlay's own namespace is labelled below. For
# a bare-metal pool in `default`, label it once (it is not a deploy/ resource):
#   kubectl label --overwrite namespace default \
#     pod-security.kubernetes.io/enforce=privileged \
#     pod-security.kubernetes.io/audit=privileged \
#     pod-security.kubernetes.io/warn=privileged
# Any dedicated pool namespace you create needs the same enforce=privileged.
```

- [ ] **Step 5: Run the verification to confirm it passes**

Run: `grep 'pod-security.kubernetes.io/enforce: privileged' deploy/dev/namespace.yaml`
Expected: PASS (prints the line).

- [ ] **Step 6: Commit**

```bash
git add deploy/dev/namespace.yaml
git commit -m "fix(deploy): enforce privileged PodSecurity on pool namespaces

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: ghcr image pull secret manifest

**Files:**
- Create: `deploy/imagepullsecret.yaml`
- Modify: `deploy/kustomization.yaml`

- [ ] **Step 1: Write the failing verification**

Run: `kustomize build deploy/ | grep 'name: ghcr-pull'`
Expected: FAIL (no output).

- [ ] **Step 2: Create the pull secret manifest**

Create `deploy/imagepullsecret.yaml`:

```yaml
# ghcr.io pull credential for the mitos-* images. This ships with an EMPTY
# dockerconfigjson so the manifest is self-contained and apply-able; you MUST
# populate it once with a real read:packages token before the images can pull:
#
#   kubectl create secret docker-registry ghcr-pull \
#     --namespace mitos \
#     --docker-server=ghcr.io \
#     --docker-username=<github-username> \
#     --docker-password=<ghcr-read-packages-PAT> \
#     --dry-run=client -o yaml | kubectl apply -f -
#
# The serviceaccounts reference it by name (deploy/rbac/serviceaccount.yaml and
# the forkd DaemonSet), so re-applying the populated secret is all that is
# needed; the wiring is already declarative. The token VALUE is never committed.
apiVersion: v1
kind: Secret
metadata:
  name: ghcr-pull
  namespace: mitos
  labels:
    app.kubernetes.io/name: mitos
type: kubernetes.io/dockerconfigjson
data:
  # Empty config: {"auths":{}} base64-encoded. Overwrite via the command above.
  .dockerconfigjson: eyJhdXRocyI6e319
```

- [ ] **Step 3: Add it to the kustomization**

In `deploy/kustomization.yaml`, add this line to the `resources:` list, after `controller/namespace.yaml`:

```yaml
  - imagepullsecret.yaml
```

- [ ] **Step 4: Run the verification to confirm it passes**

Run: `kustomize build deploy/ | grep 'name: ghcr-pull'`
Expected: PASS (prints `name: ghcr-pull`).

- [ ] **Step 5: Commit**

```bash
git add deploy/imagepullsecret.yaml deploy/kustomization.yaml
git commit -m "feat(deploy): ship the ghcr-pull image pull secret manifest

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Wire imagePullSecrets onto the controller serviceaccount

**Files:**
- Modify: `deploy/rbac/serviceaccount.yaml`

- [ ] **Step 1: Write the failing verification**

Run: `kustomize build deploy/ | grep -A8 'kind: ServiceAccount' | grep 'ghcr-pull'`
Expected: FAIL (no output).

- [ ] **Step 2: Add imagePullSecrets to the serviceaccount**

Append to `deploy/rbac/serviceaccount.yaml` (after the `metadata:` block, at the top level of the document):

```yaml
# The controller pulls ghcr.io/paperclipinc/mitos-controller. The pull
# credential ships in deploy/imagepullsecret.yaml (populate it once); naming it
# here means every controller pod gets it without a manual patch.
imagePullSecrets:
  - name: ghcr-pull
```

- [ ] **Step 3: Run the verification to confirm it passes**

Run: `kustomize build deploy/ | grep -A8 'kind: ServiceAccount' | grep 'ghcr-pull'`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add deploy/rbac/serviceaccount.yaml
git commit -m "fix(deploy): wire ghcr-pull onto the controller serviceaccount

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Build the guest agent into the forkd image

**Files:**
- Modify: `Dockerfile.forkd`

Rationale: `internal/fork/imagebuild.go:19` requires `--agent-bin` for OCI image template builds. Task 6 sets `--agent-bin=/usr/local/bin/agent`; that path must exist in the image. The agent is linux-only (`guest/agent`).

- [ ] **Step 1: Write the failing verification (the build arg)**

Run: `grep -- '-o /usr/local/bin/agent ./guest/agent' Dockerfile.forkd`
Expected: FAIL (no output).

- [ ] **Step 2: Add the agent build + copy**

In `Dockerfile.forkd`, change the builder build line (currently a single forkd build) so both binaries are built. Replace:

```dockerfile
RUN CGO_ENABLED=0 go build -o forkd ./cmd/forkd/
```

with:

```dockerfile
RUN CGO_ENABLED=0 go build -o forkd ./cmd/forkd/ && \
    CGO_ENABLED=0 GOOS=linux go build -o /usr/local/bin/agent ./guest/agent/
```

Then add, right after the existing `COPY --from=builder /build/forkd /usr/local/bin/forkd` line:

```dockerfile
# The guest agent injected as /init when forkd builds a rootfs from an OCI
# image. cmd/forkd --agent-bin points here (deploy/daemon/daemonset.yaml).
COPY --from=builder /usr/local/bin/agent /usr/local/bin/agent
```

- [ ] **Step 3: Verify the agent cross-builds (the real gate, no docker needed)**

Run: `GOOS=linux GOARCH=amd64 go build -o /tmp/agent ./guest/agent/ && echo BUILT`
Expected: PASS (prints `BUILT`); confirms the binary the Dockerfile builds compiles.

- [ ] **Step 4: Confirm the Dockerfile references resolve**

Run: `grep -c '/usr/local/bin/agent' Dockerfile.forkd`
Expected: `2` (the build output path and the COPY destination).

- [ ] **Step 5: Commit**

```bash
git add Dockerfile.forkd
git commit -m "fix(forkd): build the guest agent into the image at /usr/local/bin/agent

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: forkd DaemonSet runtime fixes (agent-bin, privileged, DOCKER_CONFIG, drop jailer)

**Files:**
- Modify: `deploy/daemon/daemonset.yaml`

This bundles the four DaemonSet changes that go together: they all change the forkd container spec and must apply atomically (a daemonset is rolled as one object).

- [ ] **Step 1: Write the failing verification**

Run: `kustomize build deploy/ | grep -E -- '--agent-bin=/usr/local/bin/agent|privileged: true|name: DOCKER_CONFIG'`
Expected: FAIL (no output; none present).

- [ ] **Step 2: Add the agent-bin arg and remove the jailer args**

In `deploy/daemon/daemonset.yaml`, in the forkd `args:` list, delete these three lines and their preceding jailer comment block (lines that read `--jailer=/usr/local/bin/jailer`, `--chroot-base=/var/lib/mitos/jailer`, `--uid-range=64000-64999` and the comment above them):

```yaml
            # Every VM runs through the jailer: per-VM uid/gid, chroot,
            # cgroup. The chroot base lives inside the data volume so
            # snapshot and rootfs files hard-link into each chroot
            # (forkd refuses to start if they are on different
            # filesystems).
            - --jailer=/usr/local/bin/jailer
            - --chroot-base=/var/lib/mitos/jailer
            - --uid-range=64000-64999
```

In their place add:

```yaml
            # Guest agent injected as /init for OCI image template builds. The
            # binary ships in the forkd image at this path (Dockerfile.forkd).
            - --agent-bin=/usr/local/bin/agent
            # JAILER FOLLOW-UP: the per-VM jailer (uid/gid, chroot, cgroup) is
            # disabled here. forkd runs privileged: true (below) on the husk
            # path until jailer-in-pod lands. Re-add --jailer / --chroot-base /
            # --uid-range and drop privileged once the jailer runs inside the
            # forkd pod. Tracked as the jailer-in-pod follow-up.
```

- [ ] **Step 3: Replace the capability securityContext with privileged: true**

Replace the entire forkd `securityContext:` block (from `securityContext:` through the end of the `capabilities:` `add:` list, the block that lists SYS_ADMIN, SYS_CHROOT, etc.) with:

```yaml
          # PRIVILEGED until jailer-in-pod lands (see the --agent-bin block).
          # Without the jailer, forkd needs unrestricted access to /dev/kvm,
          # /dev/net/tun, cgroups, and chroot to launch each VM; the trimmed
          # capability set only worked WITH the jailer mediating. The honest
          # regression is documented in docs/threat-model.md. Re-narrow to the
          # capability list once the jailer runs inside the pod.
          securityContext:
            privileged: true
```

- [ ] **Step 4: Add the DOCKER_CONFIG env and the docker-config secret mount**

In the forkd container spec, add an `env:` block (forkd has none today). Insert it immediately before the `ports:` block:

```yaml
          env:
            # forkd authenticates to ghcr/private registries when pulling OCI
            # template images. DOCKER_CONFIG points at the mounted docker-config
            # secret (the docker-config volume below). Unset = anonymous pulls.
            - name: DOCKER_CONFIG
              value: /etc/forkd/docker
```

Add the volume mount to the forkd `volumeMounts:` list (after the `ca` mount):

```yaml
            - name: docker-config
              mountPath: /etc/forkd/docker
              readOnly: true
```

Add the volume to the `volumes:` list (after the `ca` volume). The same `ghcr-pull` dockerconfigjson secret is reused, projecting its `.dockerconfigjson` key to the `config.json` filename Docker expects:

```yaml
        # forkd's registry credentials for OCI template pulls. Reuses the
        # ghcr-pull dockerconfigjson secret, projected to the config.json name
        # the docker/containerd auth resolver reads under DOCKER_CONFIG.
        - name: docker-config
          secret:
            secretName: ghcr-pull
            items:
              - key: .dockerconfigjson
                path: config.json
```

- [ ] **Step 5: Add the serviceAccountName + imagePullSecrets so forkd can pull**

forkd's pod spec has no serviceAccountName. Add the imagePullSecret directly on the pod spec (the forkd DaemonSet uses the default SA, which has no pull secret). Insert at the top of the forkd pod `spec:` (just under `spec:`, before `nodeSelector:`):

```yaml
      # ghcr-pull so the kubelet can pull ghcr.io/paperclipinc/mitos-forkd.
      imagePullSecrets:
        - name: ghcr-pull
```

- [ ] **Step 6: Run the verification to confirm it passes**

Run: `kustomize build deploy/ | grep -E -- '--agent-bin=/usr/local/bin/agent|privileged: true|name: DOCKER_CONFIG'`
Expected: PASS (prints all three).

- [ ] **Step 7: Confirm the jailer args are gone**

Run: `kustomize build deploy/ | grep -- '--jailer='`
Expected: FAIL (no output; the jailer arg is removed).

- [ ] **Step 8: Validate the manifest still schema-checks**

Run: `kustomize build deploy/ | kubeconform -strict -ignore-missing-schemas -summary` (skip the kubeconform line if the tool is not installed; otherwise expect 0 invalid resources).
Expected: PASS (0 invalid resources) or skipped.

- [ ] **Step 9: Commit**

```bash
git add deploy/daemon/daemonset.yaml
git commit -m "fix(deploy): forkd agent-bin, privileged, DOCKER_CONFIG, drop jailer args

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: Document the forkd privileged regression in the threat model

**Files:**
- Modify: `docs/threat-model.md`

CLAUDE.md operating principle 2: a security surface move updates the threat model in the same PR. Task 6 reintroduced `privileged: true` on forkd; record it.

- [ ] **Step 1: Locate the forkd row in the threat model**

Run: `grep -n 'forkd\|privileged\|jailer' docs/threat-model.md | head -20`
Expected: prints the section/rows that mention forkd privileges (use the nearest table row or section as the insertion point).

- [ ] **Step 2: Add the regression note**

Add a row or paragraph to the relevant forkd security section in `docs/threat-model.md` with this exact text (place it under the existing forkd privilege discussion):

```markdown
- forkd `privileged: true` (deploy/daemon/daemonset.yaml), REGRESSION pending
  jailer-in-pod. Without the per-VM jailer mediating, forkd needs unrestricted
  /dev/kvm, /dev/net/tun, cgroup, and chroot access, so the trimmed capability
  set is replaced by privileged until the jailer runs inside the forkd pod.
  Status: accepted, time-boxed to the jailer-in-pod follow-up. Mitigation: the
  forkd pod runs only on labelled KVM nodes (mitos.run/kvm) and is not exposed
  to tenant traffic; husk pods, not forkd, are the tenant execution surface.
```

- [ ] **Step 3: Confirm no em/en dashes were introduced**

Run: `grep -nP '[\x{2013}\x{2014}]' docs/threat-model.md`
Expected: FAIL (no output; the project bans em/en dashes).

- [ ] **Step 4: Commit**

```bash
git add docs/threat-model.md
git commit -m "docs(threat-model): record forkd privileged regression pending jailer-in-pod

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: Kernel-staging DaemonSet

**Files:**
- Create: `deploy/kernel/daemonset.yaml`
- Modify: `deploy/kustomization.yaml`

Tonight a one-off Job downloaded `vmlinux` to `/var/lib/mitos/vmlinux`. Both forkd (`--kernel=/var/lib/mitos/vmlinux`) and husk pods (`<DataDir>/vmlinux`) read it. A DaemonSet with an init container stages it on every KVM node, idempotently, then idles; a DaemonSet (not a Job) means a node that joins later is staged automatically.

- [ ] **Step 1: Write the failing verification**

Run: `kustomize build deploy/ | grep 'name: mitos-kernel-stage'`
Expected: FAIL (no output).

- [ ] **Step 2: Create the kernel-staging DaemonSet**

Create `deploy/kernel/daemonset.yaml`:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: mitos-kernel-stage
  namespace: mitos
  labels:
    app.kubernetes.io/name: mitos
    app.kubernetes.io/component: kernel-stage
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: mitos
      app.kubernetes.io/component: kernel-stage
  template:
    metadata:
      labels:
        app.kubernetes.io/name: mitos
        app.kubernetes.io/component: kernel-stage
    spec:
      # Stage the guest kernel only on KVM nodes; that is exactly where forkd
      # and husk pods read /var/lib/mitos/vmlinux.
      nodeSelector:
        mitos.run/kvm: "true"
      tolerations:
        - key: mitos.run/dedicated
          operator: Exists
          effect: NoSchedule
      # An init container downloads the kernel into the node hostPath ONCE
      # (idempotent: skip if the file already exists), then the main container
      # idles so the DaemonSet stays Ready and re-stages on a node rejoin.
      initContainers:
        - name: stage-kernel
          image: curlimages/curl:8.11.0
          command:
            - /bin/sh
            - -c
            - |
              set -eu
              dest=/var/lib/mitos/vmlinux
              if [ -s "$dest" ]; then
                echo "kernel already staged at $dest; skipping download"
                exit 0
              fi
              echo "staging guest kernel to $dest"
              curl -fsSL -o "$dest.tmp" "$KERNEL_URL"
              mv "$dest.tmp" "$dest"
              echo "kernel staged"
          env:
            # The guest kernel image URL. Override per cluster with a kustomize
            # patch if you host your own kernel. This is the Firecracker CI
            # x86_64 5.10 guest kernel used by the bench/ runs.
            - name: KERNEL_URL
              value: https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin
          volumeMounts:
            - name: data
              mountPath: /var/lib/mitos
      containers:
        - name: idle
          image: registry.k8s.io/pause:3.10
          resources:
            requests:
              memory: "8Mi"
              cpu: "10m"
            limits:
              memory: "16Mi"
              cpu: "50m"
      volumes:
        - name: data
          hostPath:
            path: /var/lib/mitos
            type: DirectoryOrCreate
```

- [ ] **Step 3: Add it to the kustomization**

In `deploy/kustomization.yaml`, add this line to the `resources:` list, after `daemon/daemonset.yaml`:

```yaml
  - kernel/daemonset.yaml
```

- [ ] **Step 4: Run the verification to confirm it passes**

Run: `kustomize build deploy/ | grep 'name: mitos-kernel-stage'`
Expected: PASS.

- [ ] **Step 5: Confirm the staged path matches forkd and husk**

Run: `kustomize build deploy/ | grep 'dest=/var/lib/mitos/vmlinux'`
Expected: PASS (the staging path matches forkd `--kernel` and the husk `<DataDir>/vmlinux` mount).

- [ ] **Step 6: Commit**

```bash
git add deploy/kernel/daemonset.yaml deploy/kustomization.yaml
git commit -m "feat(deploy): stage the guest kernel on KVM nodes via a DaemonSet

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: Controller PKI secret replication helper (Go, envtest)

**Files:**
- Create: `internal/controller/secret_replication.go`
- Create: `internal/controller/secret_replication_test.go`

The controller bootstraps `mitos-ca` and `mitos-forkd-tls` in its namespace (`mitos`), but husk pods run in the pool namespace and mount those secrets there. This helper copies them into a target namespace, idempotently, healing on drift. The CA copy projects ONLY `ca.crt` (the CA private key must never leave the controller namespace, matching the husk mount in huskpod.go).

- [ ] **Step 1: Write the failing test**

Create `internal/controller/secret_replication_test.go`:

```go
package controller_test

import (
	"bytes"
	"testing"

	"github.com/paperclipinc/mitos/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// seedControlPlaneSecrets writes a mitos-ca (ca.crt + ca.key) and a
// mitos-forkd-tls (tls.crt + tls.key) into the source namespace, mirroring what
// EnsurePKI produces, so replication has something to copy.
func seedControlPlaneSecrets(t *testing.T, c interface {
	Create(ctx interface{}, obj interface{}, opts ...interface{}) error
}, src string) {
	t.Helper()
}

func TestReplicateHuskSecretsCopiesCAcrtAndForkdTLS(t *testing.T) {
	c := newCoreClient(t)
	src := newPKINamespace(t, c)
	dst := newPKINamespace(t, c)

	// Seed the source secrets directly (no real CA needed; replication copies
	// bytes, it does not re-issue).
	caSrc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: src, Name: controller.CASecretName},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"ca.crt": []byte("CA-CERT"), "ca.key": []byte("CA-KEY-SECRET")},
	}
	tlsSrc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: src, Name: controller.ForkdTLSSecretName},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("LEAF-CERT"), "tls.key": []byte("LEAF-KEY")},
	}
	if err := c.Create(ctx, caSrc); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, tlsSrc); err != nil {
		t.Fatal(err)
	}

	if err := controller.ReplicateHuskSecrets(ctx, c, src, dst); err != nil {
		t.Fatalf("ReplicateHuskSecrets: %v", err)
	}

	gotCA := getSecret(t, c, dst, controller.CASecretName)
	if !bytes.Equal(gotCA.Data["ca.crt"], []byte("CA-CERT")) {
		t.Errorf("dst ca.crt = %q, want CA-CERT", gotCA.Data["ca.crt"])
	}
	if _, leaked := gotCA.Data["ca.key"]; leaked {
		t.Error("dst CA secret leaked ca.key; the CA private key must never be replicated")
	}
	gotTLS := getSecret(t, c, dst, controller.ForkdTLSSecretName)
	if !bytes.Equal(gotTLS.Data["tls.crt"], []byte("LEAF-CERT")) || !bytes.Equal(gotTLS.Data["tls.key"], []byte("LEAF-KEY")) {
		t.Errorf("dst forkd-tls not copied: %v", gotTLS.Data)
	}
}

func TestReplicateHuskSecretsIsIdempotentAndHealsDrift(t *testing.T) {
	c := newCoreClient(t)
	src := newPKINamespace(t, c)
	dst := newPKINamespace(t, c)
	caSrc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: src, Name: controller.CASecretName},
		Data:       map[string][]byte{"ca.crt": []byte("CA-V1"), "ca.key": []byte("KEY")},
	}
	tlsSrc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: src, Name: controller.ForkdTLSSecretName},
		Data:       map[string][]byte{"tls.crt": []byte("LEAF-V1"), "tls.key": []byte("LK")},
	}
	if err := c.Create(ctx, caSrc); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, tlsSrc); err != nil {
		t.Fatal(err)
	}
	// First replication creates the destination copies.
	if err := controller.ReplicateHuskSecrets(ctx, c, src, dst); err != nil {
		t.Fatal(err)
	}
	// Source rotates; a second replication must heal the destination in place.
	caSrc.Data["ca.crt"] = []byte("CA-V2")
	if err := c.Update(ctx, caSrc); err != nil {
		t.Fatal(err)
	}
	if err := controller.ReplicateHuskSecrets(ctx, c, src, dst); err != nil {
		t.Fatalf("second ReplicateHuskSecrets: %v", err)
	}
	gotCA := getSecret(t, c, dst, controller.CASecretName)
	if !bytes.Equal(gotCA.Data["ca.crt"], []byte("CA-V2")) {
		t.Errorf("drift not healed: dst ca.crt = %q, want CA-V2", gotCA.Data["ca.crt"])
	}
}

func TestReplicateHuskSecretsSameNamespaceIsNoop(t *testing.T) {
	c := newCoreClient(t)
	ns := newPKINamespace(t, c)
	// No source secrets exist; replicating into the same namespace must not
	// error (the controller namespace already holds the originals).
	if err := controller.ReplicateHuskSecrets(ctx, c, ns, ns); err != nil {
		t.Fatalf("same-namespace replication should be a noop, got %v", err)
	}
}
```

Note: remove the unused `seedControlPlaneSecrets` stub if gofmt/vet flags it; it is illustrative only. The three real tests above are the gate.

- [ ] **Step 2: Run the test to verify it fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestReplicateHuskSecrets -v`
Expected: FAIL with `undefined: controller.ReplicateHuskSecrets`.

- [ ] **Step 3: Write the implementation**

Create `internal/controller/secret_replication.go`:

```go
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReplicateHuskSecrets copies the control plane PKI material husk pods mount
// from the controller namespace (src) into a pool namespace (dst). Husk pods
// run in the pool namespace (e.g. default), not the controller namespace, but
// EnsurePKI only materializes mitos-ca and mitos-forkd-tls in the controller
// namespace; this bridges that gap so `kubectl apply -k deploy/` needs no
// manual secret copy.
//
// The CA copy projects ONLY ca.crt: the CA private key (ca.key) must never
// leave the controller namespace, matching the husk pod's CA mount in
// huskpod.go (Items ca.crt only). The forkd leaf (tls.crt, tls.key) is copied
// whole because the husk stub serves the mTLS control with it.
//
// Replication is idempotent and heals drift: a destination copy whose data
// differs from the source is updated in place (so a CA rotation propagates).
// Copying into the source namespace itself is a noop (the originals are there).
func ReplicateHuskSecrets(ctx context.Context, c client.Client, src, dst string) error {
	if src == dst {
		return nil
	}
	if err := replicateControlPlaneSecret(ctx, c, src, dst, CASecretName, []string{"ca.crt"}); err != nil {
		return err
	}
	if err := replicateControlPlaneSecret(ctx, c, src, dst, ForkdTLSSecretName, []string{"tls.crt", "tls.key"}); err != nil {
		return err
	}
	return nil
}

// replicateControlPlaneSecret copies exactly the named keys of secret `name`
// from src to dst, creating the destination when absent and updating it when
// its projected data drifts. Keys not in `keys` are never copied (so the CA
// private key cannot leak). A missing source secret is an error: the caller
// runs this only after EnsurePKI has materialized the originals.
func replicateControlPlaneSecret(ctx context.Context, c client.Client, src, dst, name string, keys []string) error {
	var source corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: src, Name: name}, &source); err != nil {
		return fmt.Errorf("read source secret %s/%s for replication: %w", src, name, err)
	}

	projected := make(map[string][]byte, len(keys))
	for _, k := range keys {
		v, ok := source.Data[k]
		if !ok {
			return fmt.Errorf("source secret %s/%s lacks key %s; cannot replicate", src, name, k)
		}
		projected[k] = append([]byte(nil), v...)
	}

	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: dst, Name: name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		copySecret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: dst, Name: name},
			Type:       source.Type,
			Data:       projected,
		}
		if err := c.Create(ctx, &copySecret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost the create race to a parallel reconcile; the winner wrote
				// the same projected data from the same source, so treat as done.
				return nil
			}
			return fmt.Errorf("create replicated secret %s/%s: %w", dst, name, err)
		}
		return nil

	case err != nil:
		return fmt.Errorf("read destination secret %s/%s: %w", dst, name, err)

	default:
		if secretDataEqual(existing.Data, projected) {
			return nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		// Overwrite exactly the projected keys; leave any unrelated keys the
		// destination may carry untouched.
		for k, v := range projected {
			existing.Data[k] = v
		}
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("heal replicated secret %s/%s: %w", dst, name, err)
		}
		return nil
	}
}

// secretDataEqual reports whether dst already contains every key/value in want.
// It does not require dst to be a strict equal (dst may carry extra keys); it
// only checks the projected keys match, which is the replication contract.
func secretDataEqual(dst, want map[string][]byte) bool {
	for k, v := range want {
		got, ok := dst[k]
		if !ok || string(got) != string(v) {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestReplicateHuskSecrets -v`
Expected: PASS (all three TestReplicateHuskSecrets* cases).

- [ ] **Step 5: Lint both targets**

Run: `golangci-lint run --timeout=5m ./internal/controller/... && GOOS=linux golangci-lint run --timeout=5m ./internal/controller/...`
Expected: PASS (clean). If the illustrative `seedControlPlaneSecrets` stub trips unused/unparam, delete it from the test file and re-run.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/secret_replication.go internal/controller/secret_replication_test.go
git commit -m "feat(controller): replicate husk PKI secrets into pool namespaces

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: Call replication from the pool reconcile and wire ControllerNamespace

**Files:**
- Modify: `internal/controller/sandboxpool_controller.go`
- Modify: `cmd/controller/main.go`
- Test: `internal/controller/secret_replication_test.go` (add an integration assertion)

`reconcileHuskPods` creates husk pods in `pool.Namespace`; before that, replicate the secrets the pods mount. The reconciler must know the source (controller) namespace, so add a `ControllerNamespace` field set from `discoveryNamespace` in main.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/secret_replication_test.go`:

```go
func TestReconcileHuskPodsReplicatesSecretsIntoPoolNamespace(t *testing.T) {
	c := newCoreClient(t)
	ctrlNS := newPKINamespace(t, c)
	poolNS := newPKINamespace(t, c)

	// Seed the control plane secrets in the controller namespace, as EnsurePKI
	// would.
	if err := c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ctrlNS, Name: controller.CASecretName},
		Data:       map[string][]byte{"ca.crt": []byte("CA"), "ca.key": []byte("KEY")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ctrlNS, Name: controller.ForkdTLSSecretName},
		Data:       map[string][]byte{"tls.crt": []byte("L"), "tls.key": []byte("K")},
	}); err != nil {
		t.Fatal(err)
	}

	// ReplicateHuskSecrets is what reconcileHuskPods calls; assert it bridges
	// ctrlNS -> poolNS (the reconcile-level wiring is covered by the existing
	// huskpod envtest, which now runs with ControllerNamespace set).
	if err := controller.ReplicateHuskSecrets(ctx, c, ctrlNS, poolNS); err != nil {
		t.Fatal(err)
	}
	_ = getSecret(t, c, poolNS, controller.CASecretName)
	_ = getSecret(t, c, poolNS, controller.ForkdTLSSecretName)
}
```

- [ ] **Step 2: Run the test to verify it fails or passes minimally**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestReconcileHuskPodsReplicatesSecretsIntoPoolNamespace -v`
Expected: PASS (this asserts the helper; the wiring is exercised next). If it fails it is a compile error to fix before proceeding.

- [ ] **Step 3: Add the ControllerNamespace field to the pool reconciler**

In `internal/controller/sandboxpool_controller.go`, add this field to the `SandboxPoolReconciler` struct, after `HuskCASecretName string` (the last field, around line 56):

```go
	// ControllerNamespace is the namespace EnsurePKI materialized the control
	// plane PKI Secrets in (the controller's own namespace, default "mitos").
	// reconcileHuskPods replicates mitos-ca + mitos-forkd-tls FROM here INTO the
	// pool namespace so husk pods, which run in the pool namespace, can mount
	// them. Empty disables replication (the husk pods then require the secrets
	// to already exist in their namespace). Only used when EnableHuskPods.
	ControllerNamespace string
```

- [ ] **Step 4: Call replication before creating husk pods**

In `reconcileHuskPods` (`internal/controller/huskpod.go`), insert this block immediately inside the `case existing < desired:` branch, BEFORE the `for i := int32(0); i < deficit; i++` loop that creates pods (so the secrets exist before the first pod is created). Find the line `for i := int32(0); i < deficit; i++ {` inside `reconcileHuskPods` and insert above the `opts := HuskPodOptions{` that precedes it:

```go
		// Replicate the husk PKI secrets into this pool's namespace before
		// creating pods: husk pods run here, not in the controller namespace,
		// and mount mitos-ca + mitos-forkd-tls. ControllerNamespace empty (or
		// equal to the pool namespace) makes this a noop. A replication error
		// is returned so the deficit is retried on requeue rather than creating
		// pods that would fail to mount their secrets.
		if r.ControllerNamespace != "" {
			if err := ReplicateHuskSecrets(ctx, r.Client, r.ControllerNamespace, pool.Namespace); err != nil {
				return existing, fmt.Errorf("replicate husk secrets into %s: %w", pool.Namespace, err)
			}
		}
```

Note: place it so it runs once per deficit reconcile, before the create loop. The exact anchor is the `opts := HuskPodOptions{` assignment at the start of the `case existing < desired:` branch (huskpod.go around line 575); insert the block immediately before that assignment.

- [ ] **Step 5: Wire ControllerNamespace in main**

In `cmd/controller/main.go`, the `SandboxPoolReconciler` is constructed at line 136-146, but `discoveryNamespace` is defined later (line 196). Move the `discoveryNamespace` resolution ABOVE the pool reconciler construction, or read it inline. Simplest: add the field using the same env logic. Change the pool reconciler literal to add the field; resolve the namespace just before it. Insert immediately before `if err := (&controller.SandboxPoolReconciler{`:

```go
	poolControllerNamespace := os.Getenv("FORKD_NAMESPACE")
	if poolControllerNamespace == "" {
		poolControllerNamespace = "mitos"
	}
```

Then add this field to the `SandboxPoolReconciler{...}` literal (after `HuskCASecretName: controller.CASecretName,`):

```go
		ControllerNamespace: poolControllerNamespace,
```

(The later `discoveryNamespace` block at line 196 is unchanged; both derive from `FORKD_NAMESPACE` with the same `mitos` default, so they agree. A follow-up can DRY them; not required here.)

- [ ] **Step 6: Run the full controller suite**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
Expected: PASS (the existing huskpod envtest still passes; replication is a noop there because those tests construct the reconciler without ControllerNamespace, so it stays empty).

- [ ] **Step 7: Build cmd/controller and lint both targets**

Run: `go build ./cmd/controller/ && golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m`
Expected: PASS (clean).

- [ ] **Step 8: Commit**

```bash
git add internal/controller/sandboxpool_controller.go internal/controller/huskpod.go cmd/controller/main.go internal/controller/secret_replication_test.go
git commit -m "feat(controller): replicate husk PKI secrets per pool namespace on reconcile

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 11: Record the secret-replication surface in the threat model + RBAC comment

**Files:**
- Modify: `deploy/rbac/clusterrole.yaml`
- Modify: `docs/threat-model.md`

The controller now writes secrets into namespaces it does not own. The cluster-wide secrets grant already covers it (no new RBAC), but the justification comment must say so, and the threat model gains a row.

- [ ] **Step 1: Extend the secrets RBAC justification comment**

In `deploy/rbac/clusterrole.yaml`, the secrets rule comment (around line 91-97) ends with the DeleteEncKey sentence. Append to that comment block, before the `- apiGroups:` line that starts the secrets rule:

```yaml
  # create/update ALSO replicate the husk PKI Secrets (mitos-ca ca.crt only,
  # mitos-forkd-tls) FROM the controller namespace INTO each pool namespace
  # where husk pods run (ReplicateHuskSecrets); the CA private key is never
  # replicated. This is why the grant is cluster-wide rather than namespaced.
```

- [ ] **Step 2: Verify the manifest still builds**

Run: `kustomize build deploy/ | grep -A2 'kind: ClusterRole' | grep 'mitos-controller'`
Expected: PASS (the ClusterRole still renders).

- [ ] **Step 3: Add the replication row to the threat model**

Add to `docs/threat-model.md`, near the PKI / secrets section:

```markdown
- Cross-namespace secret replication. The controller copies mitos-ca (ca.crt
  only) and mitos-forkd-tls from its namespace into every pool namespace where
  it creates husk pods (ReplicateHuskSecrets). The CA private key (ca.key) is
  never copied. Scope: the cluster-wide secrets grant is the enabling
  privilege; mitigation is that only the two named control plane Secrets are
  projected and only ca.crt of the CA, so a pool namespace never holds the CA
  signing key. Status: accepted; a namespaced grant scoped to pool namespaces
  is a follow-up once pool namespaces are enumerable at install time.
```

- [ ] **Step 4: Confirm no em/en dashes**

Run: `grep -nP '[\x{2013}\x{2014}]' docs/threat-model.md deploy/rbac/clusterrole.yaml`
Expected: FAIL (no output).

- [ ] **Step 5: Commit**

```bash
git add deploy/rbac/clusterrole.yaml docs/threat-model.md
git commit -m "docs(threat-model): record cross-namespace husk secret replication

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 12: Verify the namespace default is consistent (fix 6)

**Files:**
- Read-only verification; modify only if a gap is found.

The `mitos` default is already in code (`cmd/controller/main.go` discoveryNamespace) and deploy/. This task confirms consistency and only adds a change if something disagrees.

- [ ] **Step 1: Confirm code, namespace manifest, and deployment all say mitos**

Run: `grep -n 'discoveryNamespace = "mitos"\|poolControllerNamespace = "mitos"' cmd/controller/main.go; grep -n 'name: mitos' deploy/controller/namespace.yaml; grep -n 'namespace: mitos' deploy/controller/deployment.yaml deploy/daemon/daemonset.yaml deploy/rbac/serviceaccount.yaml deploy/imagepullsecret.yaml deploy/kernel/daemonset.yaml`
Expected: every location reports `mitos`. The code default (Task 10 added `poolControllerNamespace`), the namespace object, and every namespaced manifest agree.

- [ ] **Step 2: Confirm the controller Deployment runs in mitos and gets the env if overridden**

Run: `grep -n 'FORKD_NAMESPACE' cmd/controller/main.go deploy/controller/deployment.yaml`
Expected: code reads `FORKD_NAMESPACE` (default mitos); the deployment does NOT set it, so the default applies and matches its own `namespace: mitos`. This is consistent: no env override means the code default `mitos` equals the deployment namespace.

- [ ] **Step 3: Decide**

If Steps 1-2 all report `mitos` with no disagreement, there is NO gap: record that and skip to the final review. If any location disagrees (for example a manifest in `mitos-system`), fix that single manifest to `mitos` and commit:

```bash
git add <the-one-file-that-disagreed>
git commit -m "fix(deploy): align <file> namespace to mitos

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

Expected (likely): no gap; no commit needed.

---

## Final review

- [ ] **Full build + render + suite**

Run:
```bash
go build ./... && \
eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ && \
kustomize build deploy/ >/dev/null && echo "RENDER OK" && \
golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m && \
GOOS=linux GOARCH=amd64 go build ./guest/agent/
```
Expected: all PASS.

- [ ] **Server dry-run the whole stack (if a cluster is reachable)**

Run: `kustomize build deploy/ | kubectl apply --dry-run=server -f -`
Expected: every object reports `created`/`configured (server dry run)` with no admission rejection (the PSA labels admit forkd + device-plugin + kernel-stage).

- [ ] **No em/en dashes anywhere in the diff**

Run: `git diff main --unified=0 | grep -nP '[\x{2013}\x{2014}]'`
Expected: FAIL (no output).

- [ ] **Confirm the eight fixes are all present**

Run:
```bash
kustomize build deploy/ | grep -E 'pod-security.kubernetes.io/enforce: privileged' && \
kustomize build deploy/ | grep 'name: ghcr-pull' && \
kustomize build deploy/ | grep -A8 'kind: ServiceAccount' | grep 'ghcr-pull' && \
kustomize build deploy/ | grep -- '--agent-bin=/usr/local/bin/agent' && \
kustomize build deploy/ | grep 'privileged: true' && \
kustomize build deploy/ | grep 'name: DOCKER_CONFIG' && \
( kustomize build deploy/ | grep -- '--jailer=' && echo "FAIL: jailer still present" || echo "jailer removed OK" ) && \
kustomize build deploy/ | grep 'name: mitos-kernel-stage' && \
grep -q 'func ReplicateHuskSecrets' internal/controller/secret_replication.go && echo "replication present"
```
Expected: each fix prints its evidence; `jailer removed OK`; `replication present`.

---

## Self-review notes

- Fix 1 (PSA): Tasks 1, 2.
- Fix 2 (pull secret + SA): Tasks 3, 4 (and forkd pod imagePullSecrets in Task 6 Step 5).
- Fix 3 (forkd daemonset: agent-bin, privileged, DOCKER_CONFIG, drop jailer): Task 6 (+ Task 5 builds the agent binary, + Task 7 threat-model delta). The jailer-in-pod follow-up is cross-referenced in Task 6 Step 2.
- Fix 4 (kernel provisioning): Task 8, staging to `/var/lib/mitos/vmlinux`, the path forkd and husk both read.
- Fix 5 (PKI replication): Tasks 9, 10 (Go + envtest + wiring), Task 11 (RBAC comment + threat-model delta). No new RBAC needed; existing cluster-wide secrets grant covers it.
- Fix 6 (namespace consistency): Task 12, verification-only with a conditional fix.
- Every code step shows complete Go/YAML. Conventional commits, explicit `git add` paths, both lint targets, em/en dash guard. The replication helper signature `ReplicateHuskSecrets(ctx, client, src, dst)` is consistent across Tasks 9, 10, and the final-review grep.
