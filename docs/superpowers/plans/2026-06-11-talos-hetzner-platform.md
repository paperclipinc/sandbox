# Talos + Hetzner Reference Platform Implementation Plan (issue #16)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close issue #16 (ROADMAP section 5). Make the system deployable on its stated bare-metal reference architecture: Talos Linux on Hetzner dedicated (Robot/AX) servers, which provide `/dev/kvm` (Hetzner Cloud does not; it runs gVisor systrap, so Firecracker needs dedicated hardware). Ship a complete, validated production deploy manifest set (controller, forkd DaemonSet, CRDs, RBAC, namespace, a kustomize base), Talos machine configs for a KVM-capable worker (the kernel modules and devices Firecracker and the guest agent need), and an honest provisioning runbook. Verified in CI by validating every manifest with kubeconform and the Talos config with talosctl validate, and by the manifests applying on kind (already exercised by the dev overlay). The actual bare-metal boot and a real Firecracker fork on Hetzner need hardware and are documented as such; density and cost numbers are marked as targets per the no-unverified-claims rule until measured on the pinned reference node.

**Honesty constraint:** this PR proves the deploy artifacts are valid and apply, and that the Talos config is schema-valid; it does NOT claim a measured bare-metal density or cost (no Hetzner hardware in CI). Any density/cost figure in the runbook is labeled a target with an issue reference, never presented as measured. The runbook states exactly what is CI-verified (manifests kubeconform-clean, Talos config validates, manifests apply on kind with the mock engine) versus what requires the reference hardware (Firecracker fork on /dev/kvm, the latency/density numbers, which the bench and KVM CI already measure on shared CI and the bench program will measure on the pinned node when one exists).

**Architecture:** `deploy/` gets a kustomize base composing the CRDs, namespace, RBAC, controller Deployment, and forkd DaemonSet into one production install. The controller RBAC (ServiceAccount + ClusterRole + binding for the CRDs and the node/pod reads the ForkdDiscovery needs) is added if not already generated under `config/`. `deploy/talos/` gets a worker MachineConfig patch (kernel modules kvm, kvm_intel/kvm_amd, vhost_vsock, tun; the node label `mitos.run/kvm=true`; a dataDir disk mount; required sysctls) and a control-plane patch. `docs/platforms/talos-hetzner.md` is the runbook. CI validates manifests with kubeconform and the Talos config with talosctl validate.

**Context for the implementer:**
- Existing deploy: `deploy/crds/*` (4 CRDs), `deploy/controller/{deployment.yaml,namespace.yaml}`, `deploy/daemon/daemonset.yaml` (nodeSelector `mitos.run/kvm=true`, `/dev/kvm` hostPath, a securityContext with retained capabilities pending KVM CI proof), `deploy/dev/*` (the mock-mode kustomize overlay for `mitos dev up`). The dev overlay shows the kustomize style and the controller/forkd args.
- RBAC: check `config/rbac/` (kubebuilder usually generates `role.yaml` from `// +kubebuilder:rbac` markers in the controllers). If present, surface it into `deploy/`; if the markers are missing, add them and `make manifests` then include the generated role. The controller reconciles SandboxTemplate/Pool/Claim/Fork (CRD verbs) and reads Pods/Nodes (ForkdDiscovery watches forkd pods; node selection reads the NodeRegistry fed by heartbeats, confirm whether it lists Nodes/Pods).
- forkd flags (cmd/forkd): the production forkd runs with the real engine (KVM), mTLS (the PKI bootstrap), `--enable-networking`, optionally `--enable-dns-egress`, `--enable-encryption`; the DaemonSet args should reflect a sane production default and reference the flags that exist. Do not enable a flag the binary lacks.
- The directive: Hetzner Cloud (cpx-class) has NO /dev/kvm (gVisor); Firecracker requires Hetzner Robot/AX dedicated servers (or another bare-metal/nested-virt host). Talos is the OS. Be honest about this in the runbook.
- Tools: kubeconform (validates k8s manifests against schemas, handles CRDs with `-schema-location`); talosctl validate (validates a Talos MachineConfig, mode container/metal). Both are installable in CI (Go-install or release binaries).
- Conventions: CLAUDE.md authoritative. No em/en dashes. Explicit-path git add. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Lint clean if any Go changes (RBAC markers regen). No unverified numbers.

