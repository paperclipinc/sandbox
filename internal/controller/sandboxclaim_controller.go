package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/husk"
	"github.com/paperclipinc/sandbox/internal/observability"
	"github.com/paperclipinc/sandbox/internal/vsock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// tracer is the controller component tracer; no-op unless tracing is configured.
var tracer = observability.Tracer("agentrun-controller")

// DefaultMaxPendingDuration bounds how long a claim may stay Pending for lack of
// node capacity before the reconciler gives up and fails it with an actionable
// capacity-exhaustion message. Override per-deployment with --max-pending-duration.
const DefaultMaxPendingDuration = 5 * time.Minute

// pendingSinceAnnotation stamps, in RFC3339, the instant a claim first went
// Pending for lack of capacity. It is the durable source of truth for the
// bounded-wait deadline: a status condition's LastTransitionTime would reset on
// any unrelated condition churn, whereas this annotation only changes when the
// claim enters or leaves the capacity-pending state. Cleared on successful
// placement so a later capacity shortage starts a fresh clock.
const pendingSinceAnnotation = "agentrun.dev/capacity-pending-since"

// capacityPendingRequeue is the backoff between capacity-pending retries: long
// enough not to hot-loop a full cluster, short enough to place a claim promptly
// once a node frees up or a new node joins.
const capacityPendingRequeue = 5 * time.Second

// huskActivator is the seam the claim reconciler dials a husk stub through. The
// production value is ActivateHuskPod (huskclient.go); tests inject a fake to
// record requests without a real mTLS server.
type huskActivator func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)

type SandboxClaimReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry

	// MaxPendingDuration bounds the capacity-pending wait; zero falls back to
	// DefaultMaxPendingDuration. Set from the --max-pending-duration flag.
	MaxPendingDuration time.Duration

	// Now is the reconciler's clock, injectable for tests. Nil uses time.Now.
	Now func() time.Time

	// EnableHuskPods selects the husk-pod activation path (issue #18, slice 2):
	// the claim activates a dormant warm husk pod in place over the mTLS control
	// channel instead of SelectNode+forkOnNode. Default false: the raw-forkd path
	// is unchanged.
	EnableHuskPods bool

	// HuskTLS is the controller client mTLS config used to dial a husk stub's
	// network control (the SAME config that dials forkd, EnsurePKI's controller
	// leaf). Required when EnableHuskPods is true; a nil config makes
	// ActivateHuskPod refuse to send secrets.
	HuskTLS *tls.Config

	// HuskControlPort is the TCP port the husk stub serves the mTLS control on.
	// Zero defaults to HuskControlPort. Only used when EnableHuskPods is true.
	HuskControlPort int

	// HuskSandboxPort is the in-pod port the activated VM's sandbox HTTP API is
	// reachable on; the claim's Status.Endpoint is podIP:this. Zero defaults to
	// the husk sandbox port (9091). Only used when EnableHuskPods is true.
	HuskSandboxPort int

	// Activate is the husk-activation seam. Nil defaults to ActivateHuskPod.
	// Tests inject a fake.
	Activate huskActivator

	// Checkpoint is the live-VM checkpoint seam used under a Checkpoint
	// DrainPolicy when an active claim's husk pod is lost. Nil defaults to
	// defaultHuskCheckpointer. Tests inject a fake to record the call. Only used
	// when EnableHuskPods is true.
	Checkpoint huskCheckpointer

	// HydrateWorkspace and DehydrateWorkspace are the workspace-binding transfer
	// seams (W4 slice 2). A claim with spec.workspaceRef hydrates its workspace
	// head into the sandbox on activate and dehydrates the sandbox /workspace into
	// a new committed WorkspaceRevision on terminate. Nil defaults to the real
	// node-side transport path; envtest injects fakes that record the manifest /
	// return a scripted digest without a VM.
	HydrateWorkspace   hydrateFunc
	DehydrateWorkspace dehydrateFunc

	// DiffWorkspace computes a new revision's content-hash diff against the
	// workspace head before it, for a terminate {diff: true} output. Nil defaults
	// to the real store-backed path; envtest injects a fake.
	DiffWorkspace diffFunc

	// RendezvousGit pushes the workspace repo paths to a git rendezvous remote on
	// a per-attempt branch, for a terminate {git} output. Nil defaults to the real
	// path (workspace.Rendezvous via the git CLI); envtest and unit tests inject a
	// fake.
	RendezvousGit rendezvousFunc

	// RepoFilesForGit resolves the workspace spec.git.paths content from a
	// dehydrated revision manifest for a {git} output. Nil defaults to the real
	// store-backed path; envtest injects a fake.
	RepoFilesForGit repoFilesFunc

	// eventFilter optionally restricts which claims this reconciler watches. Nil
	// watches all claims (the production default: a deployment runs exactly one
	// claim reconciler, husk or raw). It exists so a test harness can run a raw
	// and a husk reconciler on the same manager without the two fighting over the
	// same object.
	eventFilter predicate.Predicate

	// controllerName overrides the controller-runtime controller name. Empty uses
	// the kind-derived default. Only set by the test harness so two claim
	// reconcilers can coexist on one manager.
	controllerName string
}

