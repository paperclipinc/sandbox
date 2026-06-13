# Conditions and reason-code catalogue

This is the NORMATIVE catalogue of the typed conditions and their reason codes
across the mitos.run CRDs. It is a document, not a wiki page: a reason code
is part of the API contract, and a change here is an API change. Tooling, the
SDK, and dashboards key off these reasons; do not rename one without a
deprecation note.

Every reconciler sets a `Ready` condition (type `Ready`) with `status`
(`True`/`False`), an `observedGeneration` matching the object's `generation`,
and one of the reason codes below. Condition `message` is human/LLM-legible and
carries remediation; it is not part of the contract and may change.

## Workspace (`mitos.run/v1alpha1`)

Condition type `Ready`. The reconciler computes `status.head` (the latest
committed revision, ordered by creationTimestamp then name),
`status.revisions` (the committed revision count), and `status.resumable` (the
head pairs with a memory snapshot).

| Reason | Status | Meaning |
| --- | --- | --- |
| `WorkspaceReady` | True | The model is valid: every revision's lineage resolves and head/revisions/resumable are computed. |
| `WorkspacePending` | False | No committed revision yet (the workspace has no head). |
| `WorkspaceDegraded` | False | A revision has a broken `fromWorkspaceRevision` lineage edge (a parent that does not resolve to a revision in the same workspace). |

## WorkspaceRevision (`mitos.run/v1alpha1`)

Condition type `Ready`, mirrored by `status.phase` (`Pending`/`Committed`). A
revision commits when its `contentManifest` is a valid content-addressed digest;
once committed it is immutable (single-writer-per-revision).

| Reason | Status | Phase | Meaning |
| --- | --- | --- | --- |
| `RevisionCommitted` | True | `Committed` | `contentManifest` is a valid content-addressed digest; the revision is frozen. |
| `RevisionPending` | False | `Pending` | Awaiting a valid `contentManifest` from dehydrate, or the revision's lineage edge does not resolve. |

## SandboxClaim, SandboxPool, SandboxFork (`mitos.run/v1alpha1`)

Existing reason codes, recorded here so the catalogue is complete. See the
respective reconcilers in `internal/controller` for the precise emission points.

| Reason | Kind(s) | Meaning |
| --- | --- | --- |
| `SnapshotsReady` | SandboxPool | The pool's template snapshot is built on the desired number of holder nodes. |
| `HuskPodsReady` | SandboxPool | The warm husk pod pool is at the desired replica count with at least one snapshot node. |
| `HuskActivated` | SandboxClaim | A dormant husk pod was activated in place for the claim. |
| `ActivateFailed` | SandboxClaim | Activating a husk pod failed; the claim re-pends. |
| `HuskPodRaced` | SandboxClaim | Two claims raced for the same dormant husk pod; this one lost and retries. |
| `NoHuskPod` | SandboxClaim | No dormant husk pod was available to activate. |
| `NoCapacity` / `CapacityExhausted` | SandboxClaim | No node had capacity to admit the sandbox before the pending deadline. |
| `NodeLost` | SandboxClaim | The node backing an active sandbox was lost (drain, eviction, deletion). |
| `SecretInheritanceDenied` | SandboxFork | A fork was rejected because the source claim holds secrets and inheritance was not explicitly opted into. |
| `ExplicitOptIn` | SandboxFork | Secret inheritance was explicitly permitted on the fork. |
| `Forked` / `ForksCreated` | SandboxFork | The requested forks were created. |
| `WorkspaceBusy` | SandboxClaim | Another writer holds the single-writer-per-workspace lock for the claim's target workspace; this claim waits and retries until the first writer releases it. |

### Operator actions per SandboxClaim reason

The `Ready=False` SandboxClaim reasons above are not all the same severity. The
catalogue is the normative reference the alerts and runbooks cite (see
`deploy/monitoring/` and `docs/runbooks/`).

| Reason | Status | Operator action |
| --- | --- | --- |
| `HuskActivated` | True | None; a dormant husk pod was activated in place. |
| `ActivateFailed` | False | Transient; the claim re-pends. If sustained, check forkd and KVM health on the holder node (the ClaimErrorRateHigh `reason="fork"` runbook). |
| `HuskPodRaced` | False | None; two claims raced for one husk pod, the loser retries. Benign under load. |
| `NoHuskPod` | False | Warm pool is empty for this claim's pool; scale the SandboxPool warm count (the WarmPoolStarved runbook). |
| `NoCapacity` / `CapacityExhausted` | False | No node had admission capacity before the pending deadline; add capacity or scale pools (the ClaimsPendingSustained runbook). |
| `NodeLost` | False | The backing node was lost (drain, eviction, deletion); the claim re-places. Confirm the node and recover it if unexpected. |
| `WorkspaceBusy` | False | None; the claim waits on the single-writer-per-workspace lock and retries. Investigate only if a writer never releases it. |