---

### Task 1: complete and validate the production deploy manifests

**Files:** `deploy/rbac/` (new: serviceaccount + clusterrole + clusterrolebinding for the controller, and forkd SA if needed), `deploy/kustomization.yaml` (new base), possibly `config/rbac` + controller RBAC markers + `make manifests`, `deploy/controller/deployment.yaml` (production hardening).

- [ ] RBAC: locate or generate the controller ClusterRole. If `// +kubebuilder:rbac` markers exist on the reconcilers, run `make manifests` and copy/surface the generated `config/rbac/role.yaml` into `deploy/rbac/clusterrole.yaml`; if missing, add the markers (CRD groups mitos.run verbs get/list/watch/create/update/patch/delete + status; core pods get/list/watch for ForkdDiscovery; nodes get/list/watch if node selection reads them; events create) and regen. Add a ServiceAccount (in the mitos namespace) and a ClusterRoleBinding. Reference the SA from the controller Deployment.
- [ ] `deploy/kustomization.yaml`: a base composing crds + namespace + rbac + controller + daemon into one `kubectl apply -k deploy/` production install. Keep `deploy/dev/` as the mock overlay (it can remain standalone or become an overlay of the base; do not break `mitos dev up`).
- [ ] Production-harden the controller Deployment: resource requests/limits, liveness/readiness probes (the controller serves a health/metrics port), the mTLS PKI bootstrap mode (NOT --disable-pki-bootstrap; production uses mTLS), and a non-mock engine expectation. Confirm the args match real cmd/controller flags.
- [ ] CI: add a manifest-validation job (or step in an existing job) running kubeconform over `deploy/**` (with the CRD schemas extracted so CRD-typed resources validate, e.g. point kubeconform at the CRDs or use `-skip` for CRD kinds with a note). Gate on kubeconform passing. Also assert `kubectl apply --dry-run=client -k deploy/` parses (or kustomize build deploy/ resolves).
- [ ] Commit `feat: production deploy manifests with RBAC and a kustomize base`.

### Task 2: Talos machine configs for a KVM worker

**Files:** `deploy/talos/worker-kvm.yaml` (a MachineConfig patch), `deploy/talos/controlplane.yaml` (or a patch), `deploy/talos/README.md`.

- [ ] `deploy/talos/worker-kvm.yaml`: a Talos worker MachineConfig (or a patch applied to `talosctl gen config` output) that: loads the kernel modules `kvm`, `kvm_intel` and `kvm_amd` (or the host-appropriate one), `vhost_vsock`, `tun` (machine.kernel.modules); sets the node label `mitos.run/kvm=true` (machine.nodeLabels); mounts a data disk for the forkd dataDir (machine.disks or a user volume, mounted where forkd expects, e.g. /var/lib/mitos); sets any required sysctls (machine.sysctls, e.g. for vsock/networking if needed); and notes the CPU must expose virtualization (Hetzner AX dedicated). Keep it a minimal, documented patch over a generated base, not a full hand-rolled config.
- [ ] `deploy/talos/controlplane.yaml`: a minimal control-plane patch (or note that a standard Talos control plane is used and only the workers need the KVM patch).
- [ ] `deploy/talos/README.md`: how to use these (talosctl gen config, apply the worker-kvm patch, the modules/label/disk rationale, the Hetzner-AX-dedicated requirement and why Hetzner Cloud will not work).
- [ ] CI: install talosctl and run `talosctl validate --config deploy/talos/worker-kvm.yaml --mode metal` (and the control-plane) so the configs are schema-validated. If a raw patch is not a full config, validate the merged result (gen a base in CI + apply the patch + validate) or use the talos config schema. Gate on validation passing.
- [ ] Commit `feat: Talos machine configs for KVM-capable worker nodes`.

