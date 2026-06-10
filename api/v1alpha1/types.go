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
}

type SandboxPoolStatus struct {
	ReadySnapshots   int32              `json:"readySnapshots"`
	TotalSnapshots   int32              `json:"totalSnapshots"`
	RestoringCount   int32              `json:"restoringCount"`
	LastSnapshotTime *metav1.Time       `json:"lastSnapshotTime,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	NodeDistribution map[string]int32   `json:"nodeDistribution,omitempty"`
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

	// Maximum wall-clock time for this sandbox. Zero means no limit.
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Node preference. Empty means any node with capacity.
	NodeName string `json:"nodeName,omitempty"`
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
	SandboxFailed      SandboxPhase = "Failed"
)

type SandboxClaimStatus struct {
	Phase          SandboxPhase       `json:"phase,omitempty"`
	Endpoint       string             `json:"endpoint,omitempty"`
	Node           string             `json:"node,omitempty"`
	SandboxID      string             `json:"sandboxID,omitempty"`
	ForkTimeMicros int64              `json:"forkTimeMicros,omitempty"`
	StartedAt      *metav1.Time       `json:"startedAt,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
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
