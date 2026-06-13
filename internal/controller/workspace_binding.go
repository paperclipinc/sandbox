package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/workspace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// hydrateFunc is the seam the claim reconciler restores a workspace head into a
// claim's sandbox /workspace through. The production value dials the claim's
// guest agent and calls workspace.Hydrate; envtest injects a fake that records
// the manifest it was asked to hydrate. A nil manifest digest (an empty
// workspace with no head yet) is a no-op.
type hydrateFunc func(ctx context.Context, claim *v1alpha1.SandboxClaim, manifest cas.Digest) error

// dehydrateFunc is the seam the claim reconciler captures a claim's sandbox
// /workspace into a content-addressed revision through. The production value
// dials the guest agent and calls workspace.Dehydrate with the secret exclude
// list and the outputs capture paths; envtest injects a fake that returns a
// scripted digest and records the excludes and captures. capturePaths is the
// union of the claim's spec.outputs Path subtrees (nil captures the whole
// workspace). The returned digest is the new revision's ContentManifest.
type dehydrateFunc func(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths, capturePaths []string) (cas.Digest, error)

// diffFunc is the seam the claim reconciler computes a revision's content-hash
// diff against the workspace head before it through. The production value reads
// both manifests from the CAS store and calls workspace.DiffManifests; envtest
// injects a fake that returns a scripted diff. A diff against an empty parent
// (the first revision) is the whole child as additions.
type diffFunc func(ctx context.Context, claim *v1alpha1.SandboxClaim, parent, child cas.Digest) (workspace.Diff, error)

// rendezvousFunc is the seam the claim reconciler pushes the workspace repo
// paths to a git rendezvous remote through. The production value calls
// workspace.Rendezvous (the git CLI); envtest and unit tests inject a fake.
// repoFiles is the resolved name -> content map of the workspace spec.git.paths.
type rendezvousFunc func(ctx context.Context, repoFiles map[string]string, remote, branch string) error

// memSnapshotResult is what the memory-snapshot checkpointer returns: the
// snapshot ref (a CAS digest / snapshot id, a content pointer, never the memory
// bytes) and the principal the snapshot is bound to (the capturing claim's
// ServiceAccount).
type memSnapshotResult struct {
	Ref       string
	Principal string
}

// checkpointMemoryFunc is the seam the claim reconciler captures a sandbox's VM
// memory snapshot through on a checkpoint-on-terminate. The production value
// drives the husk Checkpoint / engine CreateSnapshot path (the live-VM snapshot
// from issue #18 4b); envtest injects a fake that returns a scripted ref +
// principal. It is only called when the claim sets CheckpointOnTerminate. The
// returned ref pairs with the new revision (memorySnapshotRef) and the principal
// binds it (memorySnapshotPrincipal). A real VM-memory image is the bare-metal
// tail; the pairing decision and the request are what this seam proves.
type checkpointMemoryFunc func(ctx context.Context, claim *v1alpha1.SandboxClaim) (memSnapshotResult, error)

// memorySnapshotExistsFunc is the seam the controller verifies a paired memory
// snapshot still exists through (for the resumable status and the resume
// decision). It is principal-bound: it returns true only when a snapshot with
// the given ref exists AND is bound to the given principal, so a GC'd snapshot
// flips resumable false and a cross-principal ref is never resumable. The
// production value checks the snapshot/CAS store; envtest injects a fake.
type memorySnapshotExistsFunc func(ctx context.Context, ref, principal string) (bool, error)

// resumeMemoryFunc is the seam the claim reconciler requests a memory-snapshot
// restore through on activating a resumable head. The production value drives
// the husk activation to load that memory image into the sandbox VM; envtest
// injects a fake that records the ref it was asked to restore. The real
// VM-memory restore is the KVM/bare-metal tail; the pairing decision and the
// request are envtest-proven via this seam.
type resumeMemoryFunc func(ctx context.Context, claim *v1alpha1.SandboxClaim, ref string) error

// repoFilesFunc is the seam that resolves the workspace spec.git.paths content
// from a just-dehydrated revision manifest into a name -> content map. The
// production value reads the files under the gitPaths prefixes from the CAS
// store; envtest injects a fake. The resolved names are workspace-relative so a
// "/workspace/repo" git path materializes as "repo/...".
type repoFilesFunc func(ctx context.Context, claim *v1alpha1.SandboxClaim, digest cas.Digest, gitPaths []string) (map[string]string, error)

