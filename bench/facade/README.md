# Facade pause/resume latency harness

`bench/facade` is the reproducible source behind the "Facade vs upstream
reference: resume latency" section of [`../../BENCHMARKS.md`](../../BENCHMARKS.md).
It measures how our `agents.x-k8s.io` facade (issue #19) handles the upstream
Sandbox pause/resume contract (the `spec.replicas` 0<->1 toggle) and frames the
honest comparison against the upstream reference controller.

No result numbers are committed here. The harness produces them; what is and is
not measurable on a given cluster is spelled out below.

## What is measured

The harness, given a cluster (`--kubeconfig`):

1. Applies an upstream `agents.x-k8s.io/v1alpha1` Sandbox bound to one of our
   pools via the `mitos.run/pool` bridge annotation.
2. Times the **initial claim latency**: apply -> the facade bridges the
   husk-backed `SandboxClaim`.
3. Toggles `spec.replicas` `1 -> 0 -> 1` for `--iterations` rounds, timing the
   **object-level resume latency** each round: wall-clock from the replicas-1
   patch to the facade re-creating the bridged claim.
4. Prints a nearest-rank P50/P90/P99 distribution (via `internal/benchstat`, the
   same summarizer `cmd/bench` uses).

## The two systems being compared

| system | how it handles resume (replicas 0 -> 1) |
| --- | --- |
| our facade | RE-ACTIVATES a dormant warm husk pod: the bridged `SandboxClaim` is re-created and the warm pool hands back a pre-prepared husk (snapshot load + resume + guest-ready, the ~42ms husk activation datapoint, #66). No pod schedule, no image pull, no container start, no app boot on the hot path. |
| upstream reference controller (v0.4.6) | COLD-CREATES a pod: their controller deletes the pod on replicas 0 and creates a fresh one on replicas 1 (pod schedule + admission + image + container start + app boot). |

The order-of-magnitude resume advantage is the **design** claim: re-activating a
warm dormant VM is fundamentally cheaper than cold-creating a pod. The full
same-cluster head-to-head number is a **bare-metal-reference-node TARGET (#16)**;
see the boundary below.

## The kind / shared-CI boundary (#18): object-level only

On a shared-CI or kind cluster the husk VMM **does not boot** (the #18 nested-VMM
boundary). Two consequences, both honest:

- The facade's bridged claim never reaches Ready on kind (no in-VM boot), so the
  **in-VM resume tail** (snapshot load + resume + guest-ready) is **NOT
  measurable here**. The harness times only the OBJECT-LEVEL reconcile (the
  claim re-appearing), which is the facade's own reconcile latency, not the VM
  resume.
- A naive head-to-head on kind would be misleading: with no VMM, our in-VM
  advantage cannot show, and the upstream controller's pod actually starts (its
  container does boot on kind). Running both on kind would falsely flatter the
  upstream side. So the harness on kind asserts and measures the OBJECT-LEVEL
  resume only and does not print a head-to-head in-VM number.

For the real in-VM numbers, run on a **KVM-capable kubelet** (the #16 reference
node), where the husk VMM boots inside the husk pod and the claim reaches Ready.
There the harness's per-iteration timing can be extended to the Ready tail
(`sandboxReady`), and the upstream reference controller can be deployed
alongside for a true same-cluster head-to-head. Until that node exists, the in-VM
resume number stays a target (#16), exactly as the husk activation datapoint in
`BENCHMARKS.md` does.

## Running it

The harness assumes the target cluster already has our CRDs, our controller, a
SandboxPool named by `--pool`, and the facade deployed (the
`facade-conformance` kind job in `.github/workflows/ci.yaml` sets up exactly this
fixture; replicate it on the reference node). Then:

```sh
go build -o /tmp/facade-bench ./bench/facade/

/tmp/facade-bench \
  --kubeconfig "$HOME/.kube/config" \
  --namespace default \
  --pool default \
  --iterations 20
```

Flags:

| flag | meaning |
| --- | --- |
| `--kubeconfig` | path to the target cluster kubeconfig (required) |
| `--namespace` | namespace to apply the Sandbox in (default `default`) |
| `--name` | name of the Sandbox + bridged claim (default `facade-bench`) |
| `--pool` | the `mitos.run` pool the Sandbox binds to (default `default`) |
| `--image` | podTemplate image (the husk pool pins the real image; this only keeps the manifest valid; default `busybox:stable`) |
| `--iterations` | number of pause/resume toggles to time (default `20`) |
| `--timeout` | per-step timeout waiting for the bridged claim to transition (default `60s`) |

## Adding the upstream reference controller side (bare-metal run)

For the head-to-head, on the reference node:

1. Deploy the upstream controller from `third_party/agent-sandbox` (their helm
   chart / manifests, applied unchanged) into its own namespace.
2. Apply an equivalent upstream Sandbox there (same podTemplate, same replicas
   toggle) and time `replicas 1 -> 0 -> 1` to their Pod Ready, with the same
   timing method.
3. Record both distributions side by side, plus the host (CPU, kernel,
   Firecracker version, image) so the numbers are reproducible and auditable.

Deploying the full upstream controller is out of scope for the shared-CI slice
(it needs a running pod tail that the #18 boundary blocks on kind); it is
documented here for the bare-metal run rather than faked on kind.
