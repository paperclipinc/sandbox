package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type SandboxTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SandboxTemplateSpec `json:"spec"`
}

type SandboxTemplateSpec struct {
	Image     string           `json:"image"`
	Init      []string         `json:"init,omitempty"`
	Command   []string         `json:"command,omitempty"`
	Env       []corev1.EnvVar  `json:"env,omitempty"`
	Resources SandboxResources `json:"resources,omitempty"`
	Volumes   []SandboxVolume  `json:"volumes,omitempty"`
	Network   *NetworkPolicy   `json:"networkPolicy,omitempty"`

	// Encrypted requests at-rest encryption of the template snapshot and every
	// fork built from it. When true the controller owns the per-template
	// encryption key as a Kubernetes Secret and delivers it to the node over the
	// mTLS RPC; the node never generates or persists the key. Defaults to false
	// (plaintext snapshots, unchanged behavior).
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	Encrypted bool `json:"encrypted,omitempty"`
}

type SandboxResources struct {
	CPU    resource.Quantity `json:"cpu,omitempty"`
	Memory resource.Quantity `json:"memory,omitempty"`
}

type ForkPolicy string

const (
	ForkPolicyFresh    ForkPolicy = "Fresh"
	ForkPolicyShare    ForkPolicy = "Share"
	ForkPolicyClone    ForkPolicy = "Clone"
	ForkPolicySnapshot ForkPolicy = "Snapshot"
)

type SandboxVolume struct {
	// Name identifies the volume; it becomes the host backing-file name and the
	// Firecracker drive id, so it is constrained to a path-safe shape (no dots,
	// no slashes) to prevent traversal out of the sandbox volumes dir. The
	// pattern omits the upper bound (a bounded {m,n} quantifier breaks the
	// controller-gen marker parser on the comma); MaxLength caps the length at 64.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`
	// +kubebuilder:validation:MaxLength=64
	Name       string        `json:"name"`
	Size       string        `json:"size,omitempty"`
	Source     *VolumeSource `json:"source,omitempty"`
	ReadOnly   bool          `json:"readOnly,omitempty"`
	MountPath  string        `json:"mountPath,omitempty"`
	ForkPolicy ForkPolicy    `json:"forkPolicy,omitempty"`

	// For Snapshot fork policy: the CSI snapshot class to use
	SnapshotClass string `json:"snapshotClass,omitempty"`

	// For persistent volumes: the storage class
	StorageClass string `json:"storageClass,omitempty"`
}

type VolumeSource struct {
	S3  *S3VolumeSource  `json:"s3,omitempty"`
	GCS *GCSVolumeSource `json:"gcs,omitempty"`
	PVC *PVCVolumeSource `json:"pvc,omitempty"`
	Git *GitVolumeSource `json:"git,omitempty"`
}

type S3VolumeSource struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
	Region string `json:"region,omitempty"`
}

type GCSVolumeSource struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
}

type PVCVolumeSource struct {
	ClaimName string `json:"claimName"`
}

type GitVolumeSource struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

type EgressPolicy string

const (
	EgressDeny  EgressPolicy = "deny"
	EgressAllow EgressPolicy = "allow"
)

type NetworkPolicy struct {
	Egress EgressPolicy `json:"egress,omitempty"`
	Allow  []string     `json:"allow,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplate `json:"items"`
}

// --- SandboxPool ---

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readySnapshots
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readySnapshots`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SandboxPoolSpec   `json:"spec"`
	Status            SandboxPoolStatus `json:"status,omitempty"`
}

type SnapshotTrigger string

const (
	SnapshotAfterReady SnapshotTrigger = "Ready"
)

// HuskDrainPolicy governs what happens to an ACTIVE sandbox (a claim that has
// activated a husk pod) when that husk pod is lost: a node drain, an eviction,
// or any pod deletion that removes the backing VM. It does not change the
// warm-pool self-heal (the pool always recreates a deleted dormant slot); it
// only governs the active sandbox on top of a lost pod.
type HuskDrainPolicy string

const (
	// DrainKill is the default: when an active claim's husk pod is lost, the
	// claim re-pends (Phase Pending, Endpoint/Node cleared) and re-activates on
	// a replacement dormant slot. The in-VM state of the lost pod is gone; the
	// agent reconnects to a fresh fork from the template snapshot. This is the
	// boring, always-available behavior.
	DrainKill HuskDrainPolicy = "Kill"
	// DrainCheckpoint asks the controller to checkpoint the live VM (forkd
	// ForkRunning/CreateSnapshot) BEFORE re-pending, so the agent can resume
	// from the captured state. The live snapshot only runs where the VMM still
	// runs (a graceful drain on a KVM-capable kubelet); on an already-deleted
	// pod there is nothing left to checkpoint, so Checkpoint degrades to the
	// same re-pend as Kill, with a logged note.
	DrainCheckpoint HuskDrainPolicy = "Checkpoint"
)

