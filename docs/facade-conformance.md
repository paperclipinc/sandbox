# agents.x-k8s.io facade conformance

This document records how the `agents.x-k8s.io` conformance facade (issue #19,
`cmd/facade` + `internal/facade`) is held to the upstream SIG agent-sandbox API,
what is PROVEN today, and what is deferred. See ADR 0001
(docs/adr/0001-facade-and-naming.md) for the facade design and the toolchain
decision.

## Principle: no silent divergence

We implement the upstream API (`agents.x-k8s.io/v1alpha1 Sandbox`); we do not
fork or shadow it. Every upstream artifact, each `test/e2e/*_test.go` and each
vendored example manifest, has a row in the matrix below with one of three
statuses. There is no fourth status and no omission: an undocumented divergence
is a bug.

- PROVEN-OBJECT-LEVEL-ON-KIND: the facade-conformance CI job (or the facade
  envtest) asserts the object-level fact on a kind cluster, with their manifest
  applied UNCHANGED.
- NEEDS-BARE-METAL: the upstream predicate requires a RUNNING sandbox (a booted
  in-VM workload: PodReady / ChromeReady / the "Pod is Ready; Service Exists"
  status). Our run path reaches Ready only when a dormant Firecracker VMM boots
  inside the husk pod, which needs a KVM-capable kubelet. That is the #18
  nested-VMM boundary; it is proven on the KVM runner in kvm-test.yaml, not on a
  shared kind runner.
- JUSTIFIED-EXCEPTION: a field or behavior the facade maps differently (or does
  not yet map), with the reason. The behavior is recorded, not silently dropped.

## Pinned upstream version

- Module: `sigs.k8s.io/agent-sandbox`, version `v0.4.6` (pinned). The CRDs,
  examples, and `test/e2e` are vendored verbatim under `third_party/agent-sandbox/`.
- Latest-two-minors policy: the conformance approach tracks the upstream API as
  it moves by pinning their latest two minor releases. Today only v0.4.6 is
  wired (vendored + applied unchanged). Wiring the second minor (a parallel
  vendored tree + a CI matrix dimension) is a follow-up; this is stated honestly
  rather than implied.

## Apply-unchanged acceptance

Their example `Sandbox` manifests apply UNCHANGED (only the `${IMAGE}`
placeholder is substituted, on a copy; the vendored files are never edited) and
the facade reconciles them object-level:

- In envtest (`internal/facade/examples_test.go`): every core
  `agents.x-k8s.io/v1alpha1` Sandbox example vendored under
  `third_party/agent-sandbox/examples` (and `extensions/examples`) is applied and
  the facade creates the bridged husk-backed `SandboxClaim` (default-pool
  binding, owner reference, first-container env mirrored).
- In CI (`facade-conformance` kind job): the upstream
  `examples/hello-world-sandbox/hello-world.yaml` is applied UNCHANGED against a
  live apiserver and the five object-level facts below are asserted.

The RUNNING-sandbox behavior (the in-VM Ready tail) is the bare-metal path and is
NOT asserted on kind; see the NEEDS-BARE-METAL rows.

## The facade-conformance CI job (object level)

The `facade-conformance` job in `.github/workflows/ci.yaml` mirrors
`kind-e2e-husk`: it builds + loads the facade and controller images, creates a
kind cluster, installs the vendored upstream `agents.x-k8s.io` Sandbox CRD plus
our CRDs, deploys our controller + the facade (PKI on, `--default-pool=default`)
and a `default` SandboxPool, applies their hello-world Sandbox UNCHANGED, and
asserts, each gated with a SETUP-vs-CONFORMANCE distinction and a diagnostics
trap:

- (a) the Sandbox is ADMITTED by the upstream CRD (verbatim);
- (b) the facade creates the bridged husk-backed `SandboxClaim` (owner reference
  back to the Sandbox + the `agentrun.dev/pool` bridge annotation set to the
  default pool);
