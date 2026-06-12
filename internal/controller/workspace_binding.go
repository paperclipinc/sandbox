package controller

import (
	"context"
	"fmt"

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
// list; envtest injects a fake that returns a scripted digest and records the
// excludes. The returned digest is the new revision's ContentManifest.
type dehydrateFunc func(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths []string) (cas.Digest, error)

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
func (r *SandboxClaimReconciler) defaultDehydrate(ctx context.Context, claim *v1alpha1.SandboxClaim, excludePaths []string) (cas.Digest, error) {
	agent, store, err := r.workspaceTransport(claim)
	if err != nil {
		return "", err
	}
	return workspace.Dehydrate(ctx, agent, store, excludePaths)
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

	digest, err := r.dehydrate()(ctx, claim, WorkspaceSecretExcludePaths)
	if err != nil {
		return fmt.Errorf("dehydrate claim %s workspace %s: %w", claim.Name, claim.Spec.WorkspaceRef.Name, err)
	}
	if err := digest.Validate(); err != nil {
		return fmt.Errorf("dehydrate claim %s produced an invalid content digest: %w", claim.Name, err)
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

	if claim.Annotations == nil {
		claim.Annotations = map[string]string{}
	}
	claim.Annotations[workspaceDehydratedAnnotation] = rev.Name
	return r.Update(ctx, claim)
}