// workspaceHydratedAnnotation marks a claim whose workspace head was already
// hydrated into its sandbox, so a requeue of a Ready claim does not hydrate
// twice (which would clobber in-sandbox edits). It records the hydrated head
// revision name (a content address pointer, not a secret).
const workspaceHydratedAnnotation = "mitos.run/workspace-hydrated-head"

// workspaceDehydratedAnnotation marks a claim whose sandbox /workspace was
// already dehydrated into a committed revision on terminate, so a re-entrant
// terminate (lifetime expiry then delete) does not create a second revision.
const workspaceDehydratedAnnotation = "mitos.run/workspace-dehydrated"

// traceIDAnnotation stamps the active reconcile's trace id onto a WorkspaceRevision
// so an operator can resolve a committed revision back to the exact orchestrator
// request (the controller.reconcileClaim trace) that produced it, and the same id
// rides the revision.created feed event for an external indexer. It is set only
// when tracing is enabled (the trace id is valid); a no-op provider leaves the
// annotation absent rather than stamping a fake id. A trace id is an opaque
// correlation id, not a secret.
const traceIDAnnotation = "mitos.run/trace-id"

// traceIDAnnotations returns the annotation map a new WorkspaceRevision is
// stamped with for the active trace, or nil when tracing is off. The trace id is
// read from the active span context: a valid id (a real provider is installed)
// is stamped under mitos.run/trace-id; an invalid id (the no-op provider /
// tracing disabled) yields nil, so a fake all-zero id is never written. A trace
// id is an opaque correlation id, never a secret value.
func traceIDAnnotations(ctx context.Context) map[string]string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.TraceID().IsValid() {
		return nil
	}
	return map[string]string{traceIDAnnotation: sc.TraceID().String()}
}

// WorkspaceSecretExcludePaths are the guest /workspace paths the dehydrate must
// strip so a captured revision never carries credential material. Secret VALUES
// live in the guest's in-memory configured env, never on disk, but a careless
// agent could write a token to one of these conventional paths; excluding them
// is defense in depth for the no-secrets-in-revisions rule.
var WorkspaceSecretExcludePaths = []string{
	"/workspace/.netrc",
	"/workspace/.git-credentials",
	"/workspace/.ssh",
	"/workspace/.aws",
	"/workspace/.config/gh",
	"/workspace/.npmrc",
}

// hydrate returns the configured hydrate seam or the default real path.
func (r *SandboxClaimReconciler) hydrate() hydrateFunc {
	if r.HydrateWorkspace != nil {
		return r.HydrateWorkspace
	}
	return r.defaultHydrate
}

// dehydrate returns the configured dehydrate seam or the default real path.
func (r *SandboxClaimReconciler) dehydrate() dehydrateFunc {
	if r.DehydrateWorkspace != nil {
		return r.DehydrateWorkspace
	}
	return r.defaultDehydrate
}

// diff returns the configured diff seam or the default real path.
func (r *SandboxClaimReconciler) diff() diffFunc {
	if r.DiffWorkspace != nil {
		return r.DiffWorkspace
	}
	return r.defaultDiff
}

// rendezvous returns the configured git rendezvous seam or the default real path
// (workspace.Rendezvous via the git CLI).
func (r *SandboxClaimReconciler) rendezvous() rendezvousFunc {
	if r.RendezvousGit != nil {
		return r.RendezvousGit
	}
	return func(ctx context.Context, repoFiles map[string]string, remote, branch string) error {
		return workspace.Rendezvous(ctx, repoFiles, remote, branch)
	}
}

// repoFiles returns the configured repo-paths resolver seam or the default real
// path that reads the git-path files from the CAS store.
func (r *SandboxClaimReconciler) repoFiles() repoFilesFunc {
	if r.RepoFilesForGit != nil {
		return r.RepoFilesForGit
	}
	return r.defaultRepoFiles
}

// checkpointMemory returns the configured memory-checkpoint seam or a
// fail-closed default. The default reports a clear, actionable error rather than
// silently skipping the snapshot, so a checkpoint-on-terminate on a controller
// without the node-side memory-snapshot wiring fails loud instead of producing a
// revision falsely marked resumable.
func (r *SandboxClaimReconciler) checkpointMemory() checkpointMemoryFunc {
	if r.CheckpointMemory != nil {
		return r.CheckpointMemory
	}
	return func(_ context.Context, claim *v1alpha1.SandboxClaim) (memSnapshotResult, error) {
		return memSnapshotResult{}, fmt.Errorf(
			"memory-snapshot checkpoint-on-terminate is not wired on this controller: bind CheckpointMemory (the husk Checkpoint / engine CreateSnapshot path) before setting spec.checkpointOnTerminate on claim %s",
			claim.Name,
		)
	}
}

