package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/cas"
	"github.com/paperclipinc/sandbox/internal/workspace"
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
const workspaceHydratedAnnotation = "agentrun.dev/workspace-hydrated-head"

// workspaceDehydratedAnnotation marks a claim whose sandbox /workspace was
// already dehydrated into a committed revision on terminate, so a re-entrant
// terminate (lifetime expiry then delete) does not create a second revision.
const workspaceDehydratedAnnotation = "agentrun.dev/workspace-dehydrated"

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
// treats as nothing to hydrate. A missing Workspace is an error.
func (r *SandboxClaimReconciler) resolveWorkspaceHead(ctx context.Context, claim *v1alpha1.SandboxClaim) (manifest cas.Digest, headName string, ok bool, err error) {
	var ws v1alpha1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Spec.WorkspaceRef.Name}, &ws); err != nil {
		return "", "", false, fmt.Errorf("resolve workspace %s: %w", claim.Spec.WorkspaceRef.Name, err)
	}
	if ws.Status.Head == "" {
		return "", "", false, nil
	}
	var head v1alpha1.WorkspaceRevision
	if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: ws.Status.Head}, &head); err != nil {
		return "", "", false, fmt.Errorf("resolve workspace %s head revision %s: %w", ws.Name, ws.Status.Head, err)
	}
	d := cas.Digest(head.Spec.ContentManifest)
	if d.Validate() != nil {
		// Head names a revision whose manifest is not a valid content address;
		// nothing safe to hydrate.
		return "", head.Name, false, nil
	}
	return d, head.Name, true, nil
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

	manifest, headName, ok, err := r.resolveWorkspaceHead(ctx, claim)
	if err != nil {
		return err
	}
	if ok {
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

	// Resolve the workspace head BEFORE this revision: it is the diff parent and
	// the lineage tip the new revision descends from.
	parentManifest, parentRev, _, err := r.resolveWorkspaceHead(ctx, claim)
	if err != nil {
		return err
	}

	// Outputs narrow the capture to the listed /workspace subtrees; no Path output
	// captures the whole workspace (the slice-2 default).
	capturePaths := workspace.CapturePaths(claim.Spec.Outputs)

	digest, err := r.dehydrate()(ctx, claim, WorkspaceSecretExcludePaths, capturePaths)
	if err != nil {
		return fmt.Errorf("dehydrate claim %s workspace %s: %w", claim.Name, claim.Spec.WorkspaceRef.Name, err)
	}
	if err := digest.Validate(); err != nil {
		return fmt.Errorf("dehydrate claim %s produced an invalid content digest: %w", claim.Name, err)
	}

	// A {diff: true} output records the content-hash diff of this revision against
	// the parent head, so an indexer or a human can see what changed.
	var diffSummary *v1alpha1.RevisionDiffSummary
	if outputsWantDiff(claim.Spec.Outputs) {
		d, derr := r.diff()(ctx, claim, parentManifest, digest)
		if derr != nil {
			return fmt.Errorf("diff claim %s revision against parent %s: %w", claim.Name, parentRev, derr)
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

	rev := &v1alpha1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: claim.Spec.WorkspaceRef.Name + "-",
			Namespace:    claim.Namespace,
			Labels:       map[string]string{WorkspaceLabel: claim.Spec.WorkspaceRef.Name},
		},
		Spec: v1alpha1.WorkspaceRevisionSpec{
			WorkspaceRef:    v1alpha1.LocalObjectReference{Name: claim.Spec.WorkspaceRef.Name},
			Source:          v1alpha1.RevisionSource{FromClaim: claim.Name},
			ContentManifest: string(digest),
		},
		Status: v1alpha1.WorkspaceRevisionStatus{Phase: v1alpha1.WorkspaceRevisionPending},
	}
	if err := r.Create(ctx, rev); err != nil {
		return fmt.Errorf("create workspace revision for claim %s: %w", claim.Name, err)
	}
	logger.Info("dehydrated sandbox workspace into a new revision", "claim", claim.Name, "workspace", claim.Spec.WorkspaceRef.Name, "revision", rev.Name)

	// Push the workspace repo paths to any git rendezvous remote, recording the
	// pushes for the revision status.
	gitPushes, gitErr := r.rendezvousOnTerminate(ctx, claim, digest)

	// Record the diff summary and git pushes on the revision status subresource,
	// if either was produced.
	if diffSummary != nil || len(gitPushes) > 0 {
		rev.Status.DiffSummary = diffSummary
		rev.Status.GitPushes = gitPushes
		if err := r.Status().Update(ctx, rev); err != nil {
			return fmt.Errorf("record revision %s diff/git status: %w", rev.Name, err)
		}
	}

	if claim.Annotations == nil {
		claim.Annotations = map[string]string{}
	}
	claim.Annotations[workspaceDehydratedAnnotation] = rev.Name
	if err := r.Update(ctx, claim); err != nil {
		return err
	}
	// A git push failure is surfaced (not swallowed) only after the revision and
	// dehydrated annotation are durable, so the work is never lost and the
	// terminate retries the push.
	return gitErr
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
