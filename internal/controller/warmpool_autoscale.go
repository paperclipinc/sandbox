package controller

import (
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultScaleDownCooldown is used when ScaleDownCooldownSeconds is 0 (unset).
const defaultScaleDownCooldown = 300 * time.Second

// computeDesiredWarm returns the desired DORMANT husk pod count for a pool and
// whether the decision is a scale-DOWN from the current dormant level.
//
// When pool.Spec.Autoscale is nil the warm pool keeps the legacy fixed behavior:
// desired == Replicas, and a reduction from a too-large dormant set is reported
// as a scale-down so the metric/status are honest.
//
// When autoscaling is enabled the target is:
//
//	target = clamp(inUse + targetSpare, effectiveMin, maxWarm)
//
// where effectiveMin = min(minWarm, maxWarm) so a misconfigured MinWarm above
// MaxWarm cannot force the pool above its ceiling. Scale UP (target > dormant)
// always applies immediately. Scale DOWN (target < dormant) only applies once
// the cooldown has elapsed since the last claim arrival; inside the cooldown the
// pool HOLDS its current dormant level (hysteresis) to avoid thrash. The
// returned bool is true only when the applied desired is strictly below the
// current dormant count.
func computeDesiredWarm(pool *v1alpha1.SandboxPool, dormant, inUse int32, lastClaim *metav1.Time, now time.Time) (int32, bool) {
	as := pool.Spec.Autoscale
	if as == nil {
		desired := pool.Spec.Replicas
		return desired, desired < dormant
	}

	maxWarm := as.MaxWarm
	if maxWarm < 0 {
		maxWarm = 0
	}
	effectiveMin := as.MinWarm
	if effectiveMin > maxWarm {
		effectiveMin = maxWarm
	}

	target := inUse + as.TargetSpare
	if target < effectiveMin {
		target = effectiveMin
	}
	if target > maxWarm {
		target = maxWarm
	}

	if target >= dormant {
		// Scale up or hold: always apply immediately.
		return target, false
	}

	// Scale down requested: only apply after the cooldown has elapsed since the
	// last claim arrival. Inside the cooldown, hold the current dormant level.
	cooldown := defaultScaleDownCooldown
	if as.ScaleDownCooldownSeconds > 0 {
		cooldown = time.Duration(as.ScaleDownCooldownSeconds) * time.Second
	}
	var last time.Time
	if lastClaim != nil {
		last = lastClaim.Time
	}
	if last.IsZero() || now.Sub(last) >= cooldown {
		return target, true
	}
	return dormant, false
}