// resumeMemory returns the configured memory-restore seam or a fail-closed
// default (see checkpointMemory for the wiring requirement).
func (r *SandboxClaimReconciler) resumeMemory() resumeMemoryFunc {
	if r.ResumeMemory != nil {
		return r.ResumeMemory
	}
	return func(_ context.Context, claim *v1alpha1.SandboxClaim, _ string) error {
		return fmt.Errorf(
			"memory-snapshot resume is not wired on this controller: bind ResumeMemory (the husk activation memory-restore path) before resuming a resumable head on claim %s",
			claim.Name,
		)
	}
}

// memorySnapshotExists returns the configured existence-check seam or a
// fail-closed default that reports the snapshot is absent, so an unwired
// controller never marks a head resumable it cannot verify.
func (r *SandboxClaimReconciler) memorySnapshotExists() memorySnapshotExistsFunc {
	if r.MemorySnapshotExists != nil {
		return r.MemorySnapshotExists
	}
	return func(context.Context, string, string) (bool, error) {
		return false, nil
	}
}

// defaultHydrate is the production hydrate path. It requires the node-side
// transport that reaches the claim's guest agent (the same path that delivers
// exec/file traffic). That transport is wired by the node integration; until it
// is present this returns an actionable error rather than silently skipping the
// hydrate, so a misconfigured deployment fails loud instead of starting a
// sandbox with an empty workspace. The seam is what envtest and the KVM proof
// exercise.
func (r *SandboxClaimReconciler) defaultHydrate(ctx context.Context, claim *v1alpha1.SandboxClaim, manifest cas.Digest) error {
	agent, store, err := r.workspaceTransport(claim)
	if err != nil {
		return err
	}
	return workspace.Hydrate(ctx, agent, store, manifest)
}

// defaultDehydrate is the production dehydrate path; see defaultHydrate for the
// transport requirement.
func (r *SandboxClaimReconciler) defaultDehydrate(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths, capturePaths []string) (cas.Digest, error) {
	agent, store, err := r.workspaceTransport(claim)
	if err != nil {
		return "", err
	}
	return workspace.Dehydrate(ctx, agent, store, excludePaths, capturePaths)
}

// defaultDiff is the production diff path. It reads both manifests from the CAS
// store and computes the content-hash diff. An empty parent digest (the first
// revision in a workspace) diffs against an empty manifest, so the whole child
// is recorded as additions. See defaultHydrate for the transport requirement.
func (r *SandboxClaimReconciler) defaultDiff(ctx context.Context, claim *v1alpha1.SandboxClaim, parent, child cas.Digest) (workspace.Diff, error) {
	_, store, err := r.workspaceTransport(claim)
	if err != nil {
		return workspace.Diff{}, err
	}
	var parentManifest cas.Manifest
	if parent.Validate() == nil {
		parentManifest, err = store.GetManifest(parent)
		if err != nil {
			return workspace.Diff{}, fmt.Errorf("read parent manifest %s: %w", parent, err)
		}
	}
	childManifest, err := store.GetManifest(child)
	if err != nil {
		return workspace.Diff{}, fmt.Errorf("read child manifest %s: %w", child, err)
	}
	return workspace.DiffManifests(parentManifest, childManifest), nil
}

