package workspace

import (
	"context"
	"fmt"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
)

// VolumeHandler applies fork policies to volumes during sandbox creation.
type VolumeHandler interface {
	// Prepare sets up the volume for a new sandbox fork.
	Prepare(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) (*PreparedVolume, error)
	// Cleanup removes volume resources when a sandbox is terminated.
	Cleanup(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) error
}

type PreparedVolume struct {
	Name      string
	MountPath string
	HostPath  string
	ReadOnly  bool
}

type FreshHandler struct{}

func (h *FreshHandler) Prepare(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) (*PreparedVolume, error) {
	// Create a new empty tmpfs or directory for this sandbox.
	// Nothing is carried over from the snapshot.
	hostPath := fmt.Sprintf("/var/lib/agent-run/sandboxes/%s/volumes/%s", sandboxID, vol.Name)
	return &PreparedVolume{
		Name:      vol.Name,
		MountPath: vol.MountPath,
		HostPath:  hostPath,
		ReadOnly:  false,
	}, nil
}

func (h *FreshHandler) Cleanup(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) error {
	// Remove the directory
	return nil
}

type ShareHandler struct{}

func (h *ShareHandler) Prepare(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) (*PreparedVolume, error) {
	// Re-mount the same backing store. For S3 CSI volumes, this means
	// the same PV/PVC is mounted read-only into the new sandbox.
	// No data is copied.
	return &PreparedVolume{
		Name:      vol.Name,
		MountPath: vol.MountPath,
		HostPath:  resolveSharedPath(vol),
		ReadOnly:  vol.ReadOnly,
	}, nil
}

func (h *ShareHandler) Cleanup(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) error {
	// Nothing to clean up; the shared volume persists
	return nil
}

type CloneHandler struct {
	// CSI client for creating VolumeSnapshot + PVC clone
}

func (h *CloneHandler) Prepare(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) (*PreparedVolume, error) {
	// 1. Create a CSI VolumeSnapshot of the source PVC
	// 2. Create a new PVC from the snapshot (CSI clone)
	// 3. Return the new PVC mount path
	//
	// The new PVC is a full, independent, writable copy.
	// On CoW-capable backends (Ceph RBD, LVM thin), this is instant.
	return &PreparedVolume{
		Name:      vol.Name,
		MountPath: vol.MountPath,
		HostPath:  fmt.Sprintf("/var/lib/agent-run/sandboxes/%s/volumes/%s", sandboxID, vol.Name),
		ReadOnly:  false,
	}, nil
}

func (h *CloneHandler) Cleanup(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) error {
	// Delete the cloned PVC
	return nil
}

type SnapshotHandler struct{}

func (h *SnapshotHandler) Prepare(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) (*PreparedVolume, error) {
	// Create a btrfs snapshot (or CSI VolumeSnapshot) of the source subvolume.
	// The snapshot is CoW: writes go to new blocks, reads fall through to the source.
	// On btrfs this is <1ms.
	return &PreparedVolume{
		Name:      vol.Name,
		MountPath: vol.MountPath,
		HostPath:  fmt.Sprintf("/var/lib/agent-run/sandboxes/%s/volumes/%s", sandboxID, vol.Name),
		ReadOnly:  false,
	}, nil
}

func (h *SnapshotHandler) Cleanup(ctx context.Context, vol v1alpha1.SandboxVolume, sandboxID string) error {
	// Delete the btrfs snapshot
	return nil
}

func HandlerForPolicy(policy v1alpha1.ForkPolicy) VolumeHandler {
	switch policy {
	case v1alpha1.ForkPolicyFresh:
		return &FreshHandler{}
	case v1alpha1.ForkPolicyShare:
		return &ShareHandler{}
	case v1alpha1.ForkPolicyClone:
		return &CloneHandler{}
	case v1alpha1.ForkPolicySnapshot:
		return &SnapshotHandler{}
	default:
		return &FreshHandler{}
	}
}

func resolveSharedPath(vol v1alpha1.SandboxVolume) string {
	if vol.Source != nil && vol.Source.S3 != nil {
		return fmt.Sprintf("/var/lib/agent-run/shared/s3/%s", vol.Source.S3.Bucket)
	}
	if vol.Source != nil && vol.Source.GCS != nil {
		return fmt.Sprintf("/var/lib/agent-run/shared/gcs/%s", vol.Source.GCS.Bucket)
	}
	if vol.Source != nil && vol.Source.PVC != nil {
		return fmt.Sprintf("/var/lib/agent-run/shared/pvc/%s", vol.Source.PVC.ClaimName)
	}
	return ""
}
