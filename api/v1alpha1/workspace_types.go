package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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
	// that holds the workspace's chunked revision artifacts. When set, the
	// workspace's hydrate/dehydrate artifacts live in the referenced S3 bucket
	// (an alternative to the node CAS), with the same content-addressed,
	// byte-identical, deduplicated semantics. The bucket and credentials are
	// resolved from S3 below; unset keeps the default node CAS.
	ObjectStorageRef string `json:"objectStorageRef,omitempty"`

	// S3 configures the S3-compatible object-store backend that holds the
	// workspace's revision artifacts when ObjectStorageRef selects it. Credentials
	// are taken from a referenced Secret and are never logged.
	// +optional
	S3 *WorkspaceS3Store `json:"s3,omitempty"`

	// EncryptionKeyRef names a Secret key holding the wrapped at-rest
	// data-encryption key (DEK). When set, every revision chunk and manifest is
	// encrypted at rest with AES-256-GCM under that DEK before it reaches the
	// store (node CAS or S3), and decrypted on hydrate. The DEK the controller
	// generates is keyed per-template (by templateID), so a template's workspaces
	// share its DEK; per-workspace crypto-shred is a future option if finer
	// granularity is wanted. The DEK is a secret VALUE
	// delivered wrapped (envelope encryption via internal/kms): it is unwrapped
	// only in node memory for the duration of a hydrate/dehydrate and is never
	// logged, never placed in an error or condition, and never written to a host
	// path. The encrypted round trip stays byte-identical and content-addressed
	// dedup is preserved because the manifest digest is computed over plaintext.
	// The key is principal-bound, mirroring the memory-snapshot policy. Unset
	// keeps today's plaintext store path (backward compatible).
	// +optional
	EncryptionKeyRef *corev1.SecretKeySelector `json:"encryptionKeyRef,omitempty"`
}

// WorkspaceS3Store configures the S3-compatible object-store backend for a
// workspace's content-addressed revision artifacts.
type WorkspaceS3Store struct {
	// Bucket is the S3 bucket that holds the workspace's chunks and manifests.
	Bucket string `json:"bucket"`

	// Endpoint is the S3 endpoint URL. Empty uses the AWS default for the region;
	// set it for S3-compatible stores (MinIO, Ceph RGW, etc).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Region is the S3 region. Defaults to us-east-1 when unset (the path-style
	// default many S3-compatible stores accept).
	// +optional
	Region string `json:"region,omitempty"`

	// Prefix is an optional key prefix under which the chunks/ and manifests/
	// object keys are written, so several workspaces can share one bucket without
	// colliding. The content-addressed layout under the prefix is unchanged.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// CredentialsSecretRef names a Secret holding the S3 access-key id and
	// secret-access-key used to authenticate to the bucket. The secret values are
	// resolved at use time, passed to the S3 client only in memory, and never
	// logged, never put in an error, and never written to a host path. The
	// conventional keys are "accessKeyId" and "secretAccessKey" unless overridden
	// by AccessKeyIDKey / SecretAccessKeyKey.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`

	// AccessKeyIDKey is the Secret key holding the S3 access-key id. Defaults to
	// "accessKeyId".
	// +optional
	AccessKeyIDKey string `json:"accessKeyIdKey,omitempty"`

	// SecretAccessKeyKey is the Secret key holding the S3 secret-access-key.
	// Defaults to "secretAccessKey".
	// +optional
	SecretAccessKeyKey string `json:"secretAccessKeyKey,omitempty"`
}

type WorkspaceGit struct {
	// Paths are the repo paths inside the workspace that get version history and
	// the rendezvous remote.
	Paths []string `json:"paths,omitempty"`

	// CredentialsSecretRef names a Secret key holding the credential token used to
	// authenticate the {git} rendezvous push to an external remote. The token is a
	// secret VALUE: it is resolved at push time, delivered to git only through an
	// ephemeral credentials file in an isolated HOME, and never logged, never put
	// on the git argv, and never recorded in a condition or revision. The username
	// is taken from CredentialsUsername (or defaults to "x-access-token", the
	// token-only forge convention). Unset means an unauthenticated push (a local
	// bare repo or an already-credentialed remote helper on the host).
	// +optional
	CredentialsSecretRef *corev1.SecretKeySelector `json:"credentialsSecretRef,omitempty"`

	// CredentialsUsername is the basic-auth username paired with the token from
	// CredentialsSecretRef. It is not a secret. Defaults to "x-access-token" when
	// unset, which most token-based forges accept. Ignored when CredentialsSecretRef
	// is unset.
	// +optional
	CredentialsUsername string `json:"credentialsUsername,omitempty"`
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

	// MemorySnapshotPrincipal is the principal (the capturing claim's
	// ServiceAccount) the paired memory snapshot is bound to. A memory snapshot
	// carries secrets-in-RAM, so it is never served across principals: a resume
	// loads the memory image only when the activating claim's principal matches
	// this value. Set together with MemorySnapshotRef; nil for a content-only
	// revision.
	// +optional
	MemorySnapshotPrincipal *string `json:"memorySnapshotPrincipal,omitempty"`
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

	// DiffSummary records the content-hash diff of this revision against the
	// workspace head revision before it, when a terminate {diff: true} output
	// requested it. It is a path-level summary (added, removed, modified file
	// names plus counts); the file contents stay in the content store, never in
	// the status. Unset when no diff was requested or the revision is a root.
	// +optional
	DiffSummary *RevisionDiffSummary `json:"diffSummary,omitempty"`

	// GitPushes records the git rendezvous pushes this revision's terminate
	// performed (the per-attempt branch and the remote), so the fork-and-merge
	// rendezvous is auditable. The push content is the workspace repo paths; git
	// is the merge layer and the engine never merges working trees.
	// +optional
	GitPushes []GitPushRecord `json:"gitPushes,omitempty"`

	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

// RevisionDiffSummary is the path-level content-hash diff of a revision against
// its parent. The lists hold workspace-relative file names; a modified file
// changed content (its chunk digests differ). Renames appear as a remove plus an
// add on the workspace side (git handles renames on the repo-paths side).
type RevisionDiffSummary struct {
	// ParentRevision is the head revision the diff was computed against. Empty
	// when this revision is the first in the workspace (a root: everything added).
	// +optional
	ParentRevision string `json:"parentRevision,omitempty"`

	Added    []string `json:"added,omitempty"`
	Removed  []string `json:"removed,omitempty"`
	Modified []string `json:"modified,omitempty"`

	// AddedCount, RemovedCount, and ModifiedCount summarize the lists for a quick
	// human read and for indexers that do not want the full path lists.
	AddedCount    int32 `json:"addedCount,omitempty"`
	RemovedCount  int32 `json:"removedCount,omitempty"`
	ModifiedCount int32 `json:"modifiedCount,omitempty"`
}

// GitPushRecord is one git rendezvous push performed on terminate.
type GitPushRecord struct {
	// Remote is the rendezvous remote the workspace repo paths were pushed to.
	Remote string `json:"remote,omitempty"`
	// Branch is the per-attempt branch the push landed on.
	Branch string `json:"branch,omitempty"`
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