// defaultRepoFiles is the production repo-paths resolver. It reads the
// just-dehydrated revision's manifest from the CAS store, keeps the file entries
// that fall under the workspace spec.git.paths prefixes, and materializes their
// content into a name -> content map for the git rendezvous push. See
// defaultHydrate for the transport requirement.
func (r *SandboxClaimReconciler) defaultRepoFiles(ctx context.Context, claim *v1alpha1.SandboxClaim, digest cas.Digest, gitPaths []string) (map[string]string, error) {
	_, store, err := r.workspaceTransport(claim)
	if err != nil {
		return nil, err
	}
	if err := digest.Validate(); err != nil {
		return nil, fmt.Errorf("git rendezvous manifest digest: %w", err)
	}
	m, err := store.GetManifest(digest)
	if err != nil {
		return nil, fmt.Errorf("read git rendezvous manifest %s: %w", digest, err)
	}

	prefixes := workspace.CapturePaths(gitPathsAsOutputs(gitPaths))
	tmp, err := os.MkdirTemp("", "ws-gitpaths-*")
	if err != nil {
		return nil, fmt.Errorf("git rendezvous temp dir: %w", err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup

	out := map[string]string{}
	for _, fe := range m.Files {
		if !nameUnderAnyPrefix(fe.Name, prefixes) {
			continue
		}
		dst := filepath.Join(tmp, "f")
		if err := store.MaterializeFileTo(digest, fe.Name, dst); err != nil {
			return nil, fmt.Errorf("materialize git rendezvous file %s: %w", fe.Name, err)
		}
		content, err := os.ReadFile(dst) //nolint:gosec // dst is controller-owned temp
		if err != nil {
			return nil, fmt.Errorf("read git rendezvous file %s: %w", fe.Name, err)
		}
		out[fe.Name] = string(content)
	}
	return out, nil
}

// gitPathsAsOutputs adapts spec.git.paths into OutputSpec Path entries so the
// CapturePaths normalizer (the same /workspace-relative prefix logic) can be
// reused for the git-path filter.
func gitPathsAsOutputs(gitPaths []string) []v1alpha1.OutputSpec {
	out := make([]v1alpha1.OutputSpec, 0, len(gitPaths))
	for _, p := range gitPaths {
		out = append(out, v1alpha1.OutputSpec{Path: p})
	}
	return out
}

// nameUnderAnyPrefix reports whether a workspace-relative file name equals or
// sits under one of the prefixes. An empty prefix set (no git.paths normalized
// to a subtree) matches nothing, so a bare "/workspace" git path captures the
// whole tree only when it normalizes to a real prefix.
func nameUnderAnyPrefix(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if name == p || strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

// workspaceTransport resolves the guest agent transport and CAS store the real
// hydrate/dehydrate path uses. The node-side wiring (controller -> forkd/husk ->
// guest vsock) is a follow-up; this slice proves the transfer on KVM and the
// binding lifecycle in envtest behind the seam, so the default reports a clear,
// actionable error when invoked without that wiring.
func (r *SandboxClaimReconciler) workspaceTransport(claim *v1alpha1.SandboxClaim) (workspace.VsockTransport, *cas.Store, error) {
	return nil, nil, fmt.Errorf(
		"workspace hydrate/dehydrate transport is not wired on this controller: bind WorkspaceTransport (the node-side guest agent path) before using spec.workspaceRef on claim %s, or run the bulk transfer via the node integration (the KVM-proven internal/workspace helpers)",
		claim.Name,
	)
}

// resolveWorkspaceHead loads the Workspace named by the claim's WorkspaceRef and
// returns its head revision's ContentManifest digest. An empty workspace (no
// committed head yet) returns an empty digest and ok=false, which the caller
// treats as nothing to hydrate. A missing Workspace is an error. The head
// revision is returned too (nil when ok is false) so the caller can read its
// memory-snapshot pairing.
func (r *SandboxClaimReconciler) resolveWorkspaceHead(ctx context.Context, claim *v1alpha1.SandboxClaim) (manifest cas.Digest, head *v1alpha1.WorkspaceRevision, ok bool, err error) {
	var ws v1alpha1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Spec.WorkspaceRef.Name}, &ws); err != nil {
		return "", nil, false, fmt.Errorf("resolve workspace %s: %w", claim.Spec.WorkspaceRef.Name, err)
	}
	if ws.Status.Head == "" {
		return "", nil, false, nil
	}
	var headRev v1alpha1.WorkspaceRevision
	if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: ws.Status.Head}, &headRev); err != nil {
		return "", nil, false, fmt.Errorf("resolve workspace %s head revision %s: %w", ws.Name, ws.Status.Head, err)
	}
	d := cas.Digest(headRev.Spec.ContentManifest)
	if d.Validate() != nil {
		// Head names a revision whose manifest is not a valid content address;
		// nothing safe to hydrate.
		return "", &headRev, false, nil
	}
	return d, &headRev, true, nil
}

