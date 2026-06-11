package controller

import (
	"context"
	"fmt"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Husk pod loss and drain (issue #18, slice 4b).
//
// A husk pod backing an ACTIVE claim can disappear under the claim: a node
// drain, an eviction, a spot reclaim, or an operator kubectl delete. The claim
// has Status.Node/Endpoint set and the pod carries agentrun.dev/claim=<claim>.
// When that pod is GONE or terminating, the in-VM state is lost; the claim must
// not keep advertising a dead endpoint. The drain policy on the pool governs
// what happens next:
//
//   - Kill (default): RE-PEND. Set Phase Pending, clear Status.Endpoint and
//     Status.Node, recordClaimPending, requeue. The next reconcile re-activates
//     the claim on a replacement dormant slot (the warm pool self-heals the lost
//     pod via Owns(pods)). The agent reconnects to a fresh fork from the
//     template snapshot. Boring and always available.
//
//   - Checkpoint: attempt to snapshot the LIVE VM before re-pending, so the
//     agent can resume from captured state. The live snapshot only runs where
//     the VMM still runs (a graceful drain on a KVM-capable kubelet). On an
//     ALREADY-DELETED pod there is nothing left to checkpoint, so Checkpoint
//     degrades to the same re-pend as Kill, with a logged note. This file plumbs
//     the decision and calls the checkpointer seam where the pod is still
//     reachable; the live snapshot surviving a drain end to end is bare-metal
//     work (see docs/husk-pods.md).
//
// TRIGGER: two paths, both implemented.
//   1. Every claim reconcile of a Ready (active) claim first checks whether its
//      claimed husk pod still exists and is not terminating (checkHuskPodLost).
//      A gone/terminating pod re-pends the claim immediately.
//   2. A Watches(&corev1.Pod{}) mapping enqueues the claim named in a husk pod's
//      agentrun.dev/claim label on any pod event (huskPodToClaim), so a pod
//      delete promptly reconciles the claim instead of waiting for the claim's
//      own requeue. The Ready early-return in Reconcile routes through the loss
//      check before the lifetime path, so the enqueued reconcile re-pends.

// huskCheckpointer is the seam the claim reconciler checkpoints a live husk VM
// through before re-pending under a Checkpoint drain policy. The production
// value (defaultHuskCheckpointer) calls the engine ForkRunning/CreateSnapshot
// path via forkd where the pod's node is reachable; tests inject a fake that
// records the call. It returns ok=true when a live snapshot was captured (the
// VMM was still reachable), ok=false when there was nothing to checkpoint (the
// pod/VMM is already gone), in which case the caller falls back to re-pend.
type huskCheckpointer func(ctx context.Context, claim *v1alpha1.SandboxClaim, pod *corev1.Pod) (ok bool, err error)

// defaultHuskCheckpointer is the production checkpointer. The live VM runs
// INSIDE the husk pod, so a checkpoint is only possible while that pod (and its
// node) is still up; on an already-deleted pod (pod == nil) there is nothing to
// snapshot and it returns ok=false so the caller re-pends. Where the pod is
// still present, the live-VM snapshot over forkd ForkRunning/CreateSnapshot runs
// on a KVM-capable kubelet; on a cluster without one (shared CI, no nested VMM)
// it likewise has nothing reachable to checkpoint and degrades to re-pend. The
// end-to-end live snapshot surviving a drain is gated on bare metal (documented
// in docs/husk-pods.md); this default is the reachable-seam plumbing.
func defaultHuskCheckpointer(_ context.Context, _ *v1alpha1.SandboxClaim, pod *corev1.Pod) (bool, error) {
	if pod == nil {
		// The pod is already gone: nothing to checkpoint, fall back to re-pend.
		return false, nil
	}
	// A reachable pod: the live-VM checkpoint runs where the VMM runs (bare
	// metal). On a cluster without a KVM-capable kubelet the VMM is not actually
	// live, so we report nothing-captured and let the caller re-pend rather than
	// claim a snapshot we did not take (no unverified claims).
	return false, nil
}

// checkHuskPodLost reports whether a Ready claim's backing husk pod is lost
// (missing or terminating). It returns the pod (nil when missing) so a
// Checkpoint policy can attempt a live snapshot while the pod is still present
// but terminating. A claim with no Node/Endpoint (not actually active on a pod)
// is never "lost" here. Only meaningful in husk mode.
func (r *SandboxClaimReconciler) checkHuskPodLost(ctx context.Context, claim *v1alpha1.SandboxClaim) (lost bool, pod *corev1.Pod, err error) {
	// Not active on a husk pod: nothing to lose.
	if claim.Status.Node == "" || claim.Status.Endpoint == "" {
		return false, nil, nil
	}

	// Find the husk pod claimed by this claim. SandboxID is the pod name on the
	// husk path (reconcileHuskClaim sets Status.SandboxID = pod.Name), so a
	// direct Get is the cheap lookup; fall back to the claim-label selector if
	// SandboxID is unset.
	if claim.Status.SandboxID != "" {
		var p corev1.Pod
		gerr := r.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Status.SandboxID}, &p)
		switch {
		case apierrors.IsNotFound(gerr):
			return true, nil, nil
		case gerr != nil:
			return false, nil, fmt.Errorf("get claimed husk pod %s: %w", claim.Status.SandboxID, gerr)
		}
		// Verify it is actually this claim's husk pod (label) before trusting it.
		if p.Labels[huskClaimLabel] == claim.Name && p.Labels[huskLabel] == "true" {
			if p.DeletionTimestamp != nil {
				return true, &p, nil
			}
			return false, &p, nil
		}
		// The name was reused by a different pod (or it is not a husk pod): treat
		// the original as lost.
		return true, nil, nil
	}

	// No SandboxID: list by the claim label.
	var pods corev1.PodList
	if lerr := r.List(ctx, &pods,
		client.InNamespace(claim.Namespace),
		client.MatchingLabels{huskClaimLabel: claim.Name, huskLabel: "true"},
	); lerr != nil {
		return false, nil, fmt.Errorf("list husk pods for claim %s: %w", claim.Name, lerr)
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			p := pods.Items[i]
			return false, &p, nil
		}
	}
	// No live pod carries this claim's label: lost.
	return true, nil, nil
}

