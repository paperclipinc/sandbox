# Running the benchmark harness

`cmd/bench` is the reproducible source behind every latency number the project
publishes. It drives the real KVM-backed fork engine in-process and measures the
fork + vsock + guest-agent data path directly. This page is how to run it on a
real KVM host to reproduce the CI numbers or capture reference-hardware numbers.

For methodology and the meaning of each mode, see
[`../BENCHMARKS.md`](../BENCHMARKS.md).

## Requirements

- A Linux host with `/dev/kvm` (bare metal, or a VM with nested virt). The
  engine validates `/dev/kvm` at construction, so the tool builds and parses
  flags everywhere but only runs the timing path on a KVM host.
- A Firecracker binary (the CI pins v1.15.0).
- A guest kernel (`vmlinux`) and a template snapshot already laid out under the
  data dir (see below).

## Template layout the engine loads from

`cmd/bench --template <id> --data-dir <dir>` forks from a snapshot the engine
expects at:

```
<data-dir>/templates/<id>/snapshot/mem
<data-dir>/templates/<id>/snapshot/vmstate
<data-dir>/templates/<id>/rootfs.ext4      # the backing rootfs
<data-dir>/templates/<id>/verified         # cheap "trusted" marker (see below)
```

The snapshot must have been created with a **relative** vsock `uds_path`
(`vsock.sock`); the engine resolves it against each fork's own working
directory, so a relative path is required for forks not to collide on one host
socket. The rootfs must contain the guest agent as `/init` and a shell, so the
bench's exec (`/bin/sh -c /bin/true` inside the guest) resolves.

The cleanest way to produce this layout is to build the template through the
engine itself (`forkd`'s `CreateTemplate`, which boots the VM, snapshots it,
content-addresses it into the CAS store, and writes the `verified` marker). If
you instead lay out the snapshot files by hand (as the CI bench phase does),
the engine will refuse to fork an unverified snapshot; `touch
<data-dir>/templates/<id>/verified` tells the Fork-time gate the template is
trusted for that run. The full snapshot-create + layout sequence the CI uses is
in the "Bench harness" step of `.github/workflows/kvm-test.yaml`.

## Run it

```sh
go build -o /tmp/bench ./cmd/bench/

# fork -> first exec (cold-claim-shaped)
/tmp/bench \
  --mode fork-exec \
  --template <id> \
  --data-dir <data-dir> \
  --firecracker /usr/local/bin/firecracker \
  --kernel <data-dir>/vmlinux \
  --iterations 100 --warmup 10 \
  --summary --json fork.json

# warm exec round-trip (hot path)
/tmp/bench \
  --mode exec-rt \
  --template <id> \
  --data-dir <data-dir> \
  --firecracker /usr/local/bin/firecracker \
  --kernel <data-dir>/vmlinux \
  --iterations 100 --warmup 10 \
  --summary --json execrt.json
```

`--summary` prints the count/min/p50/p90/p99/max/mean table to stdout. `--json`
writes the same distribution as machine-readable JSON (durations in
nanoseconds) so results can be archived or diffed across hardware.

## Flags

| flag | meaning |
| --- | --- |
| `--mode` | `fork-exec` or `exec-rt` |
| `--iterations` | measured iterations (default 50) |
| `--warmup` | warmup iterations, discarded (default 5) |
| `--template` | template (snapshot) id under the data dir (required) |
| `--data-dir` | data directory holding template snapshots |
| `--firecracker` | Firecracker binary path |
| `--kernel` | guest kernel path |
| `--json` | optional path to write results JSON |
| `--summary` | print the summary table to stdout |

## Capturing reference-hardware numbers

To capture bare-metal reference numbers (roadmap section 4 / issue #15), run the
two modes on the reference node with a higher iteration count (the runs above
use 100), archive both JSON files, and record the host (CPU, kernel, Firecracker
version, rootfs) alongside them so the numbers are reproducible and auditable.