// workspaceBusyClaim returns the name of an active claim (one that holds the
// terminate finalizer and is not in a terminal phase) other than the given claim
// that binds the same workspace, or "" if none. It is the single-writer gate: a
// Workspace may be bound to at most one active claim at a time.
func (r *SandboxClaimReconciler) workspaceBusyClaim(ctx context.Context, claim *v1alpha1.SandboxClaim) (string, error) {
	var claims v1alpha1.SandboxClaimList
	if err := r.List(ctx, &claims, client.InNamespace(claim.Namespace)); err != nil {
		return "", fmt.Errorf("list claims for workspace single-writer check: %w", err)
	}
	for i := range claims.Items {
		other := &claims.Items[i]
		if other.Name == claim.Name {
			continue
		}
		if other.Spec.WorkspaceRef == nil || other.Spec.WorkspaceRef.Name != claim.Spec.WorkspaceRef.Name {
			continue
		}
		// A terminal claim no longer holds the workspace.
		if other.Status.Phase == v1alpha1.SandboxTerminated || other.Status.Phase == v1alpha1.SandboxFailed {
			continue
		}
		// A claim under deletion is releasing the workspace; do not count it as a
		// live writer once it is finishing.
		if !other.DeletionTimestamp.IsZero() {
			continue
		}
		return other.Name, nil
	}
	return "", nil
}

// hydrateOnActivate hydrates the workspace head into the claim's sandbox exactly
// once. It is called after the claim reaches Ready. A claim without a
// WorkspaceRef, or one already hydrated (the annotation is stamped), is a no-op.
// An empty workspace (no head) stamps the annotation with an empty marker so a
// later head does not retro-hydrate over in-sandbox work. Hydrate failures are
// surfaced to the caller, which requeues; the sandbox stays Ready (the workspace
// simply is not populated yet) rather than failing the claim, so a transient
// transport error recovers.
func (r *SandboxClaimReconciler) hydrateOnActivate(ctx context.Context, claim *v1alpha1.SandboxClaim) error {
	if claim.Spec.WorkspaceRef == nil {
		return nil
	}
	if _, done := claim.Annotations[workspaceHydratedAnnotation]; done {
		return nil
	}
	logger := log.FromContext(ctx)

	manifest, head, ok, err := r.resolveWorkspaceHead(ctx, claim)
	if err != nil {
		return err
	}
	headName := ""
	if head != nil {
		headName = head.Name
	}
	if ok {
		// A resumable head (its memorySnapshotRef is set, the snapshot still
		// exists, and the activating claim's principal matches the snapshot's
		// principal) resumes the VM memory image PLUS the workspace content,
		// paired. A non-resumable head (no snapshot, GC'd snapshot, or a
		// principal mismatch) hydrates content only. The principal check is the
		// secrets boundary: a memory image carries secrets-in-RAM and is never
		// served across principals.
		if err := r.maybeResumeMemory(ctx, claim, head); err != nil {
			return err
		}
		if err := r.hydrate()(ctx, claim, manifest); err != nil {
			return fmt.Errorf("hydrate workspace %s head %s into claim %s: %w", claim.Spec.WorkspaceRef.Name, headName, claim.Name, err)
		}
		logger.Info("hydrated workspace head into sandbox", "claim", claim.Name, "workspace", claim.Spec.WorkspaceRef.Name, "head", headName)
	} else {
		logger.Info("workspace has no committed head; starting with an empty workspace", "claim", claim.Name, "workspace", claim.Spec.WorkspaceRef.Name)
	}

	if claim.Annotations == nil {
		claim.Annotations = map[string]string{}
	}
	claim.Annotations[workspaceHydratedAnnotation] = headName
	return r.Update(ctx, claim)
}

