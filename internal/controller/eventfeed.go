package controller

import (
	"context"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/eventfeed"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// feedClock is the clock the feed stamps event times from. It is a seam (not a
// hidden time.Now) so a test can pin the event time and the stamped time is
// always explicit at the emit site. Nil falls back to time.Now.
type feedClock func() time.Time

// emitFeed bundles the always-on Kubernetes Event channel (the EventRecorder)
// with the opt-in CloudEvents sink. Both reconcilers hold one. The CloudEvents
// sink is NopSink unless --event-sink-url is set; the Event recorder is always
// present (a no-op FakeRecorder in tests that do not assert on it).
//
// Emit failures never block reconcile: the feed is an at-least-once side
// channel, so a sink error is logged and swallowed (the next reconcile re-emits
// the same logical event with the same dedupe id). A Kubernetes Event that fails
// to record is likewise non-fatal; record.EventRecorder already swallows its own
// delivery errors.
type emitFeed struct {
	recorder record.EventRecorder
	sink     eventfeed.Sink
	clock    feedClock
}

// NewEmitFeed builds the change feed from the manager's EventRecorder and the
// CloudEvents sink. clock is the event-time seam (nil uses time.Now); cmd/
// controller passes nil for the wall clock, tests pin it.
func NewEmitFeed(recorder record.EventRecorder, sink eventfeed.Sink, clock func() time.Time) emitFeed {
	return emitFeed{recorder: recorder, sink: sink, clock: clock}
}

func (f emitFeed) now() time.Time {
	if f.clock != nil {
		return f.clock()
	}
	return time.Now()
}

// recorderOrNop returns the configured recorder or a no-op so an unwired
// reconciler (a bare struct in a unit test) never nil-panics.
func (f emitFeed) recorderOrNop() record.EventRecorder {
	if f.recorder != nil {
		return f.recorder
	}
	return nopRecorder{}
}

// sinkOrNop returns the configured CloudEvents sink or a NopSink.
func (f emitFeed) sinkOrNop() eventfeed.Sink {
	if f.sink != nil {
		return f.sink
	}
	return eventfeed.NopSink{}
}

// emitRevisionCreated records a Kubernetes Event on the revision and emits a
// revision.created CloudEvent to the sink. The CloudEvent time is stamped from
// the feed clock (or the revision creationTimestamp when set), never a hidden
// Now inside the sink. No secret values: the payload carries the workspace and
// revision NAMES, the contentManifest DIGEST, lineage, and the memorySnapshotRef
// pointer only.
func (f emitFeed) emitRevisionCreated(ctx context.Context, rev *v1alpha1.WorkspaceRevision) {
	logger := log.FromContext(ctx)

	memRef := ""
	if rev.Spec.MemorySnapshotRef != nil {
		memRef = *rev.Spec.MemorySnapshotRef
	}
	data := eventfeed.RevisionCreatedData{
		Workspace:         rev.Spec.WorkspaceRef.Name,
		Revision:          rev.Name,
		ContentManifest:   rev.Spec.ContentManifest,
		Lineage:           revisionLineage(rev),
		MemorySnapshotRef: memRef,
		// Carry the revision's stamped trace id (empty when absent) so an indexer
		// correlates this event with the orchestrator trace. A correlation id, not
		// a secret.
		TraceID: rev.Annotations[traceIDAnnotation],
	}

	// The always-on channel: a Kubernetes Event on the revision object. The
	// message names content pointers only.
	f.recorderOrNop().Eventf(rev, "Normal", "RevisionCreated",
		"workspace %q committed revision %q (%s)", data.Workspace, data.Revision, data.Lineage)

	at := f.now()
	if !rev.CreationTimestamp.IsZero() {
		at = rev.CreationTimestamp.Time
	}
	id := eventfeed.EventID(string(rev.UID), "created")
	e := eventfeed.NewRevisionCreated(objectRef(rev), id, at, data)
	if err := f.sinkOrNop().Emit(ctx, e); err != nil {
		// At-least-once: log and move on. The dedupe id makes a later re-emit safe.
		logger.Error(err, "emit revision.created to event sink (will retry on next reconcile)", "revision", rev.Name)
	}
}

// emitPhaseChanged records a Kubernetes Event on the claim and emits a
// sandbox.phase.changed CloudEvent. The payload carries the claim and workspace
// NAMES and the old and new phase strings only. seq distinguishes successive
// transitions on the same claim (the new phase), so the dedupe id is stable per
// transition.
func (f emitFeed) emitPhaseChanged(ctx context.Context, claim *v1alpha1.SandboxClaim, oldPhase, newPhase v1alpha1.SandboxPhase) {
	logger := log.FromContext(ctx)

	ws := ""
	if claim.Spec.WorkspaceRef != nil {
		ws = claim.Spec.WorkspaceRef.Name
	}
	data := eventfeed.PhaseChangedData{
		Claim:     claim.Name,
		OldPhase:  string(oldPhase),
		NewPhase:  string(newPhase),
		Workspace: ws,
	}

	f.recorderOrNop().Eventf(claim, "Normal", "PhaseChanged",
		"sandbox claim %q phase %s -> %s", claim.Name, oldPhase, newPhase)

	at := f.now()
	id := eventfeed.EventID(string(claim.UID), string(newPhase))
	e := eventfeed.NewPhaseChanged(objectRef(claim), id, at, data)
	if err := f.sinkOrNop().Emit(ctx, e); err != nil {
		logger.Error(err, "emit sandbox.phase.changed to event sink (will retry on next reconcile)", "claim", claim.Name)
	}
}

// revisionLineage renders the human-legible lineage origin of a revision: the
// claim it was dehydrated from, or the parent revision it forks from. Names
// only; never a secret.
func revisionLineage(rev *v1alpha1.WorkspaceRevision) string {
	if rev.Spec.Source.FromClaim != "" {
		return "fromClaim:" + rev.Spec.Source.FromClaim
	}
	if rev.Spec.Source.FromWorkspaceRevision != nil {
		return "fromWorkspaceRevision:" + rev.Spec.Source.FromWorkspaceRevision.Revision
	}
	return "root"
}

// objectRef renders the CloudEvent subject for an object: namespace/name.
func objectRef(obj client.Object) string {
	if obj.GetNamespace() == "" {
		return obj.GetName()
	}
	return obj.GetNamespace() + "/" + obj.GetName()
}

// nopRecorder is an EventRecorder that drops every event, used when a reconciler
// is constructed without a recorder (a bare struct in a unit test). The
// production reconcilers always get mgr.GetEventRecorderFor.
type nopRecorder struct{}

func (nopRecorder) Event(runtime.Object, string, string, string)                  {}
func (nopRecorder) Eventf(runtime.Object, string, string, string, ...interface{}) {}
func (nopRecorder) AnnotatedEventf(runtime.Object, map[string]string, string, string, string, ...interface{}) {
}