type SandboxPoolSpec struct {
	TemplateRef   LocalObjectReference `json:"templateRef"`
	Replicas      int32                `json:"replicas"`
	SnapshotAfter SnapshotTrigger      `json:"snapshotAfter,omitempty"`

	// Delay after the trigger condition before taking the snapshot.
	// Allows init scripts to finish.
	SnapshotDelay *metav1.Duration `json:"snapshotDelay,omitempty"`

	// Whether to scale down the source sandbox after snapshot.
	ScaleDownAfterSnapshot bool `json:"scaleDownAfterSnapshot,omitempty"`

	// Where to store snapshot artifacts on the node.
	SnapshotStorage string `json:"snapshotStorage,omitempty"`

	// DrainPolicy governs an active sandbox (a claim that activated a husk pod)
	// when that husk pod is lost (drain, eviction, deletion). Kill (the default)
	// re-pends the claim onto a replacement dormant slot. Checkpoint attempts a
	// live-VM snapshot first where the VMM still runs, then re-pends. Only used
	// in husk mode.
	// +kubebuilder:validation:Enum=Kill;Checkpoint
	// +kubebuilder:default=Kill
	// +optional
	DrainPolicy HuskDrainPolicy `json:"drainPolicy,omitempty"`

	// Autoscale turns the warm pool from a fixed Replicas pre-fill into a
	// demand-driven autoscaler: it scales the number of DORMANT husk pods up
	// under claim pressure and back down to a floor after an idle cooldown.
	// When nil the pool keeps exactly the legacy behavior (the warm pool is held
	// at Replicas). When set, the desired dormant count is
	// clamp(inUse + targetSpare, minWarm, maxWarm) and Replicas is ignored for
	// warm-pool sizing (it stays the scale-subresource target for back-compat).
	// +optional
	Autoscale *PoolAutoscaleSpec `json:"autoscale,omitempty"`
}

// PoolAutoscaleSpec configures demand-driven warm-pool autoscaling for a husk
// pool. It only governs the count of DORMANT (unclaimed) husk pods; an active
// claim's pod is never scaled away. All counts are dormant-pod counts.
type PoolAutoscaleSpec struct {
	// MinWarm is the floor: the dormant pod count the pool holds when fully idle.
	// Operators who want the legacy fixed pool set MinWarm equal to Replicas.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	MinWarm int32 `json:"minWarm,omitempty"`

	// MaxWarm is the ceiling: the dormant pod count is never driven above this,
	// regardless of demand, so a burst cannot create unbounded pods. Required
	// when autoscaling is enabled.
	// +kubebuilder:validation:Minimum=1
	MaxWarm int32 `json:"maxWarm"`

	// TargetSpare is the headroom kept on top of the current in-use count: the
	// pool aims to always have this many SPARE dormant pods ready, so a claim
	// burst up to TargetSpare within one reconcile interval hits a ready dormant
	// pod instead of cold-starting. Default 2.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	// +optional
	TargetSpare int32 `json:"targetSpare,omitempty"`

	// ScaleDownCooldownSeconds is the anti-thrash window: after the pool reduces
	// the dormant count it will not reduce it again until this many seconds have
	// elapsed with NO claim arrival. Scale UP is never delayed. Default 300.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=300
	// +optional
	ScaleDownCooldownSeconds int32 `json:"scaleDownCooldownSeconds,omitempty"`
}

type SandboxPoolStatus struct {
	ReadySnapshots   int32              `json:"readySnapshots"`
	TotalSnapshots   int32              `json:"totalSnapshots"`
	RestoringCount   int32              `json:"restoringCount"`
	LastSnapshotTime *metav1.Time       `json:"lastSnapshotTime,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	NodeDistribution map[string]int32   `json:"nodeDistribution,omitempty"`

	// TemplateDigest is the content-addressed manifest digest of the pool's
	// snapshot, as reported by the node(s) holding it. It makes the snapshot
	// identity visible in the CRD so integrity can be audited. A content
	// address, safe to log.
	TemplateDigest string `json:"templateDigest,omitempty"`

	// DesiredWarm is the autoscaler's computed desired dormant pod count as of
	// the last reconcile. Equal to Replicas when autoscaling is off. Observability
	// only; mirrors the mitos_pool_desired_warm gauge.
	DesiredWarm int32 `json:"desiredWarm,omitempty"`

	// LastScaleDownTime records when the pool last reduced its dormant pod count,
	// so an operator can see the scale-down cooldown state.
	LastScaleDownTime *metav1.Time `json:"lastScaleDownTime,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxPool `json:"items"`
}