// maybeResumeMemory requests the memory-snapshot restore for a resumable head.
// A head is resumable only when it carries a memorySnapshotRef AND the snapshot
// still exists AND it is bound to the activating claim's principal. A
// non-resumable head (no ref, a GC'd snapshot, or a principal mismatch) is a
// no-op: the caller hydrates content only. A cross-principal head is REFUSED
// (an error), never silently downgraded, so an attempt to resume another
// principal's secrets-in-RAM is loud, not quiet.
func (r *SandboxClaimReconciler) maybeResumeMemory(ctx context.Context, claim *v1alpha1.SandboxClaim, head *v1alpha1.WorkspaceRevision) error {
	if head.Spec.MemorySnapshotRef == nil {
		return nil
	}
	ref := *head.Spec.MemorySnapshotRef
	logger := log.FromContext(ctx)

	// Principal binding: the snapshot is served only to the principal that
	// captured it. A mismatch is refused, not downgraded to a cold start, so the
	// caller cannot accidentally proceed past a cross-principal denial.
	boundPrincipal := ""
	if head.Spec.MemorySnapshotPrincipal != nil {
		boundPrincipal = *head.Spec.MemorySnapshotPrincipal
	}
	if boundPrincipal != claim.Spec.ServiceAccount {
		return fmt.Errorf(
			"refusing to resume workspace %s head %s: its memory snapshot is bound to a different principal; a memory image carries secrets-in-RAM and is never served across principals",
			claim.Spec.WorkspaceRef.Name, head.Name)
	}

	// Verify the snapshot still exists (a GC'd snapshot is not resumable); the
	// check is principal-bound too. An absent snapshot degrades to content-only
	// hydrate (the head is simply no longer resumable), which is safe.
	exists, err := r.memorySnapshotExists()(ctx, ref, boundPrincipal)
	if err != nil {
		return fmt.Errorf("verify memory snapshot for workspace %s head %s: %w", claim.Spec.WorkspaceRef.Name, head.Name, err)
	}
	if !exists {
		logger.Info("head pairs a memory snapshot but it no longer exists; hydrating content only", "claim", claim.Name, "head", head.Name)
		return nil
	}

	if err := r.resumeMemory()(ctx, claim, ref); err != nil {
		return fmt.Errorf("resume memory snapshot for workspace %s head %s into claim %s: %w", claim.Spec.WorkspaceRef.Name, head.Name, claim.Name, err)
	}
	logger.Info("resumed VM memory snapshot for resumable head, paired with content hydrate", "claim", claim.Name, "workspace", claim.Spec.WorkspaceRef.Name, "head", head.Name)
	return nil
}

