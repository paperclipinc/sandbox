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
apiVersion: agentrun.dev/v1alpha1
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

OPEN (later W4 slices):

- The per-workspace encryption key (#31).
- The S3 / object-storage store backend (this slice uses the node CAS).
- Outputs extraction and git rendezvous for fork-and-merge (slice 3).
- The CloudEvents revision change feed and the memory-snapshot pairing that
  produces a resumable head from a real checkpoint (slice 4).
- A streaming (non-buffered) tar for very large workspaces (this slice caps and
  buffers; see `vsock.MaxTarBytes`).
- The production controller-to-guest transport wiring for the default
  hydrate/dehydrate path: the lifecycle is proven behind a transfer seam in
  envtest and the helpers are proven on KVM; binding the node-side transport into
  the controller default is the integration follow-up.