// now returns the reconciler's current time, honoring the injectable clock.
func (r *SandboxClaimReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// maxPendingDuration returns the configured bound or the default.
func (r *SandboxClaimReconciler) maxPendingDuration() time.Duration {
	if r.MaxPendingDuration > 0 {
		return r.MaxPendingDuration
	}
	return DefaultMaxPendingDuration
}

// SandboxClaim ownership: get/list/watch to reconcile, update to write the
// terminate finalizer, delete for the garbage collector's TTL sweep of
// finished claims. status writes phase, conditions, and FinishedAt.
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxclaims/finalizers,verbs=update
// SandboxTemplate and SandboxPool are read-only inputs to claim placement.
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentrun.dev,resources=sandboxpools,verbs=get;list;watch
// Secrets: get/list to read mounted secrets referenced by a sandbox and to
// reconcile the per-sandbox token Secret; create/update to mint and heal that
// token Secret (and the controller's PKI Secrets, see EnsurePKI); delete to
// crypto-shred a template's at-rest encryption key Secret on teardown
// (DeleteEncKey).
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// controller.reconcileClaim spans the whole reconcile. Only claim
	// name/namespace and the pool name (config, no secrets) are attributes; the
	// fork RPC below is a child span carrying the trace to forkd over gRPC.
	ctx, span := tracer.Start(ctx, "controller.reconcileClaim", trace.WithAttributes(
		attribute.String("claim.name", req.Name),
		attribute.String("claim.namespace", req.Namespace),
	))
	defer span.End()

	var claim v1alpha1.SandboxClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	span.SetAttributes(attribute.String("pool", claim.Spec.PoolRef.Name))

	// A claim under deletion: reap its backing VM via the finalizer before the
	// API object is allowed to disappear.
	if !claim.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &claim)
	}

	// Already assigned. In husk mode, FIRST check whether the backing husk pod
	// was lost (a node drain, an eviction, a deletion): a Ready claim must not
	// keep advertising a dead endpoint. A lost pod re-pends the claim per the
	// pool's DrainPolicy (Kill re-pends; Checkpoint snapshots the live VM first
	// where reachable, then re-pends). This runs before the lifetime path so an
	// enqueued pod-delete event promptly re-pends. When the pod is still healthy,
	// fall through to the normal lifetime reaping.
	if claim.Status.Phase == v1alpha1.SandboxReady {
		if r.EnableHuskPods {
			lost, lostPod, err := r.checkHuskPodLost(ctx, &claim)
			if err != nil {
				return ctrl.Result{}, err
			}
			if lost {
				var pool v1alpha1.SandboxPool
				if perr := r.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.PoolRef.Name}, &pool); perr != nil {
					// The pool is gone too: re-pend with Kill semantics (an empty
					// DrainPolicy defaults to Kill in rependOnHuskPodLost).
					if client.IgnoreNotFound(perr) != nil {
						return ctrl.Result{}, perr
					}
				}
				return r.rependOnHuskPodLost(ctx, &claim, &pool, lostPod)
			}
		}
		// A Ready claim bound to a workspace hydrates its head into the sandbox
		// exactly once (idempotent via the hydrated annotation). A transient
		// transfer error requeues without failing the Ready claim; the sandbox
		// stays usable (an unpopulated workspace) until the next attempt succeeds.
		if err := r.hydrateOnActivate(ctx, &claim); err != nil {
			logger.Error(err, "hydrate workspace into sandbox; will retry", "claim", claim.Name)
			return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
		}
		return r.reconcileLifetime(ctx, &claim)
	}

	// Terminal phases: don't retry.
	if claim.Status.Phase == v1alpha1.SandboxFailed || claim.Status.Phase == v1alpha1.SandboxTerminated {
		return ctrl.Result{}, nil
	}

	// Find the pool
	var pool v1alpha1.SandboxPool
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: claim.Namespace,
		Name:      claim.Spec.PoolRef.Name,
	}, &pool); err != nil {
		logger.Error(err, "pool not found", "pool", claim.Spec.PoolRef.Name)
		return ctrl.Result{}, err
	}

	// Find the template
	var template v1alpha1.SandboxTemplate
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: pool.Namespace,
		Name:      pool.Spec.TemplateRef.Name,
	}, &template); err != nil {
		return ctrl.Result{}, err
	}

	// Single-writer-per-workspace: a claim bound to a workspace that is already
	// bound to another active claim must not acquire a VM. It pends with a clear
	// WorkspaceBusy reason and retries, so the second writer waits for the first
	// to release rather than two sandboxes racing to dehydrate the same workspace.
	if claim.Spec.WorkspaceRef != nil {
		busy, err := r.workspaceBusyClaim(ctx, &claim)
		if err != nil {
			return ctrl.Result{}, err
		}
		if busy != "" {
			claim.Status.Phase = v1alpha1.SandboxPending
			setCondition(&claim.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(r.now()),
				Reason:             "WorkspaceBusy",
				Message: fmt.Sprintf(
					"workspace %q is already bound to active claim %q; this claim will bind once that claim releases the workspace (single-writer-per-workspace)",
					claim.Spec.WorkspaceRef.Name, busy,
				),
			})
			// Best-effort status write; the return requeues regardless.
			_ = r.Status().Update(ctx, &claim)
			return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
		}
	}

	// Add the terminate finalizer before the claim acquires a backing VM, so
	// no Ready claim can ever be deleted without forkd reaping its sandbox.
	// This is a metadata Update, distinct from the status writes below.
	if controllerutil.AddFinalizer(&claim, FinalizerTerminate) {
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Mark as restoring
	claim.Status.Phase = v1alpha1.SandboxRestoring
	if err := r.Status().Update(ctx, &claim); err != nil {
		return ctrl.Result{}, err
	}

	// Husk-pod activation path (issue #18, slice 2). When enabled, the claim
	// activates a dormant warm husk pod in place over the mTLS control channel
	// instead of SelectNode+forkOnNode. The default (flag off) leaves the
	// raw-forkd path below unchanged.
	if r.EnableHuskPods {
		return r.reconcileHuskClaim(ctx, &claim, &pool, &template)
	}

	// Pick a node with a ready snapshot
	node, snapshotID, err := r.selectNode(ctx, &pool, claim.Spec.NodeName)
	if err != nil {
		// No node admits the fork under the overcommit policy: this is a real
		// capacity shortage, not a missing snapshot. Pend with backpressure and
		// fail cleanly after a bounded wait rather than hammering a full cluster
		// forever (or, worse, forcing a placement that OOMs a node).
		if errors.Is(err, ErrNoCapacity) {
			return r.reconcileNoCapacity(ctx, &claim, err)
		}
		// No registered/healthy node, or no node holds the snapshot yet: a
		// transient placement precondition the pool reconciler is expected to
		// resolve. Pend and retry indefinitely (no bounded fail).
		logger.Info("no node available for placement, pending", "error", err.Error())
		r.clearPendingSince(&claim)
		claim.Status.Phase = v1alpha1.SandboxPending
		recordClaimPending()
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Placement succeeded: clear any capacity-pending stamp so a later shortage
	// starts a fresh bounded-wait clock.
	if claim.Annotations[pendingSinceAnnotation] != "" {
		r.clearPendingSince(&claim)
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Translate the template's volumes (with this claim's VolumeOverrides
	// applied) into the Fork RPC's VolumeMounts. The node prepares and attaches
	// the backing drives per policy; the controller only forwards the spec.
	volumes := volumeMounts(template.Spec.Volumes, claim.Spec.VolumeOverrides)

	// Resolve secrets
	env, secretVals, err := r.resolveSecrets(ctx, claim.Namespace, claim.Spec.Env, claim.Spec.Secrets)
	if err != nil {
		logger.Error(err, "secret resolution failed")
		recordClaimError(claim.Spec.PoolRef.Name, "secret")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// Mint the sandbox API bearer token before forking; forkd registers it
	// at fork time. The value reaches exactly two places: the ForkRequest
	// and the owned token Secret below. Never status, conditions, events,
	// or logs.
	apiToken, err := mintAPIToken()
	if err != nil {
		logger.Error(err, "token minting failed")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// When the source template is encrypted, read its at-rest key from the
	// controller-owned Secret (idempotent read; created by the pool reconciler at
	// build time) and deliver it so the node can open the encrypted container
	// before restoring. The controller holds the key transiently; it is never
	// logged. snapshotID equals the template id, so it names the key Secret.
	var encKey []byte
	if template.Spec.Encrypted {
		encKey, err = EnsureEncKey(ctx, r.Client, claim.Namespace, snapshotID, &template)
		if err != nil {
			logger.Error(err, "read encryption key for template", "template", snapshotID)
			now := metav1.Now()
			claim.Status.Phase = v1alpha1.SandboxFailed
			claim.Status.FinishedAt = &now
			_ = r.Status().Update(ctx, &claim)
			return ctrl.Result{}, err
		}
	}

	// Call forkd on the selected node: this is the <2ms hot path
	result, err := r.forkOnNode(ctx, node, snapshotID, claim.Name, env, secretVals, template.Spec.Network, volumes, apiToken, encKey)
	if err != nil {
		// A NotFound from forkd usually means the snapshot is not built on
		// that node yet; transient while the pool reconciler catches up.
		if isNotFound(err) {
			logger.Info("snapshot not yet on node, retrying", "node", node.Name, "error", err.Error())
			claim.Status.Phase = v1alpha1.SandboxPending
			// Best-effort status write; the return below already requeues or surfaces the error.
			_ = r.Status().Update(ctx, &claim)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		logger.Error(err, "fork failed", "node", node.Name)
		recordClaimError(claim.Spec.PoolRef.Name, "fork")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// Hand the token to the claim's consumer via an owned Secret, BEFORE
	// the Ready status write: a Ready claim whose token Secret does not
	// exist would be unusable, and the Ready early-return above would never
	// retry the Secret. The token exists only in this Secret.
	if err := ensureSandboxTokenSecret(ctx, r.Client, &claim, claim.Name+tokenSecretSuffix, apiToken, result.Endpoint); err != nil {
		logger.Error(err, "token secret write failed")
		recordClaimError(claim.Spec.PoolRef.Name, "token")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, &claim)
		return ctrl.Result{}, err
	}

	// Update status
	now := metav1.Now()
	claim.Status.Phase = v1alpha1.SandboxReady
	claim.Status.Endpoint = result.Endpoint
	claim.Status.Node = node.Name
	claim.Status.SandboxID = result.SandboxID
	claim.Status.ForkTimeMicros = int64(result.ForkTimeMs * 1000)
	claim.Status.StartedAt = &now
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "Forked",
		Message:            fmt.Sprintf("forked in %.2fms on node %s", result.ForkTimeMs, node.Name),
	})

	if err := r.Status().Update(ctx, &claim); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("sandbox claimed",
		"sandbox", claim.Name,
		"node", node.Name,
		"forkTime", fmt.Sprintf("%.2fms", result.ForkTimeMs),
	)

	return ctrl.Result{}, nil
}

// reconcileHuskClaim activates a dormant warm husk pod for the claim in place
// over the mTLS control channel. It selects a Running+Ready, unclaimed husk pod
// for the pool; if none is available it pends the claim (recordClaimPending) and
// requeues, mirroring the no-node placement-precondition path. Otherwise it
// resolves env+secrets (the same resolveSecrets as the forkd path), builds an
// ActivateRequest, dials the pod's control port, and on success sets the claim's
// Endpoint (podIP:sandboxPort) and Node (pod.Spec.NodeName), marks the pod
// claimed, mints + writes the per-sandbox API token Secret, and goes Ready.
//
// FAILS CLOSED: an activate transport error or a not-OK result NEVER goes Ready;
// it pends with backpressure and an actionable message so a transient husk
// (snapshot not yet materialized, stub still starting) can recover. Secret
// VALUES are never logged or put in status/conditions.
func (r *SandboxClaimReconciler) reconcileHuskClaim(ctx context.Context, claim *v1alpha1.SandboxClaim, pool *v1alpha1.SandboxPool, template *v1alpha1.SandboxTemplate) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pod, err := r.selectDormantHuskPod(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil {
		// No warm husk slot: pend and retry. The pool reconciler is expected to
		// scale the warm pool up; this is a transient placement precondition.
		logger.Info("no dormant husk pod available, pending", "pool", pool.Name)
		claim.Status.Phase = v1alpha1.SandboxPending
		recordClaimPending()
		setCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(r.now()),
			Reason:             "NoHuskPod",
			Message:            "no warm husk pod is ready in the pool; the claim will retry once the pool scales a dormant slot up",
		})
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}

	// Resolve env + secrets (same path as the forkd fork). Secret VALUES live
	// only in memory here and ride the mTLS control channel; never logged.
	env, secretVals, err := r.resolveSecrets(ctx, claim.Namespace, claim.Spec.Env, claim.Spec.Secrets)
	if err != nil {
		logger.Error(err, "secret resolution failed")
		recordClaimError(claim.Spec.PoolRef.Name, "secret")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		claim.Status.FinishedAt = &now
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	// Mint the per-sandbox API bearer token before activating. It reaches exactly
	// the owned token Secret below; never status, conditions, events, or logs.
	apiToken, err := mintAPIToken()
	if err != nil {
		logger.Error(err, "token minting failed")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		claim.Status.FinishedAt = &now
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	controlPort := r.HuskControlPort
	if controlPort == 0 {
		controlPort = HuskControlPort
	}
	sandboxPort := r.HuskSandboxPort
	if sandboxPort == 0 {
		sandboxPort = huskSandboxPort
	}

	activate := r.Activate
	if activate == nil {
		activate = ActivateHuskPod
	}

	// Claim the dormant pod BEFORE activating it: stamp the agentrun.dev/claim
	// label under an OPTIMISTIC LOCK. This is the mutual-exclusion commit. Two
	// concurrent claims may both select the same dormant pod, but the
	// resourceVersion-guarded patch lets exactly one win; the loser gets a 409
	// Conflict and must NOT activate this pod (a second tenant on the same VM).
	// Winning the label patch is the gate to Activate, so a pod is activated by
	// exactly one claim. On conflict we requeue so the next reconcile picks a
	// different dormant pod.
	if err := r.markHuskPodClaimed(ctx, pod, claim.Name); err != nil {
		if apierrors.IsConflict(err) {
			logger.Info("husk pod claimed concurrently, requeueing to pick another", "pod", pod.Name)
			claim.Status.Phase = v1alpha1.SandboxPending
			setCondition(&claim.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(r.now()),
				Reason:             "HuskPodRaced",
				Message:            "the selected dormant husk pod was claimed by another claim concurrently; the claim will retry and pick a different dormant pod",
			})
			_ = r.Status().Update(ctx, claim)
			return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
		}
		logger.Error(err, "mark husk pod claimed failed", "pod", pod.Name)
		return ctrl.Result{}, err
	}

	// The recorded snapshot manifest digest the husk stub re-verifies the on-disk
	// snapshot against before loading it (the husk mirror of forkd's verify-on-load
	// gate). forkd reported it via GetCapacity; the NodeRegistry holds it. It is a
	// content address, not a secret. An empty digest (no node has reported it yet)
	// makes the stub refuse to activate unless it runs with the development escape
	// hatch, which is exactly the fail-closed behavior we want in production.
	expectedDigest := ""
	if r.NodeRegistry != nil {
		if d, ok := r.NodeRegistry.TemplateDigest(pool.Spec.TemplateRef.Name); ok {
			expectedDigest = d
		}
	}

	addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(controlPort))
	req := husk.ActivateRequest{
		SnapshotDir:    HuskSnapshotDir,
		ExpectedDigest: expectedDigest,
		Env:            env,
		Secrets:        secretVals,
		Network:        huskNotifyNetwork(template),
		Token:          apiToken,
	}
	res, err := activate(ctx, addr, r.HuskTLS, req)
	if err != nil || !res.OK {
		// FAIL CLOSED: do not go Ready. Pend so a transient husk can recover.
		msg := "husk activation did not complete"
		if err != nil {
			msg = fmt.Sprintf("husk activation transport error: %v", err)
		} else if res.Error != "" {
			msg = "husk activation failed: " + res.Error
		}
		logger.Info("husk activation failed, pending", "pod", pod.Name, "node", pod.Spec.NodeName, "detail", msg)
		recordClaimError(claim.Spec.PoolRef.Name, "activate")
		claim.Status.Phase = v1alpha1.SandboxPending
		setCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(r.now()),
			Reason:             "ActivateFailed",
			Message:            msg + "; the claim will retry",
		})
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}

	endpoint := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(sandboxPort))

	// The pod was already claimed (optimistic-lock label patch) BEFORE activation,
	// so this VM belongs to exactly this claim. Hand the token to the claim's
	// consumer via an owned Secret BEFORE the Ready
	// write (same ordering as the forkd path).
	if err := ensureSandboxTokenSecret(ctx, r.Client, claim, claim.Name+tokenSecretSuffix, apiToken, endpoint); err != nil {
		logger.Error(err, "token secret write failed")
		recordClaimError(claim.Spec.PoolRef.Name, "token")
		now := metav1.Now()
		claim.Status.Phase = v1alpha1.SandboxFailed
		claim.Status.FinishedAt = &now
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	claim.Status.Phase = v1alpha1.SandboxReady
	claim.Status.Endpoint = endpoint
	claim.Status.Node = pod.Spec.NodeName
	claim.Status.SandboxID = pod.Name
	claim.Status.StartedAt = &now
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "HuskActivated",
		Message:            fmt.Sprintf("activated husk pod %s on node %s in %.2fms", pod.Name, pod.Spec.NodeName, res.LatencyMs),
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("sandbox claimed via husk activation", "sandbox", claim.Name, "pod", pod.Name, "node", pod.Spec.NodeName)
	return ctrl.Result{}, nil
}

