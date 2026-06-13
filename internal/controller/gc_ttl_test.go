package controller_test

// Envtest coverage for TTL cleanup of finished claims. A Terminated claim whose
// FinishedAt is older than its TTL is deleted by a GC pass; one with a recent
// FinishedAt survives.

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// makeTerminatedClaim creates a claim, drives it to Ready on a fake node, then
// stamps it Terminated with the given FinishedAt and TTL via a status update.
// It returns the claim's sandbox id so the test can confirm the finalizer reaps
// the VM on TTL delete.
func makeTerminatedClaim(t *testing.T, prefix string, ttlSeconds int32, finishedAt time.Time) string {
	t.Helper()
	ready := makeReadyClaim(t, prefix, prefix+"-node")

	// Set the spec TTL (a spec update, distinct from the status write below).
	var withSpec v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: ready.Name, Namespace: "default"}, &withSpec); err != nil {
		t.Fatal(err)
	}
	ttl := ttlSeconds
	withSpec.Spec.TTLSecondsAfterFinished = &ttl
	if err := k8sClient.Update(ctx, &withSpec); err != nil {
		t.Fatal(err)
	}

	var got v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: ready.Name, Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	finished := metav1.NewTime(finishedAt)
	got.Status.Phase = v1alpha1.SandboxTerminated
	got.Status.FinishedAt = &finished
	if err := k8sClient.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	return got.Status.SandboxID
}

func TestGCTTLDeletesExpiredFinishedClaim(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "ttl1-node", "ttl1-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Finished well past a 10s TTL.
	sandboxID := makeTerminatedClaim(t, "ttl1", 10, time.Now().Add(-1*time.Hour))

	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	// The claim is deleted (its finalizer drains, reaping the backing VM).
	waitClaimGone(t, "ttl1-claim")
	waitTerminated(t, engine, sandboxID)
}

func TestGCTTLKeepsRecentFinishedClaim(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "ttl2-node", "ttl2-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Finished just now, with a long TTL: must survive the pass.
	makeTerminatedClaim(t, "ttl2", 3600, time.Now())

	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	var got v1alpha1.SandboxClaim
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "ttl2-claim", Namespace: "default"}, &got)
	if apierrors.IsNotFound(err) {
		t.Fatal("recently-finished claim was deleted before its TTL")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !got.DeletionTimestamp.IsZero() {
		t.Fatal("recently-finished claim is being deleted before its TTL")
	}
}
