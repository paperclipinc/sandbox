# Reversible sleep-consolidation demo

The flagship W4 slice: an agent does work, "sleeps" (consolidates its state into
a resumable head: filesystem state paired with a VM memory snapshot), "wakes"
mid-execution from that head, and the whole thing is reversible because the head
is a pointer over an immutable revision DAG.

This is the user-visible expression of the resumable head. The disk+memory
pairing and the principal-binding refusal are proven end to end in envtest
(`internal/controller/resumable_envtest_test.go`); this demo runs the same flow
on a real cluster through the released surface.

## What it shows

| Stage | Verb (released surface) | What happens |
| --- | --- | --- |
| work | `client.create(workspace=...)`, `sandbox.exec(...)` | a bound sandbox writes state into `/workspace` |
| sleep | `sandbox.terminate(checkpoint=True)` | the work consolidates into a new committed `WorkspaceRevision`; with the memory-snapshot path active the head also pairs a VM memory image (resumable) |
| log | `mitos ws log <ws>` | the head shows as `RESUMABLE` when paired |
| wake | `client.create(workspace=...)` | a fresh claim with the SAME principal resumes from the head; the consolidated state is present |
| revert | `mitos ws revert <ws> <rev>` | the head moves back to a pre-sleep revision; a new claim sees the pre-sleep state (reversible) |

## Run it

```bash
# From a machine with kubectl + the mitos CLI + the Python SDK installed,
# pointed at a running mitos cluster.
examples/sleep-consolidation/run.sh mitos-e2e ~/.kube/config
```

The script prints a `PASS:`/`FAIL:` line per stage and a final summary that names
the mode it ran in (`resumable` or `content-only`).

## The principal binding (security)

A memory image carries secrets-in-RAM, so a resumable head is bound to the
principal that captured it (the capturing claim's `ServiceAccount`). A claim with
a DIFFERENT principal is REFUSED a resume, fail-closed: the memory image is never
served across principals. This refusal is proven in
`internal/controller/resumable_envtest_test.go`
(`TestResumableHeadFromMemorySnapshot`, the `sa-b` intruder case) and enforced at
two layers (`maybeResumeMemory` in the controller and the
`WorkspaceMemorySnapshotAdapter`). The demo runs both phases as the same
principal, so the resume is allowed.

## What is cluster-gated

The REAL VM-memory resume (waking mid-execution from the live memory image)
requires:

1. the controller running with `--workspace-memory-snapshots`, and
2. a KVM-capable node (the live Firecracker memory image is bare-metal, the same
   gate as the husk e2e in `.github/workflows/cluster-e2e.yaml`).

Cluster-verify commands:

```bash
# Controller deployed with the flag (resumable heads enabled):
#   args: [ ..., "--workspace-memory-snapshots" ]
kubectl -n mitos get deploy mitos-controller -o yaml | grep workspace-memory-snapshots

# Run the demo on the KVM cluster and confirm the head is RESUMABLE:
examples/sleep-consolidation/run.sh mitos-e2e ~/.kube/kvm-cluster
mitos -n mitos-e2e ws log sleep-demo   # head row shows RESUMABLE = true
```

Without that gate the filesystem state still round-trips (hydrate/dehydrate is
KVM-proven separately, see `docs/workspaces.md`), so the demo runs in
`content-only` mode: the wake restores the consolidated disk state but not the
live memory image. The script states which mode it ran in; it never claims a
memory resume it did not perform.

## Timing (reproducible)

`bench/sleep-consolidation.sh` measures the wall clock of `sleep`
(checkpoint+dehydrate to head advance) and `wake` (resume+hydrate to Ready) and
records the MODE:

```bash
bench/sleep-consolidation.sh ~/.kube/kvm-cluster mitos-e2e 5
```

Per the repo's no-unverified-claims rule, no sleep/wake number is published here:
record the script's output (with the hardware, cluster, and MODE) into
`bench/results/` when you run it on reference hardware.