- (c) the facade UPDATES the Sandbox status with a Ready condition reflecting the
  run-path state achievable on kind (Ready=False while the husk claim is Pending,
  since no VMM boots here);
- (d) deleting the Sandbox GCs the bridged claim (the live apiserver runs the
  owner-reference garbage collector);
- (e) a replicas-0 Sandbox terminates the run-path object (pause = warm-pool
  release);
- (f) a replicas 1->0->1 toggle is an OBJECT-LEVEL resume: after the
  pause releases the claim, scaling back to replicas 1 RE-ACTIVATES it (the
  facade re-creates the bridged claim). This is the object-level half of the
  pause/resume mapping; the in-VM resume tail (snapshot load + resume +
  guest-ready) is the #18 bare-metal boundary, not asserted here.

The job echoes that the in-VM Ready tail (PodReady / ChromeReady) needs a
KVM-capable kubelet (the #18 boundary) and is not asserted there.

## Did we run their Go e2e suite? (honest answer: no, by design)

Their `test/e2e` Go suite is vendored as the conformance REFERENCE and is NOT
run against our facade. This is not laziness; it is a structural mismatch we
verified by reading the vendored sources:

- `test/e2e/framework/context.go` imports `sigs.k8s.io/agent-sandbox/controllers`
  (their in-tree controller package). The framework wires THEIR controller's
  scheme and dumps THEIR controller's pod logs (`app=agent-sandbox-controller`
  in `agent-sandbox-system`). It expects their controller running.
- Every conformance test asserts upstream-controller-created objects that our
  facade does not produce: a `Pod` named after the Sandbox, a headless
  `Service`, and a Sandbox status whose Ready condition is literally
  `Message: "Pod is Ready; Service Exists"`, `Reason: DependenciesReady` (see
  `basic_test.go`). Those predicates require a RUNNING pod that becomes Ready.

Our facade bridges a Sandbox to a husk-backed `SandboxClaim` on the fork engine;
it does not create a Pod/Service, and Ready requires the in-VM boot. So their Go
suite is bare-metal-gated (it needs both their controller and a running
sandbox). Half-running it and claiming a pass it did not achieve would violate
the no-unverified-claims rule. The matrix records each of their tests as
NEEDS-BARE-METAL accordingly.

## Conformance matrix

### Upstream `test/e2e/*_test.go` (the Go conformance reference)