// huskNotifyNetwork maps the template's network policy to the guest
// NotifyForkedNetwork delivered in the activate handshake. The husk slice
// threads the template network through for parity with the engine fork path; the
// detailed mapping is a follow-up, so this returns nil (no overrides) until the
// guest networking slice lands.
func huskNotifyNetwork(_ *v1alpha1.SandboxTemplate) *vsock.NotifyForkedNetwork {
	return nil
}

// reconcileNoCapacity handles a placement that no node admits under the
// overcommit policy. It pends the claim with an LLM-legible NoCapacity
// condition and backs off, stamping the first-pending instant. Once the claim
// has waited longer than the bounded max-pending duration without ever placing,
// it gives up and fails the claim with an actionable capacity-exhaustion
// message (and a claim_errors metric, reason "capacity"). A claim that becomes
// admittable before the deadline proceeds to Restoring on a later reconcile.
func (r *SandboxClaimReconciler) reconcileNoCapacity(ctx context.Context, claim *v1alpha1.SandboxClaim, cause error) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	now := r.now()

	// Stamp the first-pending instant on the metadata annotation if absent.
	// This is the durable deadline anchor across reconciles.
	pendingSince := now
	if stamp := claim.Annotations[pendingSinceAnnotation]; stamp != "" {
		if parsed, perr := time.Parse(time.RFC3339, stamp); perr == nil {
			pendingSince = parsed
		}
	} else {
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[pendingSinceAnnotation] = now.Format(time.RFC3339)
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	waited := now.Sub(pendingSince)
	maxWait := r.maxPendingDuration()

	// Bounded fail: capacity never freed within the allowed wait. Surface an
	// actionable terminal error and stamp FinishedAt so the GC TTL pass reaps it.
	if waited >= maxWait {
		recordClaimError(claim.Spec.PoolRef.Name, "capacity")
		finished := metav1.NewTime(now)
		claim.Status.Phase = v1alpha1.SandboxFailed
		claim.Status.FinishedAt = &finished
		setCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: finished,
			Reason:             "CapacityExhausted",
			Message: fmt.Sprintf(
				"no node had memory capacity under the overcommit policy for %s (waited past the %s bound); scale out forkd nodes or raise the overcommit factor, then recreate the claim",
				waited.Round(time.Second), maxWait,
			),
		})
		_ = r.Status().Update(ctx, claim)
		logger.Info("claim failed: capacity exhausted past bounded wait", "claim", claim.Name, "waited", waited.Round(time.Second), "maxWait", maxWait)
		return ctrl.Result{}, nil
	}

	// Within the bounded wait: pend with backpressure and retry.
	claim.Status.Phase = v1alpha1.SandboxPending
	recordClaimPending()
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(now),
		Reason:             "NoCapacity",
		Message: fmt.Sprintf(
			"no node has memory capacity under the overcommit policy; the claim will retry (waited %s of %s), scale out nodes or raise the overcommit factor",
			waited.Round(time.Second), maxWait,
		),
	})
	// Best-effort status write; the return below requeues regardless.
	_ = r.Status().Update(ctx, claim)
	return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
}

