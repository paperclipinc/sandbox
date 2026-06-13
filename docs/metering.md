# CoW-aware metering

This document describes how a node accounts for the memory and disk a sandbox
consumes, why that accounting is copy-on-write (CoW) aware, what is exact versus
approximate, the operational endpoint and metrics that expose it, and how a
hosted service would bill on top of it.

The implementation is `internal/metering` (the aggregation rule) plus the engine
`Metering()` method (`internal/fork`) that fills the samples and the forkd
`GET /v1/metering` endpoint that serves the report.

## Why CoW-aware

Firecracker forks restore the SAME template snapshot with `MAP_PRIVATE`. Every
fork of a template maps the same shared page set; a fork only diverges as it
dirties pages (copy-on-write). So the naive "sum every fork's resident set"
double-counts the shared template region once per fork. On a node running N
forks of one template, the shared region is physically resident ONCE, not N
times.

CoW-aware metering counts each template's shared footprint a single time and
attributes only the unique (private-dirty) pages to each fork. The marginal
physical cost of an additional fork is its unique set, not a whole VM. This is
both the honest capacity number the scheduler must use and the basis for honest
billing.

## What is metered

### Memory (from `/proc/<pid>/smaps_rollup`)

For each sandbox the engine reads the Firecracker process's `smaps_rollup`:

- **MemoryUnique** = `Private_Clean` + `Private_Dirty`. Pages this fork alone
  owns. Never shared, so never deduplicated.
- **MemoryShared** = `Shared_Clean` + `Shared_Dirty`. Pages mapped from the
  shared template snapshot (and any other shared mappings).

### Disk (apparent sizes via `os.Stat`)

Disk is accounted per volume by the volume's fork policy (`internal/volume`):

- **Fresh** / **Clone**: the per-fork backing is counted wholly as the fork's
  `DiskUnique` (Fresh is an empty per-fork backing, Clone is a full byte-for-byte
  copy; neither shares blocks with siblings).
- **Share**: the volume maps the template seed directly. The seed is counted as
  `DiskShared` and there is no per-fork backing.
- **Snapshot** (reflink CoW): the volume reflinks the template seed, so it shares
  the seed (`DiskShared` = seed apparent size) and the fork's divergence is
  approximated as `max(0, forkBackingApparentSize - seedApparentSize)` counted as
  `DiskUnique`.

Volumes are documented in [`volumes.md`](volumes.md).

## The aggregation rule

`metering.Aggregate` folds the per-sandbox samples into a node `Report`:

- Sandboxes are grouped by **Template** (the snapshot id the fork was restored
  from). A sandbox with an empty Template is its own single-member group, so it
  never shares with anything.
- Each group's shared footprint is counted **once** using the **MAX** of the
  group's members as the representative (`SharedOnce` for memory,
  `DiskSharedOnce` for disk). The max is the conservative representative: all
  forks of a template should map approximately the same shared set, so the
  largest observed is a safe single charge.
- The node totals:
  - **TotalUnique** = sum of every sample's MemoryUnique (never deduplicated).
  - **UsedCoWAware** = TotalUnique + sum over templates of each template's
    SharedOnce. The honest resident footprint.
  - **UsedNaive** = TotalUnique + sum of every sample's MemoryShared (the shared
    region double-counted), kept for comparison.
  - **CoWSavings** = UsedNaive - UsedCoWAware: exactly the shared bytes the CoW
    model reveals are NOT consumed per fork.
  - The `Disk*` totals mirror the memory totals for backing storage.

`GetCapacity` (the hot heartbeat path, memory only) reports `MemoryUsed =
UsedCoWAware` and `MemoryShared = SharedOnceTotal` so the scheduler sees the
honest resident footprint and never double-counts the shared template region
across forks.

## Exact vs approximate

**Exact:**

- Per-process **unique** memory (`Private_Clean` + `Private_Dirty` from
  `smaps_rollup`). This is the kernel's own per-process private-page accounting.

**Approximate:**

- The **shared-once representative**. We charge a template's shared set once
  using the MAX of its forks' `MemoryShared`. This is a conservative single
  charge, not a per-page intersection of what the forks actually still share. If
  forks diverge in which shared pages they retain, the true shared resident set
  could be smaller than the representative; the representative never undercounts.
- **Disk divergence** for reflink (Snapshot) volumes. We use **apparent** file
  sizes (`os.Stat` logical length), not allocated blocks. A reflinked fork that
  has rewritten few blocks has a large apparent size but a small physical
  divergence; the apparent-size subtraction overstates the unique disk. Precise
  block-level accounting is open (see below).

## Endpoint and metrics

### `GET /v1/metering` (forkd operational API)

forkd serves the full node `Report` as JSON on the operational mux. This is
operator/billing data in the same access class as `/metrics` and `/healthz`: it
is NOT behind the per-sandbox bearer token. The report holds only sandbox ids,
template names, and byte counts; it never contains secret values.

The JSON shape is the `metering.Report`: `Sandboxes[]` (per-sandbox
ID/Template/MemoryUnique/MemoryShared/DiskUnique/DiskShared), `Templates[]`
(Template/ForkCount/SharedOnce/DiskSharedOnce), and the node totals
(`UsedCoWAware`, `UsedNaive`, `CoWSavings`, `TotalUnique` and the `Disk*`
counterparts).

### Prometheus metrics

The forkd `/metrics` gauges are CoW-aware (`internal/daemon`):

- `mitos_memory_shared_bytes`: CoW-aware shared memory, each template's shared
  page set counted once (the shared-once total).
- `mitos_memory_unique_bytes`: per-fork unique memory summed over all
  sandboxes.
- `mitos_cow_memory_savings_bytes`: memory the CoW model reveals is not
  consumed per fork (naive minus CoW-aware).
- `mitos_metered_disk_bytes`: CoW-aware metered backing storage (template
  volume seeds counted once).

Observability is documented in [`observability.md`](observability.md).

## How a hosted service would bill

The honest unit is **unique footprint + an amortized share of the template**:

- Each sandbox is charged its **unique** memory and disk (the part it alone
  causes to be resident or allocated): this is exact.
- The template's **shared-once** set is charged once per template and amortized
  across the tenants/forks that use it (it is paid once physically, so it should
  be billed once and split, not charged in full to each fork).

`CoWSavings` is the number that makes the CoW value legible: it is the footprint
a per-VM billing model would charge that the CoW model shows is not actually
consumed.

## Open

- **Precise reflink/btrfs block accounting**: replace apparent-size disk
  divergence with allocated-block accounting (e.g. `FIEMAP` / btrfs extent
  sharing) so reflinked volumes report physical divergence, not logical length.
- **PSS-based attribution**: use proportional set size to split shared pages
  across forks instead of the conservative max-once representative.
- **KSM / same-page merging across distinct templates**: today sharing is only
  recognized within a template group; kernel same-page merging across templates
  is not accounted.
- **Per-tenant / per-workspace rollups**: aggregate the node report by tenant,
  tied to Workspace ([#21](https://github.com/paperclipinc/mitos/issues/21)).
- **Billing export / OpenCost integration** and **historical time series**:
  the metrics feed Prometheus today; dashboards and export are follow-ups
  ([#29](https://github.com/paperclipinc/mitos/issues/29),
  [#18](https://github.com/paperclipinc/mitos/issues/18)).

The CoW density datapoint and its CI proof are in
[`../BENCHMARKS.md`](../BENCHMARKS.md) (the metering CI phase forks 4 sandboxes
from one template and asserts the shared region is counted once).
