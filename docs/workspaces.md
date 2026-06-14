# Workspaces: the hydrate/dehydrate data path

A `Workspace` is durable, versioned, forkable agent state that lives independent
of any single sandbox. It is not a CSI PersistentVolume; the rationale is in
docs/adr/0002-workspace-not-csi.md. The declarative model (the `Workspace` and
`WorkspaceRevision` CRDs, the revision DAG, retention, lineage) is documented by
the API types in `api/v1alpha1/workspace_types.go`. This page documents slice 2:
how a sandbox binds to a workspace and how its `/workspace` tree moves in and out
over the content-addressed store.

## Binding a sandbox to a workspace

A `SandboxClaim` opts into workspace state with `spec.workspaceRef`:

```yaml
apiVersion: mitos.run/v1alpha1
kind: SandboxClaim
metadata:
  name: agent-session-7
spec:
  poolRef:
    name: python-pool
  workspaceRef:
    name: project-acme
```

A claim without `workspaceRef` is unchanged: its `/workspace` is ephemeral, and
no hydrate or dehydrate runs. A claim with `workspaceRef` participates in the
lifecycle below.

## The data path

```
   start (activate)                         terminate / release
   ----------------                         -------------------
   Workspace.status.head                    guest /workspace
        |                                         |
        v  resolve head revision                  v  TarDir (vsock, allowlisted)
   WorkspaceRevision.contentManifest         tar bytes
        |                                         |
        v  cas.Materialize -> tar                 v  strip secret excludes
   tar bytes                                  cas.PutSnapshot -> digest
        |                                         |
        v  UntarDir (vsock, sanitized)            v  create WorkspaceRevision
   guest /workspace                          {fromClaim, contentManifest, Pending}
                                                  |
                                                  v  Workspace controller commits
                                              head advances
```

- **Hydrate on start.** When a bound claim reaches Ready, the claim reconciler
  resolves the `Workspace`, reads `status.head`, loads that revision's
  `contentManifest`, and hydrates it into the sandbox `/workspace`. An empty
  workspace (no committed head) starts with an empty `/workspace`. Hydration runs
  exactly once per claim (an annotation guards against a requeue re-hydrating over
  in-sandbox edits); a transient transfer error requeues without failing the Ready
  claim.
- **Dehydrate on terminate.** When a bound claim terminates (lifetime/idle expiry
  or deletion), the reconciler dehydrates the sandbox `/workspace` BEFORE reaping
  the VM (the guest must still be alive to tar its workspace), creates a new
  `WorkspaceRevision{spec.workspaceRef, source.fromClaim=<claim>, contentManifest=<digest>, phase Pending}`,
  and the Workspace controller commits it (Pending -> Committed) and advances
  `status.head`. The operation is idempotent: a claim dehydrated by a
  lifetime-expiry terminate is not dehydrated again on the subsequent delete.

## The bulk transfer primitive

The guest agent serves two vsock ops, mirroring the `ReadFile`/`WriteFile`
message pattern:

- `TarDir(path)`: tars a directory and returns the tar bytes. The path is
  restricted to a workspace allowlist (only `/workspace` and paths under it; never
  `/` or any secret/token path). Symlinks and other non-regular entries are
  skipped so a restored symlink can never re-introduce an escape.
- `UntarDir(path, tar)`: extracts a tar into the target, sanitizing every member
  name against traversal (no absolute paths, no `..` escape outside the target)
  and refusing any non-regular member.

The tar is buffered whole on both ends, bounded by `vsock.MaxTarBytes` with a
matching vsock line buffer. A streaming (chunked) transfer for very large
workspaces is a later slice.

The host helpers in `internal/workspace` compose the primitive with the
content-addressed store (`internal/cas`):

- `Dehydrate(ctx, agent, store, excludePaths)`: tars `/workspace`, strips the
  exclude list, unpacks to a temp dir, and `store.PutSnapshot`s it; returns the
  manifest digest. An unchanged tree dedups to the same digest (content
  addressing).
