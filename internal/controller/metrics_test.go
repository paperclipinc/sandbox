package controller_test

// Envtest coverage for the controller-level metrics. These assert the metric
// VALUE moves when the real path runs: a GC orphan sweep bumps
// agentrun_orphan_sweeps_total, and a claim that cannot place (no node with a
// ready snapshot) bumps agentrun_claim_pending_total. The metric values are
// read from controller-runtime's global Registry, where metrics.go registers
// them.

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/controller"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// counterValue gathers the global controller-runtime registry and returns the
// value of the named counter. With matchLabels non-empty it sums only the
// series whose labels match every given pair; empty matches the single
// unlabeled series.
func counterValue(t *testing.T, name string, matchLabels map[string]string) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		var sum float64
		for _, m := range fam.GetMetric() {
			if !labelsMatch(m, matchLabels) {
				continue
			}
			sum += counterOrGauge(m)
		}
		return sum
	}
	return 0
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, lp := range m.GetLabel() {
			if lp.GetName() == k && lp.GetValue() == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func counterOrGauge(m *dto.Metric) float64 {
	if m.Counter != nil {
		return m.Counter.GetValue()
	}
	if m.Gauge != nil {
		return m.Gauge.GetValue()
	}
	return 0
}

// TestOrphanSweepMetricIncrements drives one GC pass that reaps an injected
// orphan and asserts the orphan-sweep counter advanced by exactly one.
func TestOrphanSweepMetricIncrements(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "metrics-node-1", "metrics1-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	before := counterValue(t, "agentrun_orphan_sweeps_total", nil)

	// Inject an orphan VM (no backing claim) old enough to exceed the grace.
	const orphanID = "metrics-orphan-old"
	engine.InjectSandbox(orphanID, time.Now().Add(-10*time.Minute))

	gc := &controller.GarbageCollector{
		Client:      k8sClient,
		Registry:    testRegistry,
		OrphanGrace: 60 * time.Second,
	}
	gc.RunOnce(ctx)

	// Confirm the orphan was actually reaped, then assert the counter moved.
	reaped := false
	for _, id := range engine.TerminatedIDs() {
		if id == orphanID {
			reaped = true
		}
	}
	if !reaped {
		t.Fatalf("orphan %s not reaped; terminated = %v", orphanID, engine.TerminatedIDs())
	}

	after := counterValue(t, "agentrun_orphan_sweeps_total", nil)
	if after != before+1 {
		t.Fatalf("orphan_sweeps_total = %v, want %v (before %v)", after, before+1, before)
	}
}

// TestClaimPendingMetricIncrements creates a claim whose pool has no node with
// a ready snapshot. The claim reconciler sets it Pending and bumps the
// pending-requeue counter.
func TestClaimPendingMetricIncrements(t *testing.T) {
	before := counterValue(t, "agentrun_claim_pending_total", nil)

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "pend-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pend-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "pend-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pend-claim", Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "pend-pool"}},
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

	// With no node registered, selectNode finds nothing and the claim stays
	// Pending. Wait for that phase.
	deadline := time.Now().Add(15 * time.Second)
	pending := false
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "pend-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxPending {
				pending = true
				break
			}
			if got.Status.Phase == v1alpha1.SandboxFailed {
				t.Fatalf("claim failed instead of pending: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !pending {
		t.Fatal("claim did not reach Pending within 15s")
	}

	after := counterValue(t, "agentrun_claim_pending_total", nil)
	if after <= before {
		t.Fatalf("claim_pending_total = %v, want > %v", after, before)
	}
}
