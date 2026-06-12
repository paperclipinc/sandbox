// Package eventfeed builds the workspace revision change feed: CloudEvents 1.0
// envelopes describing workspace and sandbox lifecycle events, delivered to an
// opt-in operator webhook sink and (always) mirrored as Kubernetes Events on
// the source object.
//
// The feed lets an external indexer learn of workspace activity (a new revision
// committed, a sandbox phase change) without polling the API. It is
// at-least-once: the webhook sink retries, and the event id is stable per
// (object, sequence) so a consumer can dedupe. No secret VALUES ever appear in
// a payload: only workspace and revision NAMES, the contentManifest DIGEST,
// lineage, and phases. See docs/threat-model.md for the egress note.
package eventfeed

import "time"

// CloudEvents constants for the feed. The source is the controller; the spec
// version is the CloudEvents 1.0 release.
const (
	// SpecVersion is the CloudEvents spec version the envelope declares.
	SpecVersion = "1.0"
	// Source is the CloudEvents source attribute: the controller emits the feed.
	Source = "agentrun.dev/controller"
	// DataContentType is the media type of the data payload.
	DataContentType = "application/json"

	// TypeRevisionCreated is emitted when a WorkspaceRevision is created.
	TypeRevisionCreated = "dev.agentrun.workspace.revision.created"
	// TypePhaseChanged is emitted when a SandboxClaim transitions phase.
	TypePhaseChanged = "dev.agentrun.sandbox.phase.changed"
)

// Event is a CloudEvents 1.0 envelope for the feed. The JSON tags match the
// CloudEvents 1.0 structured-content-mode binding so the marshaled body is a
// valid CloudEvent an operator webhook (or NATS, later) consumes directly.
//
// The Time is stamped by the CALLER from a clock (the object's transition time
// or an injected clock), never from a hidden time.Now inside this package, so a
// re-emit of the same logical event reproduces the same envelope.
type Event struct {
	SpecVersion     string    `json:"specversion"`
	Type            string    `json:"type"`
	Source          string    `json:"source"`
	Subject         string    `json:"subject"`
	ID              string    `json:"id"`
	Time            time.Time `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	Data            any       `json:"data"`
}

// RevisionCreatedData is the typed payload of a revision.created event. It
// carries content-addressed pointers and lineage only: workspace and revision
// NAMES, the contentManifest DIGEST, and the lineage origin. No secret values.
type RevisionCreatedData struct {
	Workspace string `json:"workspace"`
	Revision  string `json:"revision"`
	// ContentManifest is the content-addressed digest of the revision artifact
	// (a lowercase hex sha256). A digest, not content; never a secret.
	ContentManifest string `json:"contentManifest,omitempty"`
	// Lineage is the human-legible origin of the revision (the claim it was
	// dehydrated from, or the parent revision it forks from). Names only.
	Lineage string `json:"lineage,omitempty"`
	// MemorySnapshotRef, when set, names the memory snapshot the revision pairs
	// with (a CAS digest / snapshot id), making the head resumable. A pointer,
	// not the snapshot bytes; never a secret value.
	MemorySnapshotRef string `json:"memorySnapshotRef,omitempty"`
}

// PhaseChangedData is the typed payload of a sandbox.phase.changed event. It
// carries the claim name and the old and new phase strings only.
type PhaseChangedData struct {
	Claim    string `json:"claim"`
	OldPhase string `json:"oldPhase,omitempty"`
	NewPhase string `json:"newPhase"`
	// Workspace names the workspace the claim is bound to, when any. Empty for an
	// ephemeral claim. A name, not a secret.
	Workspace string `json:"workspace,omitempty"`
}

// NewRevisionCreated builds a revision.created CloudEvent. subject is the
// object ref (namespace/name) of the revision; id is the stable dedupe id (the
// object UID plus a sequence, built by EventID); at is the caller-stamped time.
func NewRevisionCreated(subject, id string, at time.Time, data RevisionCreatedData) Event {
	return newEvent(TypeRevisionCreated, subject, id, at, data)
}

// NewPhaseChanged builds a sandbox.phase.changed CloudEvent. subject is the
// claim's object ref; id is the stable dedupe id; at is the caller-stamped time.
func NewPhaseChanged(subject, id string, at time.Time, data PhaseChangedData) Event {
	return newEvent(TypePhaseChanged, subject, id, at, data)
}

func newEvent(eventType, subject, id string, at time.Time, data any) Event {
	return Event{
		SpecVersion:     SpecVersion,
		Type:            eventType,
		Source:          Source,
		Subject:         subject,
		ID:              id,
		Time:            at.UTC(),
		DataContentType: DataContentType,
		Data:            data,
	}
}

// EventID builds the stable, idempotent event id from the source object's UID
// and a per-object sequence string. Keying on the UID (not the name) means a
// recreated object with the same name gets a distinct id; the sequence
// distinguishes successive events on the same object. The same (uid, seq) always
// yields the same id, so a consumer can dedupe an at-least-once redelivery.
func EventID(uid, seq string) string {
	return uid + "/" + seq
}
