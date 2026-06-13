package controller

import (
	"context"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// newAutoscaleFakeClient builds an in-memory client seeded with objs and the
// scheme the controller needs (corev1 + mitos), for the pure-logic autoscale
// tests that do not need envtest.
func newAutoscaleFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1 to scheme: %v", err)
	}
	return fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func autoPool(min, max, spare, cooldownSec int32) *v1alpha1.SandboxPool {
	return &v1alpha1.SandboxPool{
		Spec: v1alpha1.SandboxPoolSpec{
			Replicas: 3,
			Autoscale: &v1alpha1.PoolAutoscaleSpec{
				MinWarm:                  min,
				MaxWarm:                  max,
				TargetSpare:              spare,
				ScaleDownCooldownSeconds: cooldownSec,
			},
		},
	}
}

func TestComputeDesiredWarm(t *testing.T) {
	t0 := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name           string
		pool           *v1alpha1.SandboxPool
		dormant        int32
		inUse          int32
		lastClaim      time.Time
		now            time.Time
		want           int32
		wantScaledDown bool
	}{
		{
			name:    "autoscale nil uses Replicas",
			pool:    &v1alpha1.SandboxPool{Spec: v1alpha1.SandboxPoolSpec{Replicas: 4}},
			dormant: 1, inUse: 0, now: t0, want: 4,
		},
		{
			// inUse(0)+spare(2)=2 clamped to [1,10]=2: idle floats to the spare
			// floor (above MinWarm here), and scaling down from 5 to 2 after the
			// cooldown is a scale-down.
			name:    "idle pool floats to spare floor after cooldown",
			pool:    autoPool(1, 10, 2, 300),
			dormant: 5, inUse: 0, lastClaim: t0.Add(-10 * time.Minute), now: t0,
			want: 2, wantScaledDown: true,
		},
		{
			// With targetSpare 0 and MinWarm 1, a fully idle pool floats down to
			// MinWarm exactly: clamp(0+0,1,10)=1.
			name:    "idle pool with zero spare floats to MinWarm after cooldown",
			pool:    autoPool(1, 10, 0, 300),
			dormant: 5, inUse: 0, lastClaim: t0.Add(-10 * time.Minute), now: t0,
			want: 1, wantScaledDown: true,
		},
		{
			name:    "scale down blocked inside cooldown holds current dormant",
			pool:    autoPool(1, 10, 2, 300),
			dormant: 5, inUse: 0, lastClaim: t0.Add(-10 * time.Second), now: t0,
			want: 5, wantScaledDown: false,
		},
		{
			name:    "in-use plus spare drives desired up immediately",
			pool:    autoPool(1, 10, 2, 300),
			dormant: 2, inUse: 4, lastClaim: t0.Add(-1 * time.Second), now: t0,
			want: 6, wantScaledDown: false,
		},
		{
			name:    "desired clamps to MaxWarm",
			pool:    autoPool(1, 10, 2, 300),
			dormant: 8, inUse: 20, lastClaim: t0, now: t0,
			want: 10, wantScaledDown: false,
		},
		{
			name:    "MaxWarm below MinWarm clamps MinWarm down to MaxWarm",
			pool:    autoPool(8, 4, 2, 300),
			dormant: 0, inUse: 0, lastClaim: t0.Add(-time.Hour), now: t0,
			want: 4, wantScaledDown: false,
		},
		{
			name:    "scale up allowed even inside cooldown",
			pool:    autoPool(1, 10, 2, 300),
			dormant: 2, inUse: 5, lastClaim: t0.Add(-5 * time.Second), now: t0,
			want: 7, wantScaledDown: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var last *metav1.Time
			if !tc.lastClaim.IsZero() {
				mt := metav1.NewTime(tc.lastClaim)
				last = &mt
			}
			got, scaledDown := computeDesiredWarm(tc.pool, tc.dormant, tc.inUse, last, tc.now)
			if got != tc.want {
				t.Fatalf("desired = %d, want %d", got, tc.want)
			}
			if scaledDown != tc.wantScaledDown {
				t.Fatalf("scaledDown = %v, want %v", scaledDown, tc.wantScaledDown)
			}
		})
	}
}

