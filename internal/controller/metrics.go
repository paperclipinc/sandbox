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
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		claimPendingTotal,
		orphanSweepsTotal,
		claimErrorsTotal,
		poolReadySnapshots,
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
