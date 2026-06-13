package controller_test

// Envtest coverage for Task 2: the claim reconciler reads the template's
// Volumes (applying any VolumeOverrides) and passes them through the Fork RPC's
// VolumeMounts. A claim from a template carrying a Fresh volume and a Snapshot
// volume reaches Ready, and the fake forkd's engine records both mounts with
// their name, mount path, and fork policy.

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/volume"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestClaimPlumbsVolumesToForkd(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "vol-node", "vol-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "vol-tmpl", Namespace: "default"},
		Spec: v1alpha1.SandboxTemplateSpec{
			Image: "python:3.12-slim",
			Volumes: []v1alpha1.SandboxVolume{
				{
					Name:       "scratch",
					Size:       "512Mi",
					MountPath:  "/scratch",
					ForkPolicy: v1alpha1.ForkPolicyFresh,
				},
				{
					Name:       "cache",
					MountPath:  "/cache",
					ForkPolicy: v1alpha1.ForkPolicySnapshot,
				},
			},
		},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "vol-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "vol-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "vol-claim", Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "vol-pool"}},
	}
	for _, obj := range []client.Object{template, pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	waitClaimReady(t, "vol-claim")

	// The fake forkd's engine must have recorded the VolumeMounts the claim
	// path sent. Poll briefly because Fork is recorded just before Ready.
	var got []volume.Spec
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := engine.LastForkVolumes(); len(v) == 2 {
			got = v
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) != 2 {
		t.Fatalf("forkd recorded %d volumes, want 2; template Volumes were not plumbed through the claim path", len(got))
	}

	byName := map[string]volume.Spec{}
	for _, v := range got {
		byName[v.Name] = v
	}

	scratch, ok := byName["scratch"]
	if !ok {
		t.Fatal("scratch volume not recorded")
	}
	if scratch.MountPath != "/scratch" {
		t.Errorf("scratch mount path = %q, want /scratch", scratch.MountPath)
	}
	if scratch.Policy != volume.ForkPolicyFresh {
		t.Errorf("scratch policy = %q, want Fresh", scratch.Policy)
	}
	if scratch.SizeMB != 512 {
		t.Errorf("scratch size = %d MB, want 512", scratch.SizeMB)
	}

	cache, ok := byName["cache"]
	if !ok {
		t.Fatal("cache volume not recorded")
	}
	if cache.MountPath != "/cache" {
		t.Errorf("cache mount path = %q, want /cache", cache.MountPath)
	}
	if cache.Policy != volume.ForkPolicySnapshot {
		t.Errorf("cache policy = %q, want Snapshot", cache.Policy)
	}
}
