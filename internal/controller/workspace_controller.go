package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// WorkspaceLabel is stamped on a WorkspaceRevision to record the workspace it
// belongs to, so the reconciler can list a workspace's revisions by label as
// well as by owner reference. The reconciler adopts a revision that names a
// workspace via spec.workspaceRef by setting both the label and the owner
// reference (for GC) the first time it reconciles the workspace.
const WorkspaceLabel = "mitos.run/workspace"

// Workspace reason codes. These are the normative reason strings for the
// Workspace and WorkspaceRevision Ready conditions; the catalogue lives in
// docs/conditions.md.
const (
	// ReasonWorkspaceReady marks a workspace whose model is valid: every
	// revision's lineage resolves and the head/revisions/resumable status is
	// computed.
	ReasonWorkspaceReady = "WorkspaceReady"
	// ReasonWorkspacePending marks a workspace with no committed revision yet.
	ReasonWorkspacePending = "WorkspacePending"
	// ReasonWorkspaceDegraded marks a workspace with a broken lineage edge (a
	// FromWorkspaceRevision that does not resolve to a revision in the same
	// workspace).
	ReasonWorkspaceDegraded = "WorkspaceDegraded"

	// ReasonRevisionCommitted marks a revision whose ContentManifest is a valid
	// content-addressed digest.
	ReasonRevisionCommitted = "RevisionCommitted"
	// ReasonRevisionPending marks a revision still awaiting a valid
	// ContentManifest (dehydrate has not produced one).
	ReasonRevisionPending = "RevisionPending"
)

// WorkspaceReconciler manages the declarative Workspace model. It owns the
// revision DAG of a Workspace: it adopts each revision (workspace label + owner
// reference for GC), drives every revision's Pending -> Committed transition
// with immutability of a committed ContentManifest, validates the
// FromWorkspaceRevision lineage edges, computes the workspace status (head, the
// committed revision count, resumable), and enforces retention pruning over the
// DAG without ever pruning the head or an ancestor on the head's lineage path.
//
// No data moves in this slice: hydrate/dehydrate is a later W4 slice. A test
// seam (and, later, dehydrate) supplies a revision's ContentManifest.
type WorkspaceReconciler struct {
	client.Client

	// SnapshotExists verifies a head revision's paired memory snapshot still
	// exists and is principal-bound, for the resumable status (W4 Task 2): a head
	// is resumable only when it carries a memorySnapshotRef AND the referenced
	// snapshot exists (a GC'd snapshot flips resumable false). Nil defaults to a
	// fail-closed check that treats every snapshot as absent, so an unwired
	// controller never marks a head resumable it cannot verify.
	SnapshotExists memorySnapshotExistsFunc
}

// snapshotExists returns the configured existence-check seam or a fail-closed
// default.
func (r *WorkspaceReconciler) snapshotExists() memorySnapshotExistsFunc {
	if r.SnapshotExists != nil {
		return r.SnapshotExists
	}
	return func(context.Context, string, string) (bool, error) {
		return false, nil
	}
}

