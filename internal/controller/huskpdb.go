package controller

import (
	"context"
	"fmt"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Husk pod eviction bound (issue #18, slice 4b).
//
// A node drain (kubectl drain, a cluster-autoscaler scale-down, a spot
// reclaim) evicts the pods on that node. Without a PodDisruptionBudget the
// drain may evict EVERY husk pod of a pool at once, collapsing the warm pool to
// zero and stalling every claim that wants a dormant slot until the pool
// reconcile rebuilds it. A PodDisruptionBudget bounds VOLUNTARY disruption: the
// eviction API refuses to evict a husk pod if doing so would drop the pool
// below minAvailable, so a drain proceeds at a bounded rate and the warm pool
// stays usable while it drains one slot at a time.
//
// BUDGET CHOICE (documented): minAvailable = max(1, Replicas-1). A drain can
// disrupt AT MOST ONE husk pod at a time for a pool of two or more, and for a
// single-replica pool minAvailable=1 means the lone husk pod is not voluntarily
// evicted at all (the drain must --force or wait, the operator's explicit
// choice). This trades drain speed for warm-pool availability, which is the
// right default for a latency-sensitive warm pool: a claim should almost always
// find a dormant slot even mid-drain. A maxUnavailable=1 budget would express
// "at most one out at a time" more directly, but minAvailable composes better
// with a pool scaled to 1 (it pins the floor at the lone slot). The bound is
// VOLUNTARY-disruption only: a node hard-crash still takes its husk pods, and
// the Owns(pods) self-heal recreates them.

// huskPDBName is the PodDisruptionBudget name for a pool's husk pods.
func huskPDBName(poolName string) string {
	return poolName + "-husk"
}

// huskPDBMinAvailable returns the documented minAvailable for a pool: a drain
// disrupts at most one husk pod at a time (Replicas-1), with a floor of 1 so a
// single-replica pool keeps its lone warm slot.
func huskPDBMinAvailable(replicas int32) int32 {
	if replicas-1 < 1 {
		return 1
	}
	return replicas - 1
}

// ensureHuskPDB creates or updates the pool's husk PodDisruptionBudget so a
// node drain disrupts at most a bounded number of warm husk pods at once. The
// PDB selects the pool's husk pods (mitos.run/pool=<name>,mitos.run/husk
// =true) and is owner-referenced to the pool so Kubernetes garbage collection
// deletes it when the pool is deleted. Idempotent: a second call updates the
// minAvailable in place if Replicas changed.
func (r *SandboxPoolReconciler) ensureHuskPDB(ctx context.Context, pool *v1alpha1.SandboxPool) error {
	minAvailable := intstr.FromInt32(huskPDBMinAvailable(pool.Spec.Replicas))

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      huskPDBName(pool.Name),
			Namespace: pool.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec.MinAvailable = &minAvailable
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				huskPoolLabel: pool.Name,
				huskLabel:     "true",
			},
		}
		// Owner-ref to the pool so the PDB is garbage-collected with the pool.
		// CreateOrUpdate may run on an existing object with an owner already set;
		// SetControllerReference is idempotent for the same owner.
		if existing := metav1.GetControllerOf(pdb); existing == nil {
			if serr := controllerutil.SetControllerReference(pool, pdb, r.Scheme()); serr != nil {
				return fmt.Errorf("set owner on husk PDB %s: %w", pdb.Name, serr)
			}
		}
		return nil
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure husk PDB for pool %s: %w", pool.Name, err)
	}
	return nil
}
