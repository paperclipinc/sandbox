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

## Secrets are never captured into a revision

Secret values live only in the guest's in-memory configured env (delivered over
the configure message), never on disk under `/workspace`. As defense in depth,
the dehydrate is passed an explicit exclude list
(`controller.WorkspaceSecretExcludePaths`: `.netrc`, `.git-credentials`, `.ssh`,
`.aws`, `.config/gh`, `.npmrc`) so a careless agent that wrote a token to one of
those conventional paths still does not leak it into a committed revision.

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

OPEN (later W4 slices):

- The per-workspace encryption key (#31).
- The S3 / object-storage store backend (this slice uses the node CAS).
- A real external rendezvous server and its credentials (a referenced Secret,
  principal-bound); this slice proves the push against a local bare repo with no
  auth. There is no auto-merge by design: git is the merge layer.
- The SDK/CLI `terminate(outputs=...)` surface and the `mitos ws
  log|diff|revert|branch` verbs (a separate workstream surface).
- The CloudEvents revision change feed and the memory-snapshot pairing that
  produces a resumable head from a real checkpoint (slice 4).
- A streaming (non-buffered) tar for very large workspaces (this slice caps and
  buffers; see `vsock.MaxTarBytes`).
- The production controller-to-guest transport wiring for the default
  hydrate/dehydrate path: the lifecycle is proven behind a transfer seam in
  envtest and the helpers are proven on KVM; binding the node-side transport into
  the controller default is the integration follow-up.