// +kubebuilder:rbac:groups=mitos.run,resources=workspaces;workspacerevisions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=workspaces/status;workspacerevisions/status,verbs=get;patch;update
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ws v1alpha1.Workspace
	if err := r.Get(ctx, req.NamespacedName, &ws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	revs, err := r.workspaceRevisions(ctx, &ws)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Drive each revision: adopt it (label + owner ref), validate its lineage,
	// and transition its phase. This reconciles the whole DAG of the workspace in
	// one pass, so a revision created by a test seam (no owner ref yet) is
	// committed here without needing its own watch.
	lineageOK := true
	for i := range revs {
		if err := r.adoptRevision(ctx, &revs[i], &ws); err != nil {
			return ctrl.Result{}, err
		}
		ok, err := r.reconcileRevision(ctx, &revs[i])
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			lineageOK = false
		}
	}

	// Re-list after the phase writes so head/count read the committed phases just
	// set (the cached client may lag a status patch within the same reconcile).
	revs, err = r.workspaceRevisions(ctx, &ws)
	if err != nil {
		return ctrl.Result{}, err
	}

	committed := committedRevisions(revs)
	head := headRevision(committed)

	status := metav1.ConditionTrue
	reason := ReasonWorkspaceReady
	msg := fmt.Sprintf("%d committed revision(s); head %q", len(committed), headName(head))
	switch {
	case !lineageOK:
		status = metav1.ConditionFalse
		reason = ReasonWorkspaceDegraded
		msg = "a revision has a broken fromWorkspaceRevision lineage edge"
	case head == nil:
		status = metav1.ConditionFalse
		reason = ReasonWorkspacePending
		msg = "no committed revision yet"
	}

	// A head is resumable only when it pairs a memory snapshot AND that snapshot
	// still exists (verified, principal-bound, via the store seam). A GC'd
	// snapshot flips resumable false, so the status never advertises a resume that
	// would fail. A verification error leaves resumable false (fail closed) and is
	// logged; the next reconcile retries.
	resumable := false
	if head != nil && head.Spec.MemorySnapshotRef != nil {
		principal := ""
		if head.Spec.MemorySnapshotPrincipal != nil {
			principal = *head.Spec.MemorySnapshotPrincipal
		}
		exists, existsErr := r.snapshotExists()(ctx, *head.Spec.MemorySnapshotRef, principal)
		if existsErr != nil {
			logger.Error(existsErr, "verify head memory snapshot existence; treating head as non-resumable this pass", "workspace", ws.Name, "head", head.Name)
		}
		resumable = existsErr == nil && exists
	}

	patch := client.MergeFrom(ws.DeepCopy())
	ws.Status.Head = headName(head)
	ws.Status.Revisions = int32(len(committed))
	ws.Status.Resumable = resumable
	ws.Status.ObservedGeneration = ws.Generation
	setCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: ws.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            msg,
	})
	if err := r.Status().Patch(ctx, &ws, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch workspace %s status: %w", ws.Name, err)
	}

	if err := r.enforceRetention(ctx, &ws, committed, head, logger); err != nil {
		// Retention is best-effort: a prune error is logged and retried on the
		// next reconcile; it never blocks the status update above.
		logger.Error(err, "retention pruning failed", "workspace", ws.Name)
	}

	return ctrl.Result{}, nil
}

// reconcileRevision drives a single WorkspaceRevision's phase: it validates the
// FromWorkspaceRevision lineage, transitions Pending -> Committed when the
// ContentManifest is a valid content-addressed digest, and enforces
// immutability of a committed manifest. It returns whether the revision's
// lineage is valid (a broken edge degrades the workspace).
func (r *WorkspaceReconciler) reconcileRevision(ctx context.Context, rev *v1alpha1.WorkspaceRevision) (bool, error) {
	logger := log.FromContext(ctx)

	lineageOK, lineageMsg := r.validateLineage(ctx, rev)

	// A revision commits when its ContentManifest is a valid content-addressed
	// digest (a lowercase hex sha256). dehydrate produces it in a later slice; a
	// test seam supplies one here.
	manifestValid := rev.Spec.ContentManifest != "" &&
		cas.Digest(rev.Spec.ContentManifest).Validate() == nil

	// Immutability: a Committed revision is frozen (single-writer-per-revision).
	// It never transitions back to Pending and never recomputes its phase from a
	// changed manifest; a manifest cleared or changed on an already-Committed
	// revision is rejected here (logged, status left Committed). Validating
	// admission of the change itself is a webhook in a later slice; this is the
	// reconciler-side backstop.
	desiredPhase := v1alpha1.WorkspaceRevisionPending
	switch {
	case rev.Status.Phase == v1alpha1.WorkspaceRevisionCommitted:
		if !manifestValid {
			logger.Info("rejecting mutation of a committed revision's contentManifest; a committed revision is immutable",
				"revision", rev.Name, "workspace", rev.Spec.WorkspaceRef.Name)
		}
		desiredPhase = v1alpha1.WorkspaceRevisionCommitted
	case manifestValid && lineageOK:
		desiredPhase = v1alpha1.WorkspaceRevisionCommitted
	}

	committed := desiredPhase == v1alpha1.WorkspaceRevisionCommitted
	reason := ReasonRevisionCommitted
	msg := "contentManifest is a valid content-addressed digest"
	if !committed {
		reason = ReasonRevisionPending
		switch {
		case !lineageOK:
			msg = lineageMsg
		case rev.Spec.ContentManifest == "":
			msg = "awaiting contentManifest from dehydrate"
		default:
			msg = "contentManifest is not a valid content-addressed digest"
		}
	}

	if r.revisionStatusChanged(rev, desiredPhase, reason, msg, committed) {
		patch := client.MergeFrom(rev.DeepCopy())
		rev.Status.Phase = desiredPhase
		rev.Status.ObservedGeneration = rev.Generation
		setCondition(&rev.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             conditionStatus(committed),
			ObservedGeneration: rev.Generation,
			LastTransitionTime: metav1.Now(),
			Reason:             reason,
			Message:            msg,
		})
		if err := r.Status().Patch(ctx, rev, patch); err != nil {
			return lineageOK, fmt.Errorf("patch revision %s status: %w", rev.Name, err)
		}
	}
	return lineageOK, nil
}

