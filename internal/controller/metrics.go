package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Controller-level metrics. These register with controller-runtime's global
// Registry so they appear on the controller's own /metrics endpoint alongside
// the built-in workqueue and reconcile metrics. The node-level fork metrics
// (active sandboxes, fork duration) live in the daemon on the default
// prometheus registry; these are distinct, controller-scoped signals.
//
// No metric carries secret values: labels are pool names and coarse failure
// reasons only.
var (
	// claimPendingTotal counts how many times a claim was requeued because no
	// node had a ready snapshot (the claim stayed Pending). A counter of
	// pending-requeue EVENTS is used rather than a live gauge of currently
	// pending claims: a counter is exact and lock-free to bump at the requeue
	// site, while an honest live gauge would need a periodic recount of all
	// Pending claims (a separate scan with its own staleness window). The
	// counter answers "how often are claims failing to place" directly.
	claimPendingTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mitos_claim_pending_total",
		Help: "Number of times a claim was requeued for no node with a ready snapshot (claim stayed Pending).",
	})

	// orphanSweepsTotal counts forkd VMs reaped by the GC orphan sweep.
	orphanSweepsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mitos_orphan_sweeps_total",
		Help: "Number of orphan sandbox VMs terminated by the garbage collector.",
	})

	// claimErrorsTotal counts terminal claim failures, labeled by pool and a
	// coarse reason (fork, secret, volume, token). Reasons are fixed strings,
	// never error text, so no secret or path leaks into a label value.
	claimErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_claim_errors_total",
		Help: "Number of claims that failed terminally, by pool and reason.",
	}, []string{"pool", "reason"})

	// poolReadySnapshots is the per-pool count of ready snapshots, set each pool
	// reconcile. It mirrors SandboxPool.Status.ReadySnapshots as a scrapeable
	// gauge.
	poolReadySnapshots = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mitos_pool_ready_snapshots",
		Help: "Ready snapshots per pool, as of the last pool reconcile.",
	}, []string{"pool"})

	// poolWarmDormant is the per-pool count of DORMANT (unclaimed, warm) husk
	// pods as of the last pool reconcile: the live warm-buffer size.
	poolWarmDormant = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mitos_pool_warm_dormant",
		Help: "Dormant (unclaimed, warm) husk pods per pool, as of the last reconcile.",
	}, []string{"pool"})

	// poolWarmInUse is the per-pool count of claimed/active husk pods (pods
	// carrying mitos.run/claim): the demand the autoscaler sizes against.
	poolWarmInUse = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mitos_pool_warm_in_use",
		Help: "Claimed/active husk pods per pool, as of the last reconcile.",
	}, []string{"pool"})

	// poolDesiredWarm is the per-pool autoscaler target dormant count this
	// reconcile (mirrors SandboxPool.Status.DesiredWarm).
	poolDesiredWarm = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mitos_pool_desired_warm",
		Help: "Autoscaler desired dormant husk pod count per pool, as of the last reconcile.",
	}, []string{"pool"})

	// warmScaleUpTotal and warmScaleDownTotal count autoscaler scale events per
	// pool, so an operator can alert on thrash or a stuck pool.
	warmScaleUpTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_pool_warm_scale_up_total",
		Help: "Number of times the warm-pool autoscaler increased the dormant count, by pool.",
	}, []string{"pool"})
	warmScaleDownTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_pool_warm_scale_down_total",
		Help: "Number of times the warm-pool autoscaler decreased the dormant count, by pool.",
	}, []string{"pool"})

	// refillLatencySeconds measures wall-clock from a husk pod object Create to
	// the pod being counted Ready+dormant (a warm slot), the refill cost the
	// fast-refill follow-up reduces. Buckets span sub-second to the ~10-14 s cold
	// start.
	refillLatencySeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "mitos_pool_refill_latency_seconds",
		Help:    "Seconds from creating a husk pod to it becoming a ready dormant warm slot.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 12, 20, 30},
	})

	// claimWaitForWarmSeconds measures, per claim, the wall-clock the claim waited
	// for a ready dormant pod from its creation to a successful husk activate. A
	// burst absorbed by warm capacity shows up near the activate cost (~27 ms);
	// a claim that had to wait for a cold-started pod shows up in the seconds.
	claimWaitForWarmSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "mitos_claim_wait_for_warm_seconds",
		Help:    "Seconds a claim waited from creation to activating a warm husk pod.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 15},
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		claimPendingTotal,
		orphanSweepsTotal,
		claimErrorsTotal,
		poolReadySnapshots,
		poolWarmDormant,
		poolWarmInUse,
		poolDesiredWarm,
		warmScaleUpTotal,
		warmScaleDownTotal,
		refillLatencySeconds,
		claimWaitForWarmSeconds,
	)
}

// recordClaimPending bumps the pending-requeue counter.
func recordClaimPending() {
	claimPendingTotal.Inc()
}

// recordOrphanSweep bumps the orphan-sweep counter once per reaped VM.
func recordOrphanSweep() {
	orphanSweepsTotal.Inc()
}

// recordClaimError bumps the per-pool, per-reason claim-error counter. reason
// must be a fixed label (e.g. "fork", "secret", "volume", "token"), never error
// text.
func recordClaimError(pool, reason string) {
	claimErrorsTotal.WithLabelValues(pool, reason).Inc()
}

// setPoolReadySnapshots records the ready-snapshot count for a pool.
func setPoolReadySnapshots(pool string, ready int32) {
	poolReadySnapshots.WithLabelValues(pool).Set(float64(ready))
}

// setWarmPoolGauges records the warm-pool size, in-use, and desired counts for a
// pool in one call (pool is the namespace/name key, never a secret).
func setWarmPoolGauges(pool string, dormant, inUse, desired int32) {
	poolWarmDormant.WithLabelValues(pool).Set(float64(dormant))
	poolWarmInUse.WithLabelValues(pool).Set(float64(inUse))
	poolDesiredWarm.WithLabelValues(pool).Set(float64(desired))
}

// recordWarmScaleUp / recordWarmScaleDown bump the per-pool scale-event counters.
func recordWarmScaleUp(pool string)   { warmScaleUpTotal.WithLabelValues(pool).Inc() }
func recordWarmScaleDown(pool string) { warmScaleDownTotal.WithLabelValues(pool).Inc() }

// forgetPoolMetrics drops every per-pool warm-pool label series for the given
// pool key. It is called only when a SandboxPool is genuinely deleted (the
// reconcile saw NotFound), so the controller does not accumulate one stale
// label series per distinct pool name over its lifetime. The gauges and
// scale-event counters carry a single "pool" label, so DeleteLabelValues clears
// them; the call is a no-op for a pool that was never recorded. Per-pool claim
// error series (pool, reason) are intentionally NOT cleared here: they are a
// terminal failure record an operator may still want to read after the pool is
// gone, and reason is not enumerable from this site.
func forgetPoolMetrics(pool string) {
	poolReadySnapshots.DeleteLabelValues(pool)
	poolWarmDormant.DeleteLabelValues(pool)
	poolWarmInUse.DeleteLabelValues(pool)
	poolDesiredWarm.DeleteLabelValues(pool)
	warmScaleUpTotal.DeleteLabelValues(pool)
	warmScaleDownTotal.DeleteLabelValues(pool)
}

// observeRefillLatency records seconds from husk pod create to ready dormant.
func observeRefillLatency(seconds float64) { refillLatencySeconds.Observe(seconds) }

// observeClaimWaitForWarm records seconds a claim waited from creation to
// activating a warm husk pod.
func observeClaimWaitForWarm(seconds float64) { claimWaitForWarmSeconds.Observe(seconds) }