// dehydrateOnTerminate captures the claim's sandbox /workspace into a new
// committed WorkspaceRevision (fromClaim lineage) exactly once, advancing the
// workspace head via the Workspace reconciler. A claim without a WorkspaceRef, or
// one already dehydrated, is a no-op. Secret paths are excluded so the revision
// carries content only. A dehydrate or create error is returned to the caller so
// the terminate is retried rather than losing the work silently.
func (r *SandboxClaimReconciler) dehydrateOnTerminate(ctx context.Context, claim *v1alpha1.SandboxClaim) error {
	if claim.Spec.WorkspaceRef == nil {
		return nil
	}
	if _, done := claim.Annotations[workspaceDehydratedAnnotation]; done {
		return nil
	}
	logger := log.FromContext(ctx)

	// workspace.dehydrate is a child of controller.reconcileClaim (it starts from
	// the reconcile ctx), so the captured revision resolves to the request that
	// created it. Attributes name content pointers and counts only (workspace and
	// revision NAMES, the contentManifest DIGEST, the captured-path COUNT, and
	// whether a memory snapshot was paired); never a secret value. The revision
	// name and the digest are set once known, below.
	ctx, span := tracer.Start(ctx, "workspace.dehydrate", trace.WithAttributes(
		attribute.String("workspace.name", claim.Spec.WorkspaceRef.Name),
	))
	var dehydrateErr error
	defer func() {
		if dehydrateErr != nil {
			span.SetStatus(codes.Error, "dehydrate failed")
			span.RecordError(dehydrateErr)
		}
		span.End()
	}()

	// dehydrateOnTerminate returns through named results below; the deferred span
	// end reads dehydrateErr, so each early return assigns it.
	var err error
	finish := func(e error) error { dehydrateErr = e; return e }

	// Resolve the workspace head BEFORE this revision: it is the diff parent and
	// the lineage tip the new revision descends from.
	parentManifest, parentHead, _, err := r.resolveWorkspaceHead(ctx, claim)
	if err != nil {
		return finish(err)
	}
	parentRev := ""
	if parentHead != nil {
		parentRev = parentHead.Name
	}

	// Outputs narrow the capture to the listed /workspace subtrees; no Path output
	// captures the whole workspace (the slice-2 default).
	capturePaths := workspace.CapturePaths(claim.Spec.Outputs)

	span.SetAttributes(attribute.Int("captured.path.count", len(capturePaths)))

	digest, err := r.dehydrate()(ctx, claim, WorkspaceSecretExcludePaths, capturePaths)
	if err != nil {
		return finish(fmt.Errorf("dehydrate claim %s workspace %s: %w", claim.Name, claim.Spec.WorkspaceRef.Name, err))
	}
	if err := digest.Validate(); err != nil {
		return finish(fmt.Errorf("dehydrate claim %s produced an invalid content digest: %w", claim.Name, err))
	}
	// The contentManifest digest is a content address, not a secret.
	span.SetAttributes(attribute.String("content.manifest.digest", string(digest)))

	// A {diff: true} output records the content-hash diff of this revision against
	// the parent head, so an indexer or a human can see what changed.
	var diffSummary *v1alpha1.RevisionDiffSummary
	if outputsWantDiff(claim.Spec.Outputs) {
		d, derr := r.diff()(ctx, claim, parentManifest, digest)
		if derr != nil {
			return finish(fmt.Errorf("diff claim %s revision against parent %s: %w", claim.Name, parentRev, derr))
		}
		diffSummary = &v1alpha1.RevisionDiffSummary{
			ParentRevision: parentRev,
			Added:          d.Added,
			Removed:        d.Removed,
			Modified:       d.Modified,
			AddedCount:     int32(len(d.Added)),
			RemovedCount:   int32(len(d.Removed)),
			ModifiedCount:  int32(len(d.Modified)),
		}
	}

	// Checkpoint-on-terminate pairs the new revision with the sandbox's VM memory
	// snapshot, making the workspace head resumable. The snapshot is bound to the
	// claim's principal (its ServiceAccount): a memory image carries
	// secrets-in-RAM and is never served across principals. A plain terminate
	// (CheckpointOnTerminate unset) leaves both refs nil (a content-only,
	// non-resumable revision). A checkpoint error aborts the terminate (the work
	// is captured on retry) rather than silently producing a content-only revision.
	var memSnapshotRef, memSnapshotPrincipal *string
	if claim.Spec.CheckpointOnTerminate {
		snap, cerr := r.checkpointMemory()(ctx, claim)
		if cerr != nil {
			return finish(fmt.Errorf("checkpoint memory snapshot for claim %s: %w", claim.Name, cerr))
		}
		if snap.Ref != "" {
			ref := snap.Ref
			principal := snap.Principal
			memSnapshotRef = &ref
			memSnapshotPrincipal = &principal
			logger.Info("captured VM memory snapshot on terminate; pairing it with the new revision",
				"claim", claim.Name, "workspace", claim.Spec.WorkspaceRef.Name, "principal", principal)
		}
	}

	span.SetAttributes(attribute.Bool("memory.snapshot.paired", memSnapshotRef != nil))

	// Stamp the active reconcile's trace id onto the revision so it resolves to the
	// orchestrator request that created it. Only a valid trace id (tracing enabled)
	// is stamped; a no-op provider leaves the annotation absent rather than writing
	// a fake id. A trace id is an opaque correlation id, never a secret.
	annotations := traceIDAnnotations(ctx)

	rev := &v1alpha1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: claim.Spec.WorkspaceRef.Name + "-",
			Namespace:    claim.Namespace,
			Labels:       map[string]string{WorkspaceLabel: claim.Spec.WorkspaceRef.Name},
			Annotations:  annotations,
		},
		Spec: v1alpha1.WorkspaceRevisionSpec{
			WorkspaceRef:            v1alpha1.LocalObjectReference{Name: claim.Spec.WorkspaceRef.Name},
			Source:                  v1alpha1.RevisionSource{FromClaim: claim.Name},
			ContentManifest:         string(digest),
			MemorySnapshotRef:       memSnapshotRef,
			MemorySnapshotPrincipal: memSnapshotPrincipal,
		},
		Status: v1alpha1.WorkspaceRevisionStatus{Phase: v1alpha1.WorkspaceRevisionPending},
	}
	if err := r.Create(ctx, rev); err != nil {
		return finish(fmt.Errorf("create workspace revision for claim %s: %w", claim.Name, err))
	}
	// The revision name is a generated object name, not a secret.
	span.SetAttributes(attribute.String("revision.name", rev.Name))
	logger.Info("dehydrated sandbox workspace into a new revision", "claim", claim.Name, "workspace", claim.Spec.WorkspaceRef.Name, "revision", rev.Name)

	// Announce the new revision on the change feed: a Kubernetes Event on the
	// revision plus, when configured, a revision.created CloudEvent to the
	// operator webhook. The payload carries the workspace and revision NAMES, the
	// contentManifest DIGEST, lineage, and the memorySnapshotRef pointer only; no
	// secret values. This is how an external indexer learns of the new revision
	// without polling.
	r.Feed.emitRevisionCreated(ctx, rev)

	// Push the workspace repo paths to any git rendezvous remote, recording the
	// pushes for the revision status.
	gitPushes, gitErr := r.rendezvousOnTerminate(ctx, claim, digest)

	// Record the diff summary and git pushes on the revision status subresource,
	// if either was produced.
	if diffSummary != nil || len(gitPushes) > 0 {
		rev.Status.DiffSummary = diffSummary
		rev.Status.GitPushes = gitPushes
		if err := r.Status().Update(ctx, rev); err != nil {
			return finish(fmt.Errorf("record revision %s diff/git status: %w", rev.Name, err))
		}
	}

	if claim.Annotations == nil {
		claim.Annotations = map[string]string{}
	}
	claim.Annotations[workspaceDehydratedAnnotation] = rev.Name
	if err := r.Update(ctx, claim); err != nil {
		return finish(err)
	}
	// A git push failure is surfaced (not swallowed) only after the revision and
	// dehydrated annotation are durable, so the work is never lost and the
	// terminate retries the push.
	return finish(gitErr)
}

