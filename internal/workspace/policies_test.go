package workspace

import (
	"context"
	"testing"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
)

func TestHandlerForPolicy(t *testing.T) {
	tests := []struct {
		policy   v1alpha1.ForkPolicy
		wantType string
	}{
		{v1alpha1.ForkPolicyFresh, "*workspace.FreshHandler"},
		{v1alpha1.ForkPolicyShare, "*workspace.ShareHandler"},
		{v1alpha1.ForkPolicyClone, "*workspace.CloneHandler"},
		{v1alpha1.ForkPolicySnapshot, "*workspace.SnapshotHandler"},
		{"", "*workspace.FreshHandler"},
	}

	for _, tt := range tests {
		handler := HandlerForPolicy(tt.policy)
		if handler == nil {
			t.Errorf("HandlerForPolicy(%q) returned nil", tt.policy)
		}
	}
}

func TestFreshHandler_Prepare(t *testing.T) {
	handler := &FreshHandler{}
	vol := v1alpha1.SandboxVolume{
		Name:      "scratch",
		MountPath: "/tmp",
	}

	pv, err := handler.Prepare(context.Background(), vol, "sandbox-1")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if pv.Name != "scratch" {
		t.Errorf("expected name 'scratch', got %q", pv.Name)
	}
	if pv.MountPath != "/tmp" {
		t.Errorf("expected mountPath '/tmp', got %q", pv.MountPath)
	}
	if pv.ReadOnly {
		t.Error("Fresh volumes should not be read-only")
	}
}

func TestShareHandler_Prepare(t *testing.T) {
	handler := &ShareHandler{}
	vol := v1alpha1.SandboxVolume{
		Name:      "data",
		MountPath: "/data",
		ReadOnly:  true,
		Source: &v1alpha1.VolumeSource{
			S3: &v1alpha1.S3VolumeSource{
				Bucket: "my-datasets",
			},
		},
	}

	pv, err := handler.Prepare(context.Background(), vol, "sandbox-1")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if !pv.ReadOnly {
		t.Error("Share volume should preserve ReadOnly from template")
	}
	if pv.HostPath == "" {
		t.Error("expected non-empty host path for shared S3 volume")
	}
}

func TestShareHandler_S3Path(t *testing.T) {
	vol := v1alpha1.SandboxVolume{
		Source: &v1alpha1.VolumeSource{
			S3: &v1alpha1.S3VolumeSource{Bucket: "test-bucket"},
		},
	}
	path := resolveSharedPath(vol)
	if path != "/var/lib/agent-run/shared/s3/test-bucket" {
		t.Errorf("unexpected S3 path: %s", path)
	}
}

func TestShareHandler_GCSPath(t *testing.T) {
	vol := v1alpha1.SandboxVolume{
		Source: &v1alpha1.VolumeSource{
			GCS: &v1alpha1.GCSVolumeSource{Bucket: "gcs-bucket"},
		},
	}
	path := resolveSharedPath(vol)
	if path != "/var/lib/agent-run/shared/gcs/gcs-bucket" {
		t.Errorf("unexpected GCS path: %s", path)
	}
}

func TestShareHandler_PVCPath(t *testing.T) {
	vol := v1alpha1.SandboxVolume{
		Source: &v1alpha1.VolumeSource{
			PVC: &v1alpha1.PVCVolumeSource{ClaimName: "my-pvc"},
		},
	}
	path := resolveSharedPath(vol)
	if path != "/var/lib/agent-run/shared/pvc/my-pvc" {
		t.Errorf("unexpected PVC path: %s", path)
	}
}

func TestCloneHandler_Prepare(t *testing.T) {
	handler := &CloneHandler{}
	vol := v1alpha1.SandboxVolume{
		Name:      "state",
		MountPath: "/state",
	}

	pv, err := handler.Prepare(context.Background(), vol, "sandbox-1")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if pv.ReadOnly {
		t.Error("Cloned volume should be writable")
	}
	if pv.Name != "state" {
		t.Errorf("expected name 'state', got %q", pv.Name)
	}
}

func TestSnapshotHandler_Prepare(t *testing.T) {
	handler := &SnapshotHandler{}
	vol := v1alpha1.SandboxVolume{
		Name:      "workspace",
		MountPath: "/workspace",
	}

	pv, err := handler.Prepare(context.Background(), vol, "sandbox-1")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if pv.ReadOnly {
		t.Error("Snapshot volume should be writable (CoW)")
	}
}