// clearPendingSince removes the capacity-pending stamp from the claim's
// annotations (in memory; the caller persists if needed). Idempotent.
func (r *SandboxClaimReconciler) clearPendingSince(claim *v1alpha1.SandboxClaim) {
	delete(claim.Annotations, pendingSinceAnnotation)
}

// reconcileDelete reaps the claim's backing VM via forkd Terminate, then
// removes the finalizer so the API object can be garbage collected. A claim
// that never acquired a sandbox (no Node or SandboxID) skips straight to
// finalizer removal. terminateOnNode treats a NotFound sandbox and a
// vanished node as already-terminated, so a node that left the registry never
// hangs deletion.
func (r *SandboxClaimReconciler) reconcileDelete(ctx context.Context, claim *v1alpha1.SandboxClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(claim, FinalizerTerminate) {
		return ctrl.Result{}, nil
	}

	// Dehydrate the sandbox /workspace into a new committed revision BEFORE
	// reaping the VM on delete (the guest must still be alive to tar its
	// workspace). A claim already dehydrated by a lifetime-expiry terminate is a
	// no-op (the dehydrated annotation). A dehydrate error requeues the delete so
	// the finalizer is not removed until the work is captured.
	if err := r.dehydrateOnTerminate(ctx, claim); err != nil {
		logger.Error(err, "dehydrate workspace on delete; will retry", "claim", claim.Name)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}

	if claim.Status.Node != "" && claim.Status.SandboxID != "" {
		if err := terminateOnNode(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID); err != nil {
			logger.Error(err, "terminate backing sandbox on delete", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(claim, FinalizerTerminate)
	if err := r.Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileLifetime drives a Ready claim to the terminal Terminated phase when
// it exceeds maxLifetime (Spec.Timeout from StartedAt) or goes idle
// (Spec.IdleTimeout from the later of last-activity and StartedAt). Expiry
// terminates the backing VM directly via terminateOnNode and leaves the
// finalizer in place; the bounded, tolerant terminateOnNode keeps eventual
// delete safe. A claim already Terminated returns immediately (idempotent).
func (r *SandboxClaimReconciler) reconcileLifetime(ctx context.Context, claim *v1alpha1.SandboxClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if claim.Status.Phase == v1alpha1.SandboxTerminated {
		return ctrl.Result{}, nil
	}
	if claim.Status.StartedAt == nil {
		return ctrl.Result{}, nil
	}

	hasMaxLifetime := claim.Spec.Timeout != nil
	hasIdle := claim.Spec.IdleTimeout != nil
	if !hasMaxLifetime && !hasIdle {
		return ctrl.Result{}, nil
	}

	now := time.Now()
	startedAt := claim.Status.StartedAt.Time

	// maxLifetime takes precedence: it does not depend on a reachable forkd.
	if hasMaxLifetime {
		deadline := startedAt.Add(claim.Spec.Timeout.Duration)
		if !now.Before(deadline) {
			return r.terminateLifetime(ctx, claim, "MaxLifetimeExceeded",
				fmt.Sprintf("max lifetime %s exceeded", claim.Spec.Timeout.Duration))
		}
	}

	// Idle check needs last-activity from forkd. An unreachable node means we
	// cannot evaluate idle this pass; requeue and try again.
	requeue := time.Duration(0)
	if hasIdle {
		_, lastActivity, ok := sandboxActivity(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID)
		if !ok {
			logger.Info("cannot evaluate idle, node unreachable; requeueing", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		last := startedAt
		if lastActivity.After(last) {
			last = lastActivity
		}
		idleDeadline := last.Add(claim.Spec.IdleTimeout.Duration)
		if now.After(idleDeadline) {
			return r.terminateLifetime(ctx, claim, "IdleTimeout",
				fmt.Sprintf("idle for more than %s", claim.Spec.IdleTimeout.Duration))
		}
		requeue = time.Until(idleDeadline)
	}

	// Requeue at the nearest deadline.
	if hasMaxLifetime {
		untilMax := time.Until(startedAt.Add(claim.Spec.Timeout.Duration))
		if requeue == 0 || untilMax < requeue {
			requeue = untilMax
		}
	}
	if requeue <= 0 {
		requeue = 1 * time.Second
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// terminateLifetime reaps the claim's backing VM and stamps the terminal
// Terminated phase with a FinishedAt time and a Terminated condition. The
// finalizer stays in place; the bounded terminateOnNode keeps later delete
// safe.
func (r *SandboxClaimReconciler) terminateLifetime(ctx context.Context, claim *v1alpha1.SandboxClaim, reason, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Dehydrate the sandbox /workspace into a new committed revision BEFORE
	// reaping the VM (the guest must still be alive to tar its workspace). A
	// dehydrate error requeues without terminating, so the work is not lost; the
	// operation is idempotent via the dehydrated annotation.
	if err := r.dehydrateOnTerminate(ctx, claim); err != nil {
		logger.Error(err, "dehydrate workspace on lifetime expiry; will retry", "claim", claim.Name)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}

	if claim.Status.Node != "" && claim.Status.SandboxID != "" {
		if err := terminateOnNode(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID); err != nil {
			logger.Error(err, "terminate backing sandbox on lifetime expiry", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	claim.Status.Phase = v1alpha1.SandboxTerminated
	claim.Status.FinishedAt = &now
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Terminated",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("claim terminated by lifetime policy", "claim", claim.Name, "reason", reason)
	return ctrl.Result{}, nil
}

type forkResult struct {
	SandboxID  string
	Endpoint   string
	ForkTimeMs float64
}

func (r *SandboxClaimReconciler) selectNode(ctx context.Context, pool *v1alpha1.SandboxPool, preferredNode string) (*NodeInfo, string, error) {
	templateName := pool.Spec.TemplateRef.Name
	node, err := r.NodeRegistry.SelectNode(templateName, preferredNode)
	if err != nil {
		return nil, "", err
	}
	return node, templateName, nil
}

func (r *SandboxClaimReconciler) resolveSecrets(ctx context.Context, namespace string, env []corev1.EnvVar, secrets []v1alpha1.SecretMount) (envOut, secretsOut map[string]string, err error) {
	envOut = make(map[string]string)
	secretsOut = make(map[string]string)

	for _, e := range env {
		envOut[e.Name] = e.Value
	}

	coreClient := r.Client
	for _, s := range secrets {
		var secret corev1.Secret
		if err := coreClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      s.SecretRef.Name,
		}, &secret); err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", s.SecretRef.Name, err)
		}
		value, ok := secret.Data[s.SecretRef.Key]
		if !ok {
			return nil, nil, fmt.Errorf("key %s not found in secret %s", s.SecretRef.Key, s.SecretRef.Name)
		}
		envVar := s.EnvVar
		if envVar == "" {
			envVar = s.Name
		}
		secretsOut[envVar] = string(value)
	}

	return envOut, secretsOut, nil
}

func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr)
	// The claim event filter is scoped to the PRIMARY claim source only (not the
	// pod Watches below), so the test harness can run a raw and a husk reconciler
	// on one manager: each filters the CLAIMS it owns, while the husk pod watch
	// stays unfiltered (a pod carries no claim label, so a global filter would
	// drop every mapped pod event and break the re-pend trigger).
	if r.eventFilter != nil {
		b = b.For(&v1alpha1.SandboxClaim{}, builder.WithPredicates(r.eventFilter))
	} else {
		b = b.For(&v1alpha1.SandboxClaim{})
	}
	// In husk mode, watch husk pods and map a pod event to the claim named in its
	// agentrun.dev/claim label. A husk pod delete (drain, eviction, kubectl
	// delete) then promptly reconciles the active claim, which re-pends per the
	// pool's DrainPolicy instead of waiting for the claim's own periodic requeue.
	// The mapped reconcile re-Gets the claim and routes through checkHuskPodLost,
	// so a claim the husk reconciler does not own (no husk-test label, in the
	// test harness) is simply a no-op reconcile for it. The raw reconciler does
	// not register this watch (no husk pods).
	if r.EnableHuskPods {
		b = b.Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(huskPodToClaim),
		)
	}
	// A name lets the test harness run a raw and a husk claim reconciler on one
	// manager (controller-runtime auto-names by kind and would collide). The
	// production default (controllerName empty) keeps the kind-derived name.
	if r.controllerName != "" {
		b = b.Named(r.controllerName)
	}
	return b.Complete(r)
}