func TestPoolDemandTracker(t *testing.T) {
	d := NewPoolDemand()
	key := "ns/poolA"

	if _, ok := d.LastArrival(key); ok {
		t.Fatal("expected no arrival recorded yet")
	}

	t1 := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	d.RecordArrival(key, t1)
	got, ok := d.LastArrival(key)
	if !ok || !got.Equal(t1) {
		t.Fatalf("LastArrival = %v ok=%v, want %v", got, ok, t1)
	}

	// A later arrival advances the timestamp; an earlier one does not move it back.
	t2 := t1.Add(time.Minute)
	d.RecordArrival(key, t2)
	d.RecordArrival(key, t1)
	got, _ = d.LastArrival(key)
	if !got.Equal(t2) {
		t.Fatalf("LastArrival = %v, want the latest %v", got, t2)
	}

	// Independent pools are tracked independently.
	if _, ok := d.LastArrival("ns/poolB"); ok {
		t.Fatal("poolB must be independent of poolA")
	}
}

func TestPoolDemandForget(t *testing.T) {
	d := NewPoolDemand()
	keyA := "ns/poolA"
	keyB := "ns/poolB"
	t1 := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	d.RecordArrival(keyA, t1)
	d.RecordArrival(keyB, t1)

	// Forget removes only the named pool's entry.
	d.Forget(keyA)
	if _, ok := d.LastArrival(keyA); ok {
		t.Fatal("poolA entry should be gone after Forget")
	}
	if _, ok := d.LastArrival(keyB); !ok {
		t.Fatal("poolB entry must survive Forget(poolA)")
	}

	// Forget of an unknown key is a no-op (no panic).
	d.Forget("ns/never-seen")
}

// metricSeriesExists reports whether the named metric family has at least one
// series carrying the given label key/value pair.
func metricSeriesExists(t *testing.T, name, labelKey, labelVal string) bool {
	t.Helper()
	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == labelKey && l.GetValue() == labelVal {
					return true
				}
			}
		}
	}
	return false
}

func TestForgetPoolMetricsClearsLabelSeries(t *testing.T) {
	const pool = "ns/poolForget"

	// Seed every per-pool warm-pool series for this pool name.
	setPoolReadySnapshots(pool, 1)
	setWarmPoolGauges(pool, 3, 5, 7)
	recordWarmScaleUp(pool)
	recordWarmScaleDown(pool)

	// Sanity: the series exist before cleanup.
	for _, name := range []string{
		"mitos_pool_ready_snapshots",
		"mitos_pool_warm_dormant",
		"mitos_pool_warm_in_use",
		"mitos_pool_desired_warm",
		"mitos_pool_warm_scale_up_total",
		"mitos_pool_warm_scale_down_total",
	} {
		if !metricSeriesExists(t, name, "pool", pool) {
			t.Fatalf("precondition: %s{pool=%q} should exist before Forget", name, pool)
		}
	}

	forgetPoolMetrics(pool)

	for _, name := range []string{
		"mitos_pool_ready_snapshots",
		"mitos_pool_warm_dormant",
		"mitos_pool_warm_in_use",
		"mitos_pool_desired_warm",
		"mitos_pool_warm_scale_up_total",
		"mitos_pool_warm_scale_down_total",
	} {
		if metricSeriesExists(t, name, "pool", pool) {
			t.Fatalf("%s{pool=%q} should be cleared after forgetPoolMetrics", name, pool)
		}
	}
}