### Task 3: the provisioning runbook

**Files:** `docs/platforms/talos-hetzner.md` (new), `ROADMAP.md`.

- [ ] `docs/platforms/talos-hetzner.md`: the end-to-end runbook: (1) Hetzner BOM (e.g. 3x AX-class dedicated servers; CPU with virtualization; NVMe for snapshots; the network), stated as a reference example not a measured benchmark; (2) why Hetzner Cloud will not work (no /dev/kvm, gVisor) and dedicated/Robot is required; (3) installing Talos on the dedicated servers (the Talos Hetzner/bare-metal install path, PXE or the rescue-system image), applying the control-plane and the worker-kvm machine configs; (4) verifying the node is KVM-ready (`/dev/kvm` present, the modules loaded, the `mitos.run/kvm=true` label, vsock available); (5) deploying the operator (`kubectl apply -k deploy/`), the PKI/mTLS bootstrap, creating a pool, and a smoke test (a claim reaches Ready and a real exec runs); (6) capacity planning pointers (density per node is a target until measured on the pinned reference node, link the bench program and the CoW metering). Clearly mark CI-VERIFIED (manifests valid + apply on kind; Talos config validates) vs HARDWARE-REQUIRED (the Firecracker fork, the density/cost numbers, which are targets per the no-unverified-claims rule).
- [ ] ROADMAP section 5: flip the talos-hetzner doc + machine-config fragments to done (configs validated, runbook shipped); keep the bare-metal MEASURED density/cost and the pinned-hardware bench run as open (⬜) with a note they need the reference hardware, NOT fabricated.
- [ ] Commit `docs: Talos plus Hetzner bare-metal provisioning runbook`.

### Task 4: verification + PR

- [ ] Full verification:
```bash
go build ./... && GOOS=linux go build ./... && go vet ./... && ~/go/bin/golangci-lint run --timeout=5m && GOOS=linux ~/go/bin/golangci-lint run --timeout=5m
eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/... -count=1 -race 2>&1 | tail -8   # if any Go changed (RBAC markers)
# manifest + talos validation locally if the tools are available:
command -v kubeconform >/dev/null && kubeconform -summary -ignore-missing-schemas deploy/ || echo "kubeconform not local; CI validates"
command -v kustomize >/dev/null && kustomize build deploy/ >/dev/null && echo "kustomize base resolves" || kubectl kustomize deploy/ >/dev/null 2>&1 && echo ok || echo "validated in CI"
gofmt -l ./api ./internal ./cmd | wc -l   # 0
grep -rlP '\x{2014}|\x{2013}' --include='*.go' --include='*.md' --include='*.yaml' --include='*.yml' . | grep -v '^./.git' | wc -l   # 0
python3 -c "import yaml,glob; [list(yaml.safe_load_all(open(f))) for f in glob.glob('deploy/**/*.yaml',recursive=True)]; print('deploy yaml ok')"
python3 -c "import yaml; list(yaml.safe_load_all(open('.github/workflows/ci.yaml'))); print('ci yaml ok')"
git status --short
```
- [ ] Push `feat/talos-hetzner-platform`, PR `Talos plus Hetzner reference platform: deploy manifests, machine configs, runbook` body `## Summary` bullets (production deploy manifests with RBAC + a kustomize base, validated by kubeconform in CI; Talos worker machine config for KVM nodes validated by talosctl; honest Hetzner-AX-dedicated provisioning runbook; what is CI-verified vs hardware-required clearly marked; density/cost are targets until measured on the pinned node; `Closes #16`), `## Test plan` checklist, `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. No em/en dashes. Do NOT merge (controller watches CI and merges).

**Out of scope (follow-ups):** the pinned bare-metal CI runner and the MEASURED density/cost/latency numbers on the reference node (needs the hardware; ROADMAP section 4 bench-on-bare-metal); a Hetzner Robot API / Terraform provisioning automation; the KVM device plugin (vs the hostPath /dev/kvm + nodeSelector approach); high-availability control-plane sizing; the husk-pods #18 deployment shape; air-gapped/offline install.
