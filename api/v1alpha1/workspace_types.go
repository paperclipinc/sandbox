package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Workspace ---
//
// A Workspace is durable, versioned, forkable agent state that lives
// independent of any single sandbox. It is NOT a CSI PersistentVolume: a
// Workspace is a versioned content-addressed artifact whose revisions form a
// DAG, optionally paired with a memory snapshot, hydrated and dehydrated over
// the same content-addressed transfer layer as snapshot distribution. The
// rationale is recorded in docs/adr/0002-workspace-not-csi.md. This slice is
// the declarative model only: no data moves yet. hydrate/dehydrate, git
// rendezvous, outputs extraction, and the revision change feed are later W4
// slices.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Head",type=string,JSONPath=`.status.head`
// +kubebuilder:printcolumn:name="Revisions",type=integer,JSONPath=`.status.revisions`
// +kubebuilder:printcolumn:name="Resumable",type=boolean,JSONPath=`.status.resumable`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkspaceSpec   `json:"spec"`
	Status            WorkspaceStatus `json:"status,omitempty"`
}

type WorkspaceSpec struct {
	// Store selects the content-addressed object store that backs the
	// workspace's revision artifacts.
	Store WorkspaceStore `json:"store,omitempty"`

	// Git declares the repo paths that get history and the fork-and-merge
	// rendezvous remote. Git is the merge layer; the engine never does
	// filesystem merge. The rendezvous itself is a later W4 slice.
	Git WorkspaceGit `json:"git,omitempty"`

	// Retention bounds how many committed revisions are kept and how old a
	// revision must be before it is eligible for pruning.
	Retention WorkspaceRetention `json:"retention,omitempty"`

	// Grants are the explicit, auditable workspace access grants. Workspace
	// access is declarative Kubernetes RBAC over these grant objects, not a
	// pod-scoped mechanism.
	Grants []WorkspaceGrant `json:"grants,omitempty"`
}

type WorkspaceStore struct {
	// ObjectStorageRef names the S3-compatible, content-addressed object store
	// that holds the workspace's chunked revision artifacts. This slice resolves
	// it against the existing content-addressed store; the S3 backend is a later
	// W4 slice.
	ObjectStorageRef string `json:"objectStorageRef,omitempty"`
}

type WorkspaceGit struct {
	// Paths are the repo paths inside the workspace that get version history and
	// the rendezvous remote.
	Paths []string `json:"paths,omitempty"`
}

type WorkspaceRetention struct {
	// Revisions is the maximum number of committed revisions to keep. Committed
	// revisions beyond this count that are also older than MinAge are pruned
	// oldest-first. The head and any ancestor on the head's lineage path are
	// never pruned. Zero or unset disables count-based pruning.
	// +optional
	Revisions int32 `json:"revisions,omitempty"`

	// MinAge is the minimum age a revision must reach before it is eligible for
	// pruning. A revision younger than MinAge is never pruned even if the count
	// is over Revisions. Unset treats every over-count revision as eligible.
	// +optional
	MinAge *metav1.Duration `json:"minAge,omitempty"`
}

type WorkspaceAccess string

const (
	WorkspaceAccessReadOnly  WorkspaceAccess = "readonly"
	WorkspaceAccessReadWrite WorkspaceAccess = "readwrite"
)

type WorkspaceGrant struct {
	// ServiceAccount is the principal the grant applies to.
	ServiceAccount string `json:"serviceAccount"`

	// Access is the level of access the grant confers.
	// +kubebuilder:validation:Enum=readonly;readwrite
	Access WorkspaceAccess `json:"access"`
}

type WorkspaceStatus struct {
	// Head is the name of the latest committed revision in the workspace's
	// lineage. Empty until the first revision commits. Ordering is by revision
	// creationTimestamp, then by name as a deterministic tiebreaker.
	Head string `json:"head,omitempty"`

	// Revisions is the number of committed revisions.
	Revisions int32 `json:"revisions,omitempty"`

	// Resumable reports whether the head revision pairs with a memory snapshot
	// (its memorySnapshotRef is set), so a sandbox bound to the workspace can
	// resume from the captured VM state rather than a cold start.
	Resumable bool `json:"resumable,omitempty"`

	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

// --- WorkspaceRevision ---
//
// A WorkspaceRevision is one immutable, content-addressed point in a
// Workspace's revision DAG. A revision is committed when its ContentManifest is
// a valid content-addressed digest (produced by dehydrate, a later W4 slice; a
// test seam supplies one here). Once committed, the ContentManifest is
// immutable: single-writer-per-revision. Each revision is owner-referenced to
// its Workspace, so deleting the Workspace garbage-collects its revisions.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.spec.workspaceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type WorkspaceRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkspaceRevisionSpec   `json:"spec"`
	Status            WorkspaceRevisionStatus `json:"status,omitempty"`
}

type WorkspaceRevisionSpec struct {
	// WorkspaceRef names the Workspace this revision belongs to.
	WorkspaceRef LocalObjectReference `json:"workspaceRef"`

	// Source records the lineage origin of the revision: a sandbox claim that
	// produced it, or a parent revision it forks from. Exactly one is set in a
	// well-formed lineage; both unset marks a root revision.
	Source RevisionSource `json:"source,omitempty"`

	// ContentManifest is the content-addressed manifest digest of the revision's
	// artifact (a lowercase hex sha256). It is set by dehydrate (a later W4
	// slice). A revision with a valid ContentManifest is committed; once
	// committed the manifest is immutable.
	ContentManifest string `json:"contentManifest,omitempty"`

	// MemorySnapshotRef pairs the revision with a memory snapshot, making the
	// revision resumable. Principal-bound per the secrets policy.
	// +optional
	MemorySnapshotRef *string `json:"memorySnapshotRef,omitempty"`
}

type RevisionSource struct {
	// FromClaim names the sandbox claim whose dehydrate produced this revision.
	// +optional
	FromClaim string `json:"fromClaim,omitempty"`

	// FromWorkspaceRevision names the parent revision this revision forks from,
	// the DAG lineage edge. The parent must be a revision in the same workspace.
	// +optional
	FromWorkspaceRevision *WorkspaceRevisionRef `json:"fromWorkspaceRevision,omitempty"`
}

type WorkspaceRevisionRef struct {
	// Workspace is the name of the workspace the parent revision lives in.
	Workspace string `json:"workspace"`
	// Revision is the name of the parent revision.
	Revision string `json:"revision"`
}

type WorkspaceRevisionPhase string

const (
	WorkspaceRevisionPending   WorkspaceRevisionPhase = "Pending"
	WorkspaceRevisionCommitted WorkspaceRevisionPhase = "Committed"
)

type WorkspaceRevisionStatus struct {
	// Phase is Pending until the revision's ContentManifest is a valid
	// content-addressed digest, then Committed. A committed revision is
	// immutable.
	// +kubebuilder:validation:Enum=Pending;Committed
	Phase WorkspaceRevisionPhase `json:"phase,omitempty"`

	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
type WorkspaceRevisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkspaceRevision `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&Workspace{}, &WorkspaceList{},
		&WorkspaceRevision{}, &WorkspaceRevisionList{},
	)
}
