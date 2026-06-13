package controller

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