| Upstream test | What it asserts upstream | Status | Notes |
| --- | --- | --- | --- |
| `test/e2e/basic_test.go` :: `TestSimpleSandbox` | Sandbox -> Pod Ready + Service, status `"Pod is Ready; Service Exists"` | NEEDS-BARE-METAL | Asserts a running Pod/Service the facade does not create; Ready needs the in-VM boot (#18). Object-level Sandbox admission + the bridged claim ARE proven on kind (facade-conformance (a),(b)). |
| `test/e2e/replicas_test.go` :: `TestSandboxReplicas` | replicas 0 deletes the Pod, keeps the Service | PROVEN-OBJECT-LEVEL-ON-KIND (run-path object) / NEEDS-BARE-METAL (Pod/Service) | The pause/resume contract is proven against our run-path object: facade-conformance (e) asserts replicas 0 RELEASES the bridged claim (warm-pool release) and (f) asserts a replicas 1->0->1 toggle RE-ACTIVATES it (object-level resume). The upstream Pod/Service deletion + the in-VM resume tail need their controller + a running sandbox (#18). |
| `test/e2e/shutdown_test.go` :: `TestSandboxShutdownTime`, `TestSandboxRetainedExpiryPreservesFinishedCondition` | shutdown tears down Pod/Service in bounded time; Finished condition retained | NEEDS-BARE-METAL | Requires a running Pod that succeeds and the upstream Finished-condition controller. The deletion/GC object contract is proven object-level (facade-conformance (d)). |
| `test/e2e/parallelism_test.go` :: `TestParallelSandboxes`, `TestParallelSandboxClaimsWith{Sufficient,Insufficient}WarmPool` | many Sandboxes/Claims reach Ready in parallel via a warm pool | NEEDS-BARE-METAL | Waits `ReadyConditionIsTrue` on running sandboxes drawn from a warm pool; needs the in-VM boot and the warm-pool/claim extension mappings (a later slice). |
| `test/e2e/volumeclaimtemplate_test.go` :: `TestSandboxVolumeClaimTemplates` | `volumeClaimTemplates` produce PVCs bound to the Pod | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | The facade does not yet map `volumeClaimTemplates` (storage contract, exception 4 below); upstream also needs a running Pod. The manifest still applies unchanged and the claim bridges (proven object-level). |
| `test/e2e/chromesandbox_test.go` :: `TestRunChromeSandbox`, `BenchmarkChromeSandboxStartup` | Chrome serves CDP inside the sandbox; measures PodReady + ChromeReady | NEEDS-BARE-METAL | The canonical running-sandbox predicate (ChromeReady on the CDP port). Pure in-VM boot; the #18 boundary. |
| `test/e2e/chromesandbox_claim_test.go` :: `BenchmarkChromeSandboxClaimStartup` | Chrome via a `SandboxClaim` drawn from a warm pool | NEEDS-BARE-METAL | Running sandbox + the warm-pool/claim extension mapping (later slice). |
| `test/e2e/extensions/pythonruntime_test.go` :: `TestRunPythonRuntimeSandbox`, `...Claim`, `...Warmpool` | a python runtime sandbox/claim/warmpool serves requests | NEEDS-BARE-METAL | Running sandbox + the extension mappings (later slice). |
| `test/e2e/extensions/shutdown_policy_test.go` :: `TestSandboxClaim{DeleteForeground,TTL...,ExpiryUsesEarlier...,FinishedWithoutTTLIsRetained,TTLZeroRetain...}` | SandboxClaim TTL / shutdown-policy / finished-condition retention | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | Exercises the upstream `SandboxClaim` extension (not yet mapped, exception 3) and needs a running claim that finishes. |
| `test/e2e/extensions/warmpool_rollout_test.go` :: `TestWarmPoolRollout`, `...MultiTemplateIsolation`, `...SwitchTemplate`, `...MetadataUpdate` | SandboxWarmPool rollout/template-switch semantics | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | Exercises the upstream `SandboxWarmPool` extension (not yet mapped, exception 3) and needs running pool pods. |
| `test/e2e/extensions/warmpool_sandbox_watcher_test.go` :: `TestWarmPoolSandboxWatcher`, `TestWarmPoolPodNameAnnotationBeforeReady` | warm-pool watcher annotates the bound pod | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | `SandboxWarmPool` extension (exception 3) + a running pod. |
| `test/e2e/extensions/sandboxclaim_metric_test.go` :: `TestSandboxClaimObservabilityAnnotation` | a SandboxClaim observability annotation is set | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | Upstream `SandboxClaim` extension (exception 3) + the upstream controller. |
| `test/e2e/framework/watchset_test.go` :: `TestWatchSet...` | the framework's own watch-set unit test | JUSTIFIED-EXCEPTION (not a facade conformance test) | Tests the upstream test framework internals, not the Sandbox API surface; nothing for the facade to satisfy. Vendored for completeness of the reference. |

### Vendored example manifests (apply-unchanged)

Core `agents.x-k8s.io/v1alpha1` Sandbox examples: each applies UNCHANGED and the
facade bridges a husk-backed claim object-level (`internal/facade/examples_test.go`
asserts all of these; `facade-conformance` asserts hello-world end to end against
a live apiserver). The fields beyond identity + the first container's env are
JUSTIFIED-EXCEPTIONs (see the exceptions section); the manifest still applies and
reconciles.

| Example manifest | Extra podTemplate fields (exceptions) | Status |
| --- | --- | --- |
| `examples/hello-world-sandbox/hello-world.yaml` | restartPolicy | PROVEN-OBJECT-LEVEL-ON-KIND (envtest + facade-conformance job) |
| `examples/sandbox-ksa/sandbox.yaml` | serviceAccountName, volumeClaimTemplates, volumeMounts, command | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/hermes-agent/sandbox.yaml` | env.valueFrom (secret refs, mirrored through), ports, volumeMounts, volumes, volumeClaimTemplates, args | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/python-runtime-sandbox/sandbox-python-kind.yaml` | ports, imagePullPolicy, podTemplate labels/annotations | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/aio-sandbox/aio-sandbox.yaml` | ports, resources | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/kata-gke-sandbox/sandbox-kata-gke.yaml` | runtimeClassName, resources | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/openclaw-sandbox/openclaw-sandbox.yaml` | ports, env, volumes | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/jupyterlab/jupyterlab.yaml` | ports, resources | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/jupyterlab/jupyterlab-full.yaml` | multiple containers, env.valueFrom, volumeClaimTemplates | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/analytics-tool/jupyterlab.yaml` | ports, resources | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/analytics-tool/analytics-tool/sandbox-python.yaml` | ports | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/langchain/deployment.yaml` (the Sandbox doc) | env, resources (the Deployment docs are not Sandboxes) | PROVEN-OBJECT-LEVEL-ON-KIND (envtest, Sandbox doc only) |
| `examples/vscode-sandbox/base/vscode-sandbox.yaml` | env, ports, volumeClaimTemplates | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/vscode-sandbox/overlays/kata-mshv/patch-kata.yaml` | runtimeClassName (full manifest with containers) | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/policy/kyverno/.chainsaw-tests/setup-sandbox.yaml` | minimal Sandbox fixture | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/vscode-sandbox/overlays/{kata,gvisor}/patch-kata.yaml`, `patch-gvisor.yaml` | strategic-merge patch fragments (no containers) | JUSTIFIED-EXCEPTION | not standalone applyable Sandboxes; layered onto the base via kustomize upstream. Recorded under the vscode-sandbox base row. |

Extension example manifests (`extensions.agents.x-k8s.io` kinds): the facade maps
only the core `Sandbox` today; the extension kinds are a later-slice mapping
(exception 3). Each is vendored and recorded.

| Extension example manifest | Kind | Status |
| --- | --- | --- |
| `extensions/examples/sandboxwarmpool.yaml` | SandboxWarmPool | JUSTIFIED-EXCEPTION (extension mapping is a later slice; exception 3) |
| `extensions/examples/sandboxtemplate.yaml`, `secure-sandboxtemplate.yaml`, `llm.yaml` | SandboxTemplate | JUSTIFIED-EXCEPTION (extension mapping is a later slice; exception 3) |
| `extensions/examples/sandboxclaim.yaml`, `sandbox-claim.yaml` | SandboxClaim (upstream extension) | JUSTIFIED-EXCEPTION (extension mapping is a later slice; exception 3) |

## Documented exceptions (justified, not silent)

1. podTemplate fidelity. The facade reconciles the upstream
   `spec.podTemplate.spec.containers[0].env` onto our run path (env.valueFrom
   refs copy through unchanged). Other podTemplate fields (image, resources,
   ports, command/args, volumes/volumeMounts, serviceAccountName, security
   context, multiple containers) are NOT yet honored per-Sandbox: the husk pool
   pins image and resources at pool-build time, and a Sandbox binds to a pool via
   the `agentrun.dev/pool` bridge annotation. The manifests still apply unchanged
   and the facade bridges the claim. Closing this requires the pool/template
   extension mappings (a later slice).

2. pause/resume semantics. Upstream hibernation is a disk roundtrip (the pod is
   torn down and its state persisted to a volume, then rebuilt). The upstream
   pause/resume contract is the `spec.replicas` 0<->1 toggle (upstream v0.4.6 has
   NO stateful hibernate field; their controller deletes the pod on 0 and
   cold-creates a fresh one on 1). The facade maps it onto the husk warm pool:
   replicas 0 (pause) RELEASES the bridged claim so the bound husk pod returns
   dormant to the warm pool; replicas 1 after a 0 (resume) RE-ACTIVATES a dormant
   warm husk pod via the same fast path as create (the ~42ms husk activation,
   #66). The conformant observable is preserved: `Status.Replicas` reflects 0/1,
   the Ready condition reflects Paused/Ready honestly, and pause clears
   `serviceFQDN` + `podIPs` while resume re-populates them. This OBJECT-LEVEL
   behavior is proven on kind (envtest `internal/facade` + the `facade-conformance`
   job's replicas 1->0->1 resume assertion). The resume-latency advantage (warm
   re-activation vs a cold pod create) is the DESIGN claim; the in-VM
   head-to-head number is a bare-metal-reference-node TARGET (#16), measured by
   `bench/facade/` (see [`../bench/facade/README.md`](../bench/facade/README.md)
   and the "Facade vs upstream reference: resume latency" section of
   `BENCHMARKS.md`). Note our resume is STATE-FRESH (a warm dormant pod, not the
   exact pre-pause in-VM state); a state-PRESERVING pause (a memory snapshot taken
   across the pause via the Checkpoint primitive, so resume restores the exact
   in-VM state) is a documented FUTURE (slice 4), not implemented here. No public
   latency number is stated until the bare-metal harness produces it (the
   no-unverified-claims rule).

3. extension kinds. `SandboxWarmPool` -> our pool, `SandboxTemplate` -> our
   template, and the upstream `SandboxClaim` -> our fork-from-snapshot are NOT yet
   mapped. The facade today maps only the core `Sandbox`. The extension mappings
   are a later slice.

4. stable identity / storage contract. Stable hostname/identity and the full
   storage (`volumeClaimTemplates`) contract fidelity are a later slice.

## What is PROVEN now

- envtest (`internal/facade`): the facade creates the bridged husk-backed claim
  for a Sandbox, mirrors its readiness into the Sandbox status, RELEASES the
  claim + clears the serving observables on replicas 0 (pause), RE-ACTIVATES the
  claim on replicas 1 after a 0 (resume), is stable + idempotent under a
  1->0->1->0 toggle, and leaves the claim owner-referenced for GC on delete.
  Every vendored core Sandbox example applies unchanged and bridges a claim.
- CI (`facade-conformance` kind job): their hello-world Sandbox applies UNCHANGED
  against a live apiserver and the object-level facts (a)-(f) above hold,
  including the replicas 1->0->1 OBJECT-LEVEL resume (the facade releases then
  re-creates the bridged claim).
- `bench/facade/`: the reproducible pause/resume latency harness + methodology
  (object-level resume on kind; the in-VM head-to-head a bare-metal target, #16).

## What is OPEN (later #19 slices)

- The in-VM conformance (PodReady / ChromeReady, the "Pod is Ready; Service
  Exists" status) on a KVM-capable kubelet / bare-metal reference node (the #18
  nested-VMM boundary).
- Running the full upstream Go e2e suite green end to end (needs their controller
  + the running-sandbox tail).
- The latest-two-minors CI matrix (only v0.4.6 is wired now; the second minor is
  a follow-up).
- State-PRESERVING pause: a memory snapshot taken across the pause (the
  Checkpoint primitive) so resume restores the exact pre-pause in-VM state, not a
  fresh warm pod. The object-level pause/resume mapping (warm-pool release + fast
  re-activation) and the `bench/facade/` methodology are DONE; the in-VM
  head-to-head resume number stays a bare-metal target (#16).
- The `SandboxWarmPool` -> our pool and upstream `SandboxClaim` -> our
  fork-from-snapshot extension mappings.
- Full podTemplate fidelity, stable hostname/identity, and the storage contract.