// revisionStatusChanged reports whether the revision's status needs a write.
func (r *WorkspaceReconciler) revisionStatusChanged(rev *v1alpha1.WorkspaceRevision, phase v1alpha1.WorkspaceRevisionPhase, reason, msg string, committed bool) bool {
	if rev.Status.Phase != phase || rev.Status.ObservedGeneration != rev.Generation {
		return true
	}
	existing := findCondition(rev.Status.Conditions, "Ready")
	if existing == nil {
		return true
	}
	return existing.Reason != reason || existing.Message != msg || existing.Status != conditionStatus(committed)
}

// adoptRevision stamps the workspace label and owner reference on a revision the
// first time the reconciler sees it. The owner reference is what makes deleting
// the workspace garbage-collect the revision. A no-op once both are present.
func (r *WorkspaceReconciler) adoptRevision(ctx context.Context, rev *v1alpha1.WorkspaceRevision, ws *v1alpha1.Workspace) error {
	hasLabel := rev.Labels[WorkspaceLabel] == ws.Name
	hasOwner := false
	for _, o := range rev.OwnerReferences {
		if o.Kind == "Workspace" && o.Name == ws.Name {
			hasOwner = true
			break
		}
	}
	if hasLabel && hasOwner {
		return nil
	}

	patch := client.MergeFrom(rev.DeepCopy())
	if rev.Labels == nil {
		rev.Labels = map[string]string{}
	}
	rev.Labels[WorkspaceLabel] = ws.Name
	if !hasOwner {
		if err := ctrl.SetControllerReference(ws, rev, r.Scheme()); err != nil {
			return fmt.Errorf("set owner reference on revision %s: %w", rev.Name, err)
		}
	}
	if err := r.Patch(ctx, rev, patch); err != nil {
		return fmt.Errorf("adopt revision %s into workspace %s: %w", rev.Name, ws.Name, err)
	}
	return nil
}

// validateLineage reports whether a revision's FromWorkspaceRevision edge
// resolves to an existing revision in the SAME workspace. A revision with no
// FromWorkspaceRevision edge (a root, or one sourced FromClaim) is valid.
func (r *WorkspaceReconciler) validateLineage(ctx context.Context, rev *v1alpha1.WorkspaceRevision) (bool, string) {
	src := rev.Spec.Source.FromWorkspaceRevision
	if src == nil {
		return true, ""
	}
	if src.Workspace != rev.Spec.WorkspaceRef.Name {
		return false, fmt.Sprintf("fromWorkspaceRevision references workspace %q but this revision belongs to %q; cross-workspace lineage is not allowed", src.Workspace, rev.Spec.WorkspaceRef.Name)
	}
	var parent v1alpha1.WorkspaceRevision
	if err := r.Get(ctx, types.NamespacedName{Namespace: rev.Namespace, Name: src.Revision}, &parent); err != nil {
		return false, fmt.Sprintf("fromWorkspaceRevision references revision %q which does not exist in workspace %q", src.Revision, src.Workspace)
	}
	if parent.Spec.WorkspaceRef.Name != rev.Spec.WorkspaceRef.Name {
		return false, fmt.Sprintf("fromWorkspaceRevision parent %q belongs to workspace %q, not %q", src.Revision, parent.Spec.WorkspaceRef.Name, rev.Spec.WorkspaceRef.Name)
	}
	return true, ""
}

// workspaceRevisions lists the revisions that belong to a workspace, matching on
// spec.workspaceRef (which a revision carries from creation, before the
// reconciler adopts it with the label).
func (r *WorkspaceReconciler) workspaceRevisions(ctx context.Context, ws *v1alpha1.Workspace) ([]v1alpha1.WorkspaceRevision, error) {
	var all v1alpha1.WorkspaceRevisionList
	if err := r.List(ctx, &all, client.InNamespace(ws.Namespace)); err != nil {
		return nil, fmt.Errorf("list workspace %s revisions: %w", ws.Name, err)
	}
	out := make([]v1alpha1.WorkspaceRevision, 0, len(all.Items))
	for i := range all.Items {
		if all.Items[i].Spec.WorkspaceRef.Name == ws.Name {
			out = append(out, all.Items[i])
		}
	}
	return out, nil
}

