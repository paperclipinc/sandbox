package controller

import (
	"sync"
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

// PoolDemand tracks the most recent claim-arrival time per pool, the demand
// signal the autoscaler uses to decide whether the scale-down cooldown has
// elapsed. It is process-local and best-effort: a controller restart loses the
// in-flight window and the pool simply treats the next reconcile as "no recent
// claim" (it will not scale down until a fresh cooldown elapses or LastArrival
// is repopulated by the next claim). It holds only timestamps, never any claim
// payload, so it carries no secret. Safe for concurrent use.
type PoolDemand struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// NewPoolDemand builds an empty demand tracker.
func NewPoolDemand() *PoolDemand {
	return &PoolDemand{last: make(map[string]time.Time)}
}

// RecordArrival stamps a claim arrival for the pool key (namespace/name) at t,
// advancing the recorded time only forward so an out-of-order reconcile cannot
// move the window backwards.
func (d *PoolDemand) RecordArrival(key string, t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if prev, ok := d.last[key]; ok && t.Before(prev) {
		return
	}
	d.last[key] = t
}

// LastArrival returns the most recent recorded claim-arrival time for the pool
// key and whether one was recorded.
func (d *PoolDemand) LastArrival(key string) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.last[key]
	return t, ok
}

// Forget drops the demand-tracker entry for the pool key. It is called when a
// pool is genuinely deleted (the reconcile saw NotFound) so the process-local
// map does not accumulate one entry per distinct pool name over the controller
// lifetime. Forgetting an unknown key is a no-op. Safe for concurrent use.
func (d *PoolDemand) Forget(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.last, key)
}