- `Hydrate(ctx, agent, store, manifest)`: materializes the manifest, tars it, and
  `UntarDir`s it into `/workspace`.

## Single-writer-per-workspace

A `Workspace` is bound to at most one active claim at a time. A claim referencing
a workspace already bound to another active claim PENDS with the Ready condition
reason `WorkspaceBusy` and retries; it acquires no VM until the first claim
releases the workspace. This keeps two sandboxes from racing to dehydrate the same
workspace into divergent heads.

## Outputs: capturing only what matters, with a diff

By default the dehydrate-on-terminate captures the whole `/workspace` tree into
the new revision. A claim narrows and enriches that capture with
`spec.outputs`, matching the v2-spec `onTerminate.outputs` shape:

```yaml
apiVersion: mitos.run/v1alpha1
kind: SandboxClaim
metadata:
  name: agent-session-7
spec:
  poolRef: { name: python-pool }
  workspaceRef: { name: project-acme }
  outputs:
    - { path: /workspace/dist }                       # capture only this subtree
    - { diff: true }                                  # record the diff vs the parent head
    - { git: { remote: rendezvous, branch: "attempt/{{.name}}" } }
```

- A `{path}` output narrows the captured revision to that `/workspace` subtree.
  With any `path` output set, only the union of those subtrees enters the
  revision; with no `path` output the whole workspace is captured (the default).
  The filter is a prefix match on the workspace-relative file names, so `dist`
  captures `dist/app.js` but never `distractor/x.txt`.
- A `{diff: true}` output records a content-hash diff of the new revision against
  the workspace head before it on `WorkspaceRevision.status.diffSummary`: the
  added, removed, and modified file names plus counts. Modified means a file
  present in both whose chunk digests differ (its content changed). An unchanged
  tree diffs to empty. This is a content diff, not rename-aware: a rename shows
  as a delete plus an add on the workspace side (git handles renames on the
  repo-paths side).

The secret exclude list still applies to the captured set, so outputs never widen
what a revision may carry.

## Git rendezvous: fork-and-merge through git

A `{git}` output pushes the workspace repo paths to a rendezvous remote on a
per-attempt branch. On terminate, for each `{git}` output the controller resolves
the workspace `spec.git.paths` content from the just-dehydrated revision,
materializes it into a temporary worktree, makes one deterministic commit, and
pushes it to the output's `remote` on a branch rendered from the output's
`branch` template (a `text/template` over `{{.name}}`, the claim name, defaulting
to `attempt/<name>`). The push is recorded on
`WorkspaceRevision.status.gitPushes` (the branch and remote).

GIT IS THE MERGE LAYER. The engine only ever pushes a branch; it never merges
working trees. Fork-and-merge means: fork the workspace, run each attempt in its
own sandbox, push each attempt's repo paths to its own per-attempt branch, and
let a human or CI merge the branches with git. There is no automatic merge by
design.

Honest behavior:

- A `{git}` output on a workspace with no `spec.git.paths` is a no-op with a
  logged warning: there is nothing to push.
- A push failure surfaces on the claim/revision condition and the terminate
  retries it; it is never silently swallowed. The revision and the dehydrated
  marker are made durable first, so a failing push never loses the captured work.
- A `{git}` output is a NEW EGRESS of tenant repo data to an operator-declared
  external remote; see docs/threat-model.md section 3.

The push uses the host `git` CLI via exec, so it adds no new dependency. CI's
Linux runner has git; the controller image must ship git for the production path
(the tests skip gracefully when git is absent so the unit suite is not flaky).

### Authenticating the push to an external remote

A push to a real external rendezvous remote (not a local bare repo) needs
credentials. Set them on the workspace:

```yaml
spec:
  git:
    paths: ["/workspace/repo"]
    credentialsUsername: x-access-token   # not a secret; defaults to x-access-token
    credentialsSecretRef:
      name: rendezvous-creds              # a Secret in the workspace namespace
      key: token                          # the key holding the push token
```

The controller resolves the Secret at push time and hands the token to git. The
token is a SECRET VALUE and is handled accordingly:

- It NEVER appears on the git argv (so it cannot show up in a process table), in
  a log line, in an error, in a status condition, or in a committed revision.
- It reaches git ONLY through a mode `0o600` `.git-credentials` file written into
  an ephemeral, isolated `HOME` that is created per push and removed when the push
  returns (`credential.helper=store` reads only that file).
- Any git output that is surfaced in an error has the token scrubbed defensively
  before it is returned.
- A missing or empty key is reported with an LLM-legible error that names only
  the Secret and the key, never the value.

Credentials require an `https://` (or `http://`) remote that can carry basic
auth; a `file://` or scp-like remote with credentials is rejected.

The matching server side is `cmd/rendezvous-server` (package
`internal/rendezvous`): a minimal authenticated git-http server that wraps the
stock `git http-backend` CGI behind HTTP basic auth, with its token mounted from
a Secret. It creates a bare repo on first push and enables `receive-pack` only
after the auth check passes. Deploy it as the `remote` the `{git}` output targets,
or point the output at any standard authenticated git remote (a forge or an
internal git server). An end-to-end test
(`internal/rendezvous/server_test.go:TestRendezvousCredentialedPushLandsOnServer`)
proves the credentialed push authenticates to this server and lands the branch.

Cluster-gated: the unit and envtest suites prove the credentialed push logic and
the token redaction against a local authenticated server. The push to a REAL
external rendezvous server on a live cluster is the gated tail; see
`test/cluster-e2e/workspace-e2e.sh` (run with
`gh workflow run cluster-e2e.yaml -f suite=workspace`).

This `{git}` egress with Secret credentials is a security-surface move; see
docs/threat-model.md section 3.

## The revision change feed for external indexers

The controller emits a CloudEvents 1.0 feed so an external indexer (a vector DB,
a search index, an audit sink) can react to workspace changes WITHOUT mitos
embedding any indexer of its own. There is no built-in vector store by design
(ROADMAP.md, EPIC W4): indexing is the consumer's job, fed by this event.

A `dev.mitos.workspace.revision.created` event is emitted when a
`WorkspaceRevision` is created. The envelope is a structured-mode CloudEvent
(`internal/eventfeed/event.go`); the `data` payload is `RevisionCreatedData`:

```json
{
  "specversion": "1.0",
  "type": "dev.mitos.workspace.revision.created",
  "source": "mitos.run/controller",
  "subject": "<namespace>/<revision-name>",
  "id": "<stable-dedupe-id>",
  "time": "2026-06-14T00:00:00Z",
  "datacontenttype": "application/json",
  "data": {
    "workspace": "<workspace-name>",
    "revision": "<revision-name>",
    "contentManifest": "<lowercase-hex-sha256-digest>",
    "lineage": "fromClaim:<claim> | fromWorkspaceRevision:<revision> | root",
    "memorySnapshotRef": "<snapshot-id-when-resumable>",
    "traceId": "<orchestrator-trace-id-when-enabled>"
  }
}
```

The payload carries content-addressed POINTERS and lineage NAMES only: a
manifest digest, not content; a snapshot id, not snapshot bytes; never a secret
value. An indexer materializes the revision's files itself from the content store
using the `contentManifest`, then indexes them.

Delivery is via the existing `WebhookSink` (`internal/eventfeed/sink.go`), which
POSTs each event to an operator-configured webhook URL. The `id` is a stable
dedupe key so a re-emit of the same logical event reproduces the same envelope,
letting the consumer dedupe idempotently.

## Secrets are never captured into a revision