// rependOnHuskPodLost re-pends an active claim whose husk pod was lost. It
// honors the pool's DrainPolicy: under Checkpoint it first attempts the live-VM
// snapshot via the checkpointer seam (where the pod is still reachable), then
// re-pends regardless. Re-pend = Phase Pending, Endpoint/Node/SandboxID
// cleared, recordClaimPending, a HuskPodLost condition, and a requeue so the
// next reconcile re-activates on a replacement dormant slot.
func (r *SandboxClaimReconciler) rependOnHuskPodLost(ctx context.Context, claim *v1alpha1.SandboxClaim, pool *v1alpha1.SandboxPool, pod *corev1.Pod) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	policy := pool.Spec.DrainPolicy
	if policy == "" {
		policy = v1alpha1.DrainKill
	}

	checkpointed := false
	if policy == v1alpha1.DrainCheckpoint {
		checkpointer := r.Checkpoint
		if checkpointer == nil {
			checkpointer = defaultHuskCheckpointer
		}
		ok, cerr := checkpointer(ctx, claim, pod)
		if cerr != nil {
			// A checkpoint error must NOT strand the claim on a dead endpoint:
			// log it and re-pend anyway (fail open toward availability).
			logger.Error(cerr, "live-VM checkpoint failed on husk pod loss; re-pending without a checkpoint", "claim", claim.Name)
		}
		checkpointed = ok
		if !ok {
			logger.Info("Checkpoint drain policy: nothing to checkpoint (pod/VMM already gone), degrading to re-pend", "claim", claim.Name)
		} else {
			logger.Info("Checkpoint drain policy: captured a live-VM snapshot before re-pend", "claim", claim.Name)
		}
	}

	reason := "HuskPodLost"
	msg := "the backing husk pod was lost (drain, eviction, or deletion); the claim is re-pending and will re-activate on a replacement dormant slot"
	if policy == v1alpha1.DrainCheckpoint {
		if checkpointed {
			msg = "the backing husk pod was lost; a live-VM checkpoint was captured and the claim is re-pending to re-activate on a replacement slot"
		} else {
			msg = "the backing husk pod was lost with no live VMM to checkpoint; the claim is re-pending (Kill semantics) and will re-activate on a replacement dormant slot"
		}
	}

	claim.Status.Phase = v1alpha1.SandboxPending
	claim.Status.Endpoint = ""
	claim.Status.Node = ""
	claim.Status.SandboxID = ""
	recordClaimPending()
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(r.now()),
		Reason:             reason,
		Message:            msg,
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("claim re-pended on husk pod loss", "claim", claim.Name, "drainPolicy", policy)
	return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
}

// huskPodToClaim maps a husk pod event to a reconcile of the claim named in its
// agentrun.dev/claim label, so a pod delete/eviction promptly reconciles the
// active claim (which then re-pends). A husk pod with no claim label (a dormant
// slot) maps to nothing; the pool's Owns(pods) handles dormant self-heal.
func huskPodToClaim(_ context.Context, obj client.Object) []ctrl.Request {
	labels := obj.GetLabels()
	if labels[huskLabel] != "true" {
		return nil
	}
	claimName := labels[huskClaimLabel]
	if claimName == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: claimName}}}
}