// --- SandboxClaim ---

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.node`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type SandboxClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SandboxClaimSpec   `json:"spec"`
	Status            SandboxClaimStatus `json:"status,omitempty"`
}

type SandboxClaimSpec struct {
	PoolRef LocalObjectReference `json:"poolRef"`
	Env     []corev1.EnvVar      `json:"env,omitempty"`
	Secrets []SecretMount        `json:"secrets,omitempty"`

	// Override fork policies for specific volumes on this claim.
	VolumeOverrides []VolumeOverride `json:"volumeOverrides,omitempty"`

	// Maximum wall-clock time for this sandbox (maxLifetime). Zero means no
	// limit.
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// IdleTimeout reaps the sandbox after this much time with no exec or file
	// activity. Zero means no idle limit.
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// Node preference. Empty means any node with capacity.
	NodeName string `json:"nodeName,omitempty"`

	// WorkspaceRef binds this sandbox to a durable Workspace. On activation the
	// workspace's head revision is hydrated into the sandbox /workspace; on
	// terminate the /workspace is dehydrated into a new committed
	// WorkspaceRevision with fromClaim lineage, and the workspace head advances.
	// A Workspace is single-writer: a claim referencing a workspace already bound
	// to another active claim pends with reason WorkspaceBusy. Empty leaves the
	// claim's /workspace ephemeral (no hydrate/dehydrate), exactly as before.
	// +optional
	WorkspaceRef *LocalObjectReference `json:"workspaceRef,omitempty"`

	// ServiceAccount is the principal this claim runs as, the identity the
	// workspace grants are evaluated against and the principal a memory snapshot
	// captured on terminate is bound to. A memory snapshot is never paired or
	// served across principals: a resume only loads a head's memory image when the
	// activating claim's ServiceAccount matches the principal that captured it.
	// Empty is the unnamed default principal.
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// CheckpointOnTerminate asks the controller to capture the sandbox's VM memory
	// snapshot when this claim terminates, pairing it with the new
	// WorkspaceRevision (memorySnapshotRef) so the workspace head becomes
	// resumable: a later claim with the same principal can resume from the
	// captured VM state instead of a cold start. Requires WorkspaceRef. A plain
	// terminate (this unset) leaves the revision's memorySnapshotRef nil.
	// +optional
	CheckpointOnTerminate bool `json:"checkpointOnTerminate,omitempty"`

	// TTLSecondsAfterFinished bounds how long a finished claim (terminal
	// Terminated or Failed phase) lingers in the API after FinishedAt before the
	// garbage collector deletes it, freeing etcd. Unset uses the controller's
	// default. Job-like semantics.
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// Outputs declares what the terminate-with-outputs step captures from the
	// sandbox /workspace into the new WorkspaceRevision. With no Path entry the
	// whole workspace is captured (the default). Each Path entry narrows the
	// capture to that subtree; a Diff entry records the content-hash diff of the
	// new revision against the workspace head before it; a Git entry pushes the
	// workspace repo paths to a rendezvous remote on a per-attempt branch (git is
	// the merge layer, the engine never merges working trees). Requires
	// WorkspaceRef; ignored otherwise.
	// +optional
	Outputs []OutputSpec `json:"outputs,omitempty"`
}

// OutputSpec is one terminate-with-outputs directive. At most one of Path, Diff,
// or Git is meaningful per entry, mirroring the v2 spec onTerminate.outputs
// shape (docs/api/v2-spec.md).
type OutputSpec struct {
	// Path narrows the captured revision to this /workspace subtree (a prefix).
	// When any output sets Path, only the union of those subtrees is captured;
	// with no Path output the whole workspace is captured.
	// +optional
	Path string `json:"path,omitempty"`

	// Diff requests that the new revision record the content-hash diff (added,
	// removed, modified paths) against the workspace head revision before it.
	// +optional
	Diff bool `json:"diff,omitempty"`

	// Git pushes the workspace spec.git.paths content to a rendezvous remote on a
	// per-attempt branch. Git is the merge layer: the engine pushes branches, a
	// human or CI merges them, the engine never merges working trees.
	// +optional
	Git *GitOutput `json:"git,omitempty"`
}

// GitOutput declares a git rendezvous push target for a terminate output.
type GitOutput struct {
	// Remote is the rendezvous git remote the workspace repo paths are pushed to.
	// Operator-declared (an external egress; see docs/threat-model.md). The
	// pattern restricts the remote to safe transports (https/http/ssh/git/file
	// URLs and scp-like git@host:path), so admission rejects flag-shaped values
	// (a leading "-") and the dangerous ext::/fd:: transports at the API edge.
	// +optional
	// +kubebuilder:validation:Pattern=`^(https://|http://|ssh://|git://|file://|[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:).+`
	Remote string `json:"remote,omitempty"`

	// Branch is the per-attempt branch the push lands on. It is a text/template
	// rendered with the claim name as {{.name}}, for example "attempt/{{.name}}".
	// +optional
	Branch string `json:"branch,omitempty"`
}