// enforceRetention prunes committed revisions beyond spec.retention.Revisions
// that are also older than spec.retention.MinAge, oldest first. It NEVER prunes
// the head or any ancestor on the head's lineage path (walked via the
// FromWorkspaceRevision chain), so a kept revision can always be reconstructed
// from its ancestors. A zero/unset Revisions disables count-based pruning.
func (r *WorkspaceReconciler) enforceRetention(ctx context.Context, ws *v1alpha1.Workspace, committed []v1alpha1.WorkspaceRevision, head *v1alpha1.WorkspaceRevision, logger logger) error {
	keep := ws.Spec.Retention.Revisions
	if keep <= 0 || int32(len(committed)) <= keep {
		return nil
	}

	protected := headLineage(committed, head)

	// Order oldest first so we prune the oldest over-count revisions.
	sorted := append([]v1alpha1.WorkspaceRevision(nil), committed...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return revisionLess(&sorted[i], &sorted[j])
	})

	// Prune everything beyond the keep count, drawn from the oldest end, skipping
	// protected (head + ancestors) and any revision younger than minAge.
	excess := int(int32(len(committed)) - keep)
	now := time.Now()
	pruned := 0
	for i := 0; i < len(sorted) && pruned < excess; i++ {
		rev := &sorted[i]
		if protected[rev.Name] {
			continue
		}
		if ws.Spec.Retention.MinAge != nil {
			if now.Sub(rev.CreationTimestamp.Time) < ws.Spec.Retention.MinAge.Duration {
				continue
			}
		}
		if err := r.Delete(ctx, rev); err != nil {
			return fmt.Errorf("prune revision %s: %w", rev.Name, err)
		}
		logger.Info("pruned revision past retention policy", "workspace", ws.Name, "revision", rev.Name, "keep", keep)
		pruned++
	}
	return nil
}

// headLineage returns the set of revision names on the head's ancestry path (the
// head itself plus every ancestor reachable through FromWorkspaceRevision).
// These are never pruned.
func headLineage(committed []v1alpha1.WorkspaceRevision, head *v1alpha1.WorkspaceRevision) map[string]bool {
	protected := map[string]bool{}
	if head == nil {
		return protected
	}
	byName := map[string]*v1alpha1.WorkspaceRevision{}
	for i := range committed {
		byName[committed[i].Name] = &committed[i]
	}
	cur := head
	for cur != nil {
		if protected[cur.Name] {
			break // cycle guard
		}
		protected[cur.Name] = true
		src := cur.Spec.Source.FromWorkspaceRevision
		if src == nil {
			break
		}
		cur = byName[src.Revision]
	}
	return protected
}

// committedRevisions filters a revision set to those in the Committed phase.
func committedRevisions(revs []v1alpha1.WorkspaceRevision) []v1alpha1.WorkspaceRevision {
	out := make([]v1alpha1.WorkspaceRevision, 0, len(revs))
	for i := range revs {
		if revs[i].Status.Phase == v1alpha1.WorkspaceRevisionCommitted {
			out = append(out, revs[i])
		}
	}
	return out
}

// headRevision returns the latest committed revision, ordered by creation
// timestamp then name (the deterministic tiebreaker). Returns nil for an empty
// set.
func headRevision(committed []v1alpha1.WorkspaceRevision) *v1alpha1.WorkspaceRevision {
	if len(committed) == 0 {
		return nil
	}
	head := &committed[0]
	for i := 1; i < len(committed); i++ {
		if revisionLess(head, &committed[i]) {
			head = &committed[i]
		}
	}
	return head
}

// revisionLess reports whether a is older than b: earlier creation timestamp,
// then lexicographically smaller name as a stable tiebreaker for revisions
// created in the same instant.
func revisionLess(a, b *v1alpha1.WorkspaceRevision) bool {
	at, bt := a.CreationTimestamp.Time, b.CreationTimestamp.Time
	if at.Equal(bt) {
		return a.Name < b.Name
	}
	return at.Before(bt)
}

func headName(head *v1alpha1.WorkspaceRevision) string {
	if head == nil {
		return ""
	}
	return head.Name
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// logger is the minimal logging surface the reconciler uses, satisfied by the
// controller-runtime logr.Logger.
type logger interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}

func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Owns(WorkspaceRevision): an adopted revision is owner-referenced to its
	// workspace, so a revision update/delete enqueues the owning workspace and
	// its head/count/resumable recompute. Watches(WorkspaceRevision) on top maps
	// EVERY revision (including one not yet adopted, freshly created by a test
	// seam or dehydrate) to its workspace via spec.workspaceRef, so the first
	// reconcile that adopts and commits it is event-driven, not poll-driven.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Workspace{}).
		Owns(&v1alpha1.WorkspaceRevision{}).
		Watches(&v1alpha1.WorkspaceRevision{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			rev, ok := obj.(*v1alpha1.WorkspaceRevision)
			if !ok || rev.Spec.WorkspaceRef.Name == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{
				Namespace: rev.Namespace,
				Name:      rev.Spec.WorkspaceRef.Name,
			}}}
		})).
		Complete(r)
}