Secret values live only in the guest's in-memory configured env (delivered over
the configure message), never on disk under `/workspace`. As defense in depth,
the dehydrate is passed an explicit exclude list
(`controller.WorkspaceSecretExcludePaths`: `.netrc`, `.git-credentials`, `.ssh`,
`.aws`, `.config/gh`, `.npmrc`) so a careless agent that wrote a token to one of
those conventional paths still does not leak it into a committed revision.

## SDK and CLI surface

A user creates, binds, logs, diffs, forks, reverts, and terminates-with-outputs
a workspace through the SDKs and the `mitos ws` CLI without hand-writing a CRD.
The verbs are git-shaped and map onto the revision DAG: a fork is a new
`WorkspaceRevision` whose `source.fromWorkspaceRevision` points at the parent, in
a (possibly new) workspace; a revert is a new tip in the same workspace that
shares a past revision's content. Refusals carry an LLM-legible
`{code, cause, remediation}` (issue #28): forking an uncommitted revision is
`revision_not_committed`.

Python:

```python
from mitos import AgentRun

run = AgentRun(namespace="team-a")
ws = run.create_workspace("proj-x")

# Bind a sandbox to the workspace: the controller hydrates the head into
# /workspace on start and dehydrates a new committed revision on terminate.
sb = run.sandbox(image="python", workspace="proj-x", ready=True)
sb.files.write("/workspace/data.txt", "hello")

# Terminate with outputs: keep only a subtree, record a diff, push repo paths.
sb.terminate(outputs=["/workspace", {"diff": True}], checkpoint=False)

for rev in ws.log():            # newest first
    print(rev.name, rev.phase, rev.lineage)
ws.diff(ws.log()[0].name)       # path-level content-hash diff
branch_rev = ws.fork(ws.log()[0].name, "proj-x-branch")  # content-addressed branch
ws.revert("proj-x-1")           # new tip sharing a past revision's content
```

TypeScript:

```typescript
import { AgentRun, KubeConfigApi } from "@mitos/sdk";

const run = new AgentRun({ k8s: new KubeConfigApi(), namespace: "team-a" });
const ws = await run.createWorkspace("proj-x");

const sb = await run.create("python-pool", { workspace: "proj-x" });
await sb.files.write("/workspace/data.txt", "hello");
await sb.terminate({ outputs: ["/workspace", { diff: true }] });

const revs = await ws.log();    // newest first
await ws.fork(revs[0].name, "proj-x-branch");
await ws.revert("proj-x-1");
```

CLI: `mitos ws create|ls|log|diff|fork|revert|rm|bind` (see `docs/cli.md`).

## Resumable head: pairing the workspace with a VM memory snapshot

A plain terminate dehydrates `/workspace` into a content-only revision. A
`terminate(checkpoint=True)` additionally pairs the new revision with the
sandbox's VM MEMORY snapshot, so the workspace head becomes RESUMABLE: a later
claim bound to the workspace resumes MID-EXECUTION from the captured VM state
(memory image + filesystem state, restored together) instead of a cold start.
This is the "sleep / wake" of the reversible sleep-consolidation demo
(`examples/sleep-consolidation/`).

The pairing is two refs on the committed `WorkspaceRevision`:

- `memorySnapshotRef`: the snapshot pointer (a content address / snapshot id,
  never the memory bytes);
- `memorySnapshotPrincipal`: the principal the image is BOUND to (the capturing
  claim's `ServiceAccount`).

On a new claim's activation the controller resumes the head's memory image ONLY
when (a) the head pairs a snapshot, (b) the snapshot still exists (a GC'd
snapshot degrades to a content-only hydrate), AND (c) the activating claim's
principal MATCHES `memorySnapshotPrincipal`. A principal MISMATCH is REFUSED,
fail-closed (an error, never a silent cold-start downgrade): a memory image
carries secrets-in-RAM and is never served across principals. A resume reseeds
the guest CRNG and steps the wall clock exactly like a fork (see
`docs/fork-correctness.md`); the principal binding is a threat-model row (see
`docs/threat-model.md`).

The disk+memory pairing logic, the resumable status, and the principal-binding
refusal are proven END TO END in envtest behind the checkpoint/resume/exists
seams (`internal/controller/resumable_envtest_test.go`,
`TestResumableHeadFromMemorySnapshot`, including the cross-principal `sa-b`
intruder case). The seams are bound to the husk live-VM snapshot path behind the
controller `--workspace-memory-snapshots` flag
(`internal/controller/workspace_memory_snapshot.go`); off by default a
checkpoint-on-terminate fails loud rather than producing a falsely-resumable
revision.

CLUSTER-GATED: the real bare-metal VM-memory image (a live Firecracker memory
snapshot resuming mid-execution) needs a KVM-capable node, the same gate as the
husk e2e (`.github/workflows/cluster-e2e.yaml`). Cluster-verify:

```bash
# Controller deployed with the flag:
kubectl -n mitos get deploy mitos-controller -o yaml | grep workspace-memory-snapshots
# Run the demo on the KVM cluster, then confirm the head is resumable:
examples/sleep-consolidation/run.sh mitos-e2e ~/.kube/kvm-cluster
mitos -n mitos-e2e ws log sleep-demo   # head row RESUMABLE = true
```

Without the gate the filesystem state still round-trips (hydrate/dehydrate is
KVM-proven), so the head is content-only and a wake restores disk state but not
the live memory image. Timing for the sleep (checkpoint+dehydrate) and wake
(resume+hydrate) phases is reproducible from `bench/sleep-consolidation.sh`; no
number is published until it is recorded from that script (no-unverified-claims).

## Proven vs open

PROVEN:

- The bulk transfer and CAS round trip on KVM: `cmd/ws-smoke` boots two real VMs,
  writes a known tree (nested + binary content + a secret file) into the source
  `/workspace`, Dehydrates to a CAS digest, Hydrates into the destination
  `/workspace`, and asserts every file is byte-identical while the secret is
  excluded (the gated KVM phase in `.github/workflows/kvm-test.yaml`).
- The binding + revision lifecycle in envtest: hydrate-on-activate with the head
  manifest, dehydrate-on-terminate creating a `fromClaim` revision that advances
  the head, single-writer `WorkspaceBusy`, an unbound claim unaffected, and the
  secret exclude list passed to dehydrate (`internal/controller/workspace_binding_test.go`).
- Outputs extraction: the path filter captures only the listed subtrees and the
  content-hash diff detects add/remove/modify (unchanged diffs to empty) in unit
  tests (`internal/workspace/outputs_test.go`); an envtest proves a `{path}`
  output narrows the dehydrate capture and a `{diff: true}` output records the
  diff summary on the new revision.
- Git rendezvous: the push to a LOCAL bare repo lands the per-attempt branch with
  exactly one commit carrying the repo-paths content
  (`internal/workspace/git_test.go`); an envtest proves a `{git}` output renders
  the branch, calls the rendezvous push with the resolved repo files, and records
  the push on the revision status.
- Credentialed git rendezvous: a workspace `spec.git.credentialsSecretRef` is
  resolved and the token delivered to git only through an ephemeral, mode
  `0o600` `.git-credentials` file in an isolated `HOME`, never on argv, in a log,
  in an error, in a condition, or in a revision; redaction on the failure path is
  asserted in unit tests and the token-never-in-conditions invariant in an
  envtest (`internal/workspace/git_test.go`,
  `internal/controller/workspace_binding_test.go`).
- The authenticated rendezvous server: `internal/rendezvous` wraps
  `git http-backend` behind HTTP basic auth (constant-time token compare); a
  unit test rejects an unauthenticated push with 401, accepts an authenticated
  one, and an end-to-end test proves the engine's credentialed push lands the
  branch on the server (`internal/rendezvous/server_test.go`).
- The revision change feed: the `dev.mitos.workspace.revision.created` CloudEvent
  with pointers-and-names-only payload, delivered via `WebhookSink`
  (`internal/eventfeed`, `internal/controller/eventfeed.go`); documented above
  for external indexers (no built-in vector store by design).
- The SDK/CLI surface: the controller-side fork/revert verbs with LLM-legible
  rejection (`internal/controller/workspace_verbs.go`), the `mitos ws` CLI over a
  cluster `WorkspaceBackend` (`internal/agentcli/workspace_{backend,cmd}.go`,
  `clusterbackend.go`), the Python `Workspace` handle plus
  `terminate(outputs=..., checkpoint=...)` (`sdk/python/mitos/workspace.py`), and
  the TypeScript parity (`sdk/typescript/src/workspace.ts`), each unit-tested.
- Workspace store encryption at rest (#31): when `spec.store.encryptionKeyRef` is
  set, every revision chunk and manifest is encrypted with AES-256-GCM under a
  data-encryption key (DEK). The DEK the controller generates is keyed
  per-template (by templateID, `internal/controller/enc_key_secret.go`), so a
  template's workspaces share its DEK; the nonce scheme is digest-keyed and safe
  under a shared key, but isolation granularity is the template. Per-workspace
  crypto-shred is a future option if finer granularity is wanted. It uses the same
  KMS envelope custody as templates; the DEK is unwrapped only in node memory and
  never logged. The manifest digest stays
  computed over plaintext, so an encrypted dehydrate yields the SAME content
  identifier as a plaintext dehydrate (dedup preserved) and the round trip is
  byte-identical with chunks ciphertext at rest; a wrong key fails closed. All
  asserted in `internal/workspace/encryption_test.go`.
- The S3 object-store backend: an S3-compatible bucket
  (`spec.store.s3` + `objectStorageRef`) as an alternative to the node CAS, with
  the same content-addressed interface, the same revision digest for a tree
  (drop-in), byte-identical round trip, dedup by chunk-digest object key, and
  composition with per-workspace encryption (ciphertext at rest in the bucket).
  Credentials come from a referenced Secret; the secret-access-key derives only
  the SigV4 signing key and never appears on the wire in cleartext. Proven in
  `internal/workspace/s3store_test.go` and `s3client_test.go`.
- Workspace benchmarks: `bench/workspace-hydrate-latency.sh` (hydrate /
  dehydrate wall clock, recording the store mode) and
  `bench/workspace-fork-latency.sh` (fork wall clock plus an O(0)-new-bytes
  assertion: the forked revision shares the parent content manifest). Method-only;
  numbers live in `bench/results/` only when reproducible from the scripts.

OPEN (later W4 slices):

- The push to a REAL external rendezvous server on a LIVE cluster is the gated
  tail (`test/cluster-e2e/workspace-e2e.sh`); the credentialed-push logic, the
  token redaction, and the authenticated server are proven in unit + envtest
  above. There is no auto-merge by design: git is the merge layer.
- The fork-sees-committed-state path and the S3 / encrypted round trip on a LIVE
  cluster are the gated tail (`test/cluster-e2e/workspace-e2e.sh`); the round
  trip, the dedup, and the encryption are proven offline in unit tests above.
- The memory-snapshot pairing that produces a resumable head is DONE: the
  disk+memory pairing, the resumable status, and the principal-binding refusal are
  envtest-proven and wired behind `--workspace-memory-snapshots` (see "Resumable
  head" above). OPEN tail: the real bare-metal live-VM memory image resuming
  mid-execution, which is cluster-gated on a KVM node.
- A streaming (non-buffered) tar for very large workspaces (this slice caps and
  buffers; see `vsock.MaxTarBytes`).
- The production controller-to-guest transport wiring for the default
  hydrate/dehydrate path: the lifecycle is proven behind a transfer seam in
  envtest and the helpers are proven on KVM; binding the node-side transport into
  the controller default is the integration follow-up.