type SecretMount struct {
	Name      string                   `json:"name"`
	SecretRef corev1.SecretKeySelector `json:"secretRef"`
	EnvVar    string                   `json:"envVar,omitempty"`
	MountPath string                   `json:"mountPath,omitempty"`
}

type VolumeOverride struct {
	Name       string     `json:"name"`
	ForkPolicy ForkPolicy `json:"forkPolicy"`
}

type SandboxPhase string

const (
	SandboxPending     SandboxPhase = "Pending"
	SandboxRestoring   SandboxPhase = "Restoring"
	SandboxReady       SandboxPhase = "Ready"
	SandboxTerminating SandboxPhase = "Terminating"
	SandboxTerminated  SandboxPhase = "Terminated"
	SandboxFailed      SandboxPhase = "Failed"
)

type SandboxClaimStatus struct {
	Phase          SandboxPhase `json:"phase,omitempty"`
	Endpoint       string       `json:"endpoint,omitempty"`
	Node           string       `json:"node,omitempty"`
	SandboxID      string       `json:"sandboxID,omitempty"`
	ForkTimeMicros int64        `json:"forkTimeMicros,omitempty"`
	StartedAt      *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is set when the claim reaches a terminal phase (Terminated or
	// Failed), so the GC TTL pass can reap it.
	FinishedAt *metav1.Time       `json:"finishedAt,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClaim `json:"items"`
}

// --- SandboxFork ---

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceRef.name`
// +kubebuilder:printcolumn:name="Forks",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyForks`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type SandboxFork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SandboxForkSpec   `json:"spec"`
	Status            SandboxForkStatus `json:"status,omitempty"`
}

type SandboxForkSpec struct {
	SourceRef LocalObjectReference `json:"sourceRef"`
	Replicas  int32                `json:"replicas"`

	// Override fork policies for specific volumes.
	VolumeOverrides []VolumeOverride `json:"volumeOverrides,omitempty"`

	// Whether to pause the source sandbox during checkpoint.
	// Reduces checkpoint time but causes a brief interruption.
	PauseSource bool `json:"pauseSource,omitempty"`

	// AllowSecretInheritance permits forking a sandbox whose claim holds
	// secrets. A live fork duplicates guest memory, including any delivered
	// secret values, into every fork. Default is to reject such forks; see
	// docs/fork-correctness.md §3. The long-term default is per-fork
	// credential reissue.
	AllowSecretInheritance bool `json:"allowSecretInheritance,omitempty"`
}

type ForkInfo struct {
	Name           string       `json:"name"`
	SandboxID      string       `json:"sandboxID"`
	Endpoint       string       `json:"endpoint"`
	Node           string       `json:"node"`
	Phase          SandboxPhase `json:"phase"`
	ForkTimeMicros int64        `json:"forkTimeMicros,omitempty"`
}

type SandboxForkStatus struct {
	ReadyForks     int32              `json:"readyForks"`
	TotalForks     int32              `json:"totalForks"`
	Forks          []ForkInfo         `json:"forks,omitempty"`
	CheckpointTime *metav1.Time       `json:"checkpointTime,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxForkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxFork `json:"items"`
}

// --- Shared types ---

type LocalObjectReference struct {
	Name string `json:"name"`
}

func init() {
	SchemeBuilder.Register(
		&SandboxTemplate{}, &SandboxTemplateList{},
		&SandboxPool{}, &SandboxPoolList{},
		&SandboxClaim{}, &SandboxClaimList{},
		&SandboxFork{}, &SandboxForkList{},
	)
}