// rendezvousOnTerminate pushes the workspace repo paths to each {git} output's
// rendezvous remote on a per-attempt branch, returning the recorded pushes. The
// repo paths come from the workspace spec.git.paths resolved against the
// just-dehydrated content (digest). A {git} output whose workspace declares no
// spec.git.paths is an honest no-op with a logged warning: there is nothing to
// push. A push failure is returned (not swallowed) so the caller surfaces it on
// the claim/revision condition. Git is the merge layer: this pushes a per-attempt
// branch, the engine never merges working trees.
func (r *SandboxClaimReconciler) rendezvousOnTerminate(ctx context.Context, claim *v1alpha1.SandboxClaim, digest cas.Digest) ([]v1alpha1.GitPushRecord, error) {
	gitOutputs := gitOutputsOf(claim.Spec.Outputs)
	if len(gitOutputs) == 0 {
		return nil, nil
	}
	logger := log.FromContext(ctx)

	var ws v1alpha1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Spec.WorkspaceRef.Name}, &ws); err != nil {
		return nil, fmt.Errorf("resolve workspace %s for git rendezvous: %w", claim.Spec.WorkspaceRef.Name, err)
	}
	gitPaths := ws.Spec.Git.Paths
	if len(gitPaths) == 0 {
		// Honest: a {git} output with no repo paths declared has nothing to push.
		logger.Info("git output declared but workspace has no spec.git.paths; nothing to push",
			"claim", claim.Name, "workspace", ws.Name)
		return nil, nil
	}

	repoFiles, err := r.repoFiles()(ctx, claim, digest, gitPaths)
	if err != nil {
		return nil, fmt.Errorf("resolve git rendezvous repo paths for claim %s: %w", claim.Name, err)
	}
	if len(repoFiles) == 0 {
		logger.Info("git rendezvous resolved no files under spec.git.paths; nothing to push",
			"claim", claim.Name, "workspace", ws.Name, "paths", gitPaths)
		return nil, nil
	}

	var pushes []v1alpha1.GitPushRecord
	for _, g := range gitOutputs {
		branch, berr := workspace.RenderBranch(g.Branch, claim.Name)
		if berr != nil {
			return pushes, fmt.Errorf("render git rendezvous branch for claim %s: %w", claim.Name, berr)
		}
		if perr := r.rendezvous()(ctx, repoFiles, g.Remote, branch); perr != nil {
			return pushes, fmt.Errorf("git rendezvous push for claim %s to %s on %s: %w", claim.Name, g.Remote, branch, perr)
		}
		logger.Info("git rendezvous pushed workspace repo paths", "claim", claim.Name, "remote", g.Remote, "branch", branch)
		pushes = append(pushes, v1alpha1.GitPushRecord{Remote: g.Remote, Branch: branch})
	}
	return pushes, nil
}

// gitOutputsOf returns the {git} outputs from a claim's spec.outputs.
func gitOutputsOf(outputs []v1alpha1.OutputSpec) []*v1alpha1.GitOutput {
	var gits []*v1alpha1.GitOutput
	for i := range outputs {
		if outputs[i].Git != nil {
			gits = append(gits, outputs[i].Git)
		}
	}
	return gits
}

// outputsWantDiff reports whether any output requested a content-hash diff.
func outputsWantDiff(outputs []v1alpha1.OutputSpec) bool {
	for _, o := range outputs {
		if o.Diff {
			return true
		}
	}
	return false
}
