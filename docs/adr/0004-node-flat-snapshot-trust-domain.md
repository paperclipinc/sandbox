# ADR 0004: node-flat snapshot trust domain

Status: accepted (2026-06-15)
Issue: #18 (W1 husk pods), #30 (residual ADRs). Related: docs/threat-model.md
section 7 (multi-tenancy statement), section 2 (CoW page sharing), section 5
(snapshot integrity), section 0 surface 3 (shared read-only snapshot mount) and
the node-CAS row in section 3; the code is `internal/cas`,
`internal/controller/huskpod.go` (`huskCASMountPath`), `internal/snapcompat`.

## Context

Snapshots are executable memory images: loading one is equivalent to running code
at sandbox privilege. mitos stores them content-addressed in a per-node store
(`internal/cas`, under `<dataDir>`), and the run path shares them across pods on
the same node:

- A node's template snapshot mem/vmstate is mounted READ-ONLY into every husk
  pod that serves that template; all husk pods on a node share the SAME read-only
  snapshot dir (docs/threat-model.md section 0 surface 3).
- All forks of a snapshot share read-only memory pages via `mmap(MAP_PRIVATE)` of
  the same mem file (docs/threat-model.md section 2, CoW page sharing).
- The node content-addressed store (`<dataDir>/cas`) is shared per node across
  all husk pods, mounted READ-WRITE for the W4 workspace path
  (`huskCASMountPath`, docs/threat-model.md section 3 node-CAS row).

Integrity of what is READ is protected: a snapshot is content-addressed by sha256
manifest digest, verified on load against the recorded digest, and checked for
environment compatibility (`internal/snapcompat`) before any Firecracker launch
(docs/threat-model.md section 5). But integrity is NOT the same as ISOLATION, and
the threat model is explicit that there is no per-tenant separation of snapshots
today (section 7): snapshots on a node are a flat directory shared by all tenants,
there is no enforcement that a claim only forks snapshots its namespace published,
and VMs of different namespaces share nodes, host kernel, and forkd.

This needs a recorded decision because it sets the multi-tenancy posture mitos
commits to honestly: what a Kubernetes namespace boundary actually buys around
snapshots today, and what it does not.

## Decision: snapshots are content-addressed and node-shared; treat the whole cluster as one trust domain until per-tenant snapshot isolation lands

We accept, as the current shipped posture, that the snapshot store is FLAT per
node and shared across tenants, protected by content-addressing for integrity but
NOT partitioned for isolation. Concretely:

- A snapshot's identity and integrity are its content address: the sha256
  manifest digest, verified on load and on each husk activate before the image is
  loaded into a VM (docs/threat-model.md section 0 surface 3, section 5). A
  tampered-on-disk or environment-incompatible snapshot is refused fail-closed.
- There is NO per-namespace or per-tenant partition of the snapshot store, and no
  enforcement that a claim may only fork snapshots its own namespace published.
  Cross-namespace objects are therefore NOT a trust boundary for snapshots: a
  Kubernetes namespace buys RBAC on the CRDs and nothing else around snapshots
  (docs/threat-model.md section 7).
- Cross-tenant page sharing must be prevented by NEVER sharing snapshot files
  across trust boundaries; since the store is node-flat, the only safe statement
  today is that the cluster is one trust domain (docs/threat-model.md section 2,
  section 7).

The operating consequence, stated exactly as the threat model states it: until
per-tenant snapshot isolation lands, TREAT THE WHOLE CLUSTER AS ONE TRUST DOMAIN.
Different namespaces are different RBAC scopes, not different security tenants,
where snapshots are concerned. Hard tenant separation is the `dedicatedNodes:
true` pool option (node pools/taints), which is planned and NOT implemented
(docs/threat-model.md section 7).

## Why content-addressing is integrity, not isolation

Content-addressing guarantees that what you READ is what its digest says it is: a
forged or tampered chunk fails the digest check and is rejected. It does NOT
guarantee:

- CONFINEMENT of WRITES on the shared read-write node CAS: a guest that escapes
  its VM into the husk pod can delete, truncate, or corrupt another tenant's
  committed-revision chunks on the same node (a cross-tenant AVAILABILITY attack;
  the read side rejects a forged chunk, but cannot un-delete a destroyed one).
  This is the open/high node-CAS row (docs/threat-model.md section 3).
- SIDE-CHANNEL isolation between forks that share read-only pages of the same
  mem file (docs/threat-model.md section 2, section 8: side-channel resistance
  between forks of the same snapshot is explicitly NOT claimed).
- TENANT scoping: content-addressing dedups by digest across tenants, which is a
  feature for a single trust domain and a hazard across trust boundaries. The
  same digest is the same bytes for everyone on the node.

So content-addressing is necessary for snapshot integrity but does not, by
itself, make the node multi-tenant-safe.

## Consequences

- The honest multi-tenancy claim is bounded: a namespace boundary buys RBAC on
  the CRDs only; it is NOT a snapshot isolation boundary. docs/compliance-claims.md
  and the README must not imply per-namespace snapshot separation.
- The path to lifting "one trust domain" is named and tracked: per-tenant
  snapshot store partitioning (or per-node destruction protection), the
  `dedicatedNodes: true` hard-separation pool option, and removing the
  unverified fork-load path (`--allow-unverified-snapshots` on live forks) are
  the residuals that must close before cross-namespace can be a real tenant
  boundary (docs/threat-model.md section 3 node-CAS row, section 7).
- Any change that would share a snapshot ACROSS what an operator believes is a
  tenant boundary moves the security surface and must update docs/threat-model.md
  in the same PR (the change-discipline rule).
- Operators running untrusted multi-tenant workloads must NOT rely on namespaces
  for snapshot isolation today; the supported answer for hard separation is
  separate clusters (one trust domain each) until `dedicatedNodes` ships.