func gaugeValueByLabel(t *testing.T, name, labelKey, labelVal string) float64 {
	t.Helper()
	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == labelKey && l.GetValue() == labelVal {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("metric %s{%s=%q} not found", name, labelKey, labelVal)
	return 0
}

func TestWarmPoolMetricsSetters(t *testing.T) {
	// Gauges: set then read back via the testutil gatherer.
	setWarmPoolGauges("ns/poolM", 3, 5, 7)
	if got := gaugeValueByLabel(t, "mitos_pool_warm_dormant", "pool", "ns/poolM"); got != 3 {
		t.Fatalf("dormant gauge = %v, want 3", got)
	}
	if got := gaugeValueByLabel(t, "mitos_pool_warm_in_use", "pool", "ns/poolM"); got != 5 {
		t.Fatalf("in-use gauge = %v, want 5", got)
	}
	if got := gaugeValueByLabel(t, "mitos_pool_desired_warm", "pool", "ns/poolM"); got != 7 {
		t.Fatalf("desired gauge = %v, want 7", got)
	}

	// Counters: bump once each and confirm they are registered (no panic).
	recordWarmScaleUp("ns/poolM")
	recordWarmScaleDown("ns/poolM")

	// Histograms: observe once each (no panic, registered).
	observeRefillLatency(0.5)
	observeClaimWaitForWarm(0.02)
}

func TestReconcileHuskPodsDesiredArg(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pool-uid"},
		Spec:       v1alpha1.SandboxPoolSpec{TemplateRef: v1alpha1.LocalObjectReference{Name: "t"}, Replicas: 2},
	}
	tmpl := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "img"},
	}
	cl := newAutoscaleFakeClient(t, pool, tmpl)
	r := &SandboxPoolReconciler{Client: cl, EnableHuskPods: true, HuskStubImage: "stub:latest"}

	// desired=3 creates 3 dormant pods (existence-counting under no kubelet).
	res, err := r.reconcileHuskPods(context.Background(), pool, tmpl, 3)
	if err != nil {
		t.Fatalf("reconcileHuskPods: %v", err)
	}
	if res.dormant != 3 {
		t.Fatalf("dormant = %d, want 3", res.dormant)
	}
	if res.inUse != 0 {
		t.Fatalf("inUse = %d, want 0", res.inUse)
	}
	if !res.scaledUp {
		t.Fatalf("scaledUp = false, want true on a create")
	}

	// desired=1 deletes the surplus down to 1.
	res, err = r.reconcileHuskPods(context.Background(), pool, tmpl, 1)
	if err != nil {
		t.Fatalf("reconcileHuskPods scale down: %v", err)
	}
	if res.dormant != 1 {
		t.Fatalf("dormant after scale down = %d, want 1", res.dormant)
	}
	if !res.scaledDn {
		t.Fatalf("scaledDn = false, want true on a delete")
	}
}

func TestPoolReconcileAutoscaleDesired(t *testing.T) {
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pool-uid"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "t"},
			Replicas:    1,
			Autoscale:   &v1alpha1.PoolAutoscaleSpec{MinWarm: 1, MaxWarm: 10, TargetSpare: 2, ScaleDownCooldownSeconds: 300},
		},
	}
	tmpl := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "img"},
	}
	cl := newAutoscaleFakeClient(t, pool, tmpl)
	r := &SandboxPoolReconciler{
		Client: cl, EnableHuskPods: true, HuskStubImage: "stub:latest",
		Demand: NewPoolDemand(),
	}

	// No in-use, no demand: desired = clamp(0+2,1,10)=2.
	desired := r.desiredWarm(pool, 0, 0)
	if desired != 2 {
		t.Fatalf("desiredWarm with no in-use = %d, want 2", desired)
	}

	// 4 in-use drives desired to 6.
	if got := r.desiredWarm(pool, 0, 4); got != 6 {
		t.Fatalf("desiredWarm with 4 in-use = %d, want 6", got)
	}
}

func TestClaimRecordsDemandArrival(t *testing.T) {
	d := NewPoolDemand()
	r := &SandboxClaimReconciler{Demand: d, Now: func() time.Time {
		return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	}}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "p"}},
	}
	r.recordHuskDemand(claim)
	got, ok := d.LastArrival("ns/p")
	if !ok {
		t.Fatal("expected a recorded arrival for ns/p")
	}
	if !got.Equal(time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("arrival = %v, want the injected clock time", got)
	}
}
