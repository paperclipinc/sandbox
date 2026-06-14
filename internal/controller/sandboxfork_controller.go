package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/husk"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// huskForkSnapshotter is the controller->husk fork-snapshot transport seam. Nil
// defaults to ForkSnapshotOnHusk; tests inject a fake.
type huskForkSnapshotter func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error)

// huskForkSnapshotRemover is the controller->husk remove-fork-snapshot seam. Nil
// defaults to RemoveForkSnapshotOnHusk; tests inject a fake.
type huskForkSnapshotRemover func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error)

// huskForkFinalizer guards a husk fork so its node-local fork snapshot is
// removed from the source pod before the SandboxFork object is deleted.
const huskForkFinalizer = "mitos.run/husk-fork-snapshot"

type SandboxForkReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry

	// EnableHuskPods selects the husk fork path: snapshot the source husk pod's
	// running VM, then create + activate N child husk pods from that fork
	// snapshot. Off (default in this struct's zero value) keeps the raw-forkd
	// ForkRunning path unchanged.
	EnableHuskPods bool
	// HuskTLS is the controller client mTLS config used to dial a husk stub's
	// control channel (the SAME config the claim reconciler uses). Required when
	// EnableHuskPods is true.
	HuskTLS *tls.Config
	// HuskControlPort / HuskSandboxPort default to HuskControlPort / huskSandboxPort.
	HuskControlPort int
	HuskSandboxPort int
	// HuskStubImage / DataDir / KVMResourceName configure the fork child pods.
	HuskStubImage   string
	DataDir         string
	KVMResourceName string
	// HuskTLSSecretName / HuskCASecretName are the husk PKI Secrets every husk
	// pod mounts for its mTLS control channel: the leaf (tls.crt, tls.key) at
	// /etc/husk/tls and the CA (ca.crt) at /etc/husk/ca. They are the SAME
	// Secrets the warm-pool reconciler threads into HuskPodOptions; a fork child
	// is a husk pod and its stub reads --tls-cert/--tls-key/--tls-ca from these
	// mounts at start, so both MUST be set or the child crash-loops on the first
	// missing PEM (open /etc/husk/tls/tls.crt: no such file or directory).
	HuskTLSSecretName string
	HuskCASecretName  string

	// forkSnapshot / activate / removeForkSnapshot are the husk control seams.
	// Nil defaults to ForkSnapshotOnHusk / ActivateHuskPod / RemoveForkSnapshotOnHusk.
	// Tests inject fakes.
	forkSnapshot       huskForkSnapshotter
	activate           huskActivator
	removeForkSnapshot huskForkSnapshotRemover

	// eventFilter / controllerName optionally restrict which forks this reconciler
	// watches and name the controller, so a raw and a husk fork reconciler can
	// share one manager in tests. Production leaves them unset.
	eventFilter    predicate.Predicate
	controllerName string
}

// SandboxFork ownership: get/list/watch to reconcile, update to write
// progress, delete for the garbage collector's TTL sweep. status writes
// ReadyForks and conditions (e.g. Rejected). The fork reconciler reads its
// source SandboxClaim, the owning SandboxPool, and the SandboxTemplate, all of
// which are already covered by the SandboxClaim reconciler's markers.
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxforks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxforks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxforks/finalizers,verbs=update
func (r *SandboxForkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var fork v1alpha1.SandboxFork
	if err := r.Get(ctx, req.NamespacedName, &fork); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Husk fork deletion + finalizer handling MUST come before the terminal and
	// already-satisfied short-circuits below: a fork being deleted (or one that
	// has reached its replicas) still needs its node-local fork snapshot GC'd.
	if r.EnableHuskPods {
		if !fork.DeletionTimestamp.IsZero() {
			return r.finalizeHuskFork(ctx, &fork)
		}
		if !controllerutil.ContainsFinalizer(&fork, huskForkFinalizer) {
			controllerutil.AddFinalizer(&fork, huskForkFinalizer)
			if err := r.Update(ctx, &fork); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// A rejected fork is terminal: never reconcile it again.
	if meta.IsStatusConditionTrue(fork.Status.Conditions, "Rejected") {
		return ctrl.Result{}, nil
	}

	if fork.Status.ReadyForks >= fork.Spec.Replicas {
		return ctrl.Result{}, nil
	}

	// Find the source sandbox (a SandboxClaim)
	var source v1alpha1.SandboxClaim
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: fork.Namespace,
		Name:      fork.Spec.SourceRef.Name,
	}, &source); err != nil {
		logger.Error(err, "source sandbox not found", "source", fork.Spec.SourceRef.Name)
		return ctrl.Result{}, err
	}

	// Live-fork secret gate: duplicating guest memory duplicates any
	// delivered secrets into every fork. Default-deny without explicit
	// opt-in. Spec-level check: fires regardless of source readiness.
	if len(source.Spec.Secrets) > 0 {
		now := metav1.Now()
		if !fork.Spec.AllowSecretInheritance {
			setCondition(&fork.Status.Conditions, metav1.Condition{
				Type:               "Rejected",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "SecretInheritanceDenied",
				Message:            "source claim holds secrets; recreate the fork with spec.allowSecretInheritance=true to permit it (forks duplicate guest memory, including secret values)",
			})
			if err := r.Status().Update(ctx, &fork); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil // terminal: no requeue
		}
		// Audit trail for the explicit opt-in. Only write status when the
		// condition is not already recorded, so the status-update-triggered
		// re-reconcile does not loop on itself.
		if c := meta.FindStatusCondition(fork.Status.Conditions, "SecretInheritance"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ExplicitOptIn" {
			setCondition(&fork.Status.Conditions, metav1.Condition{
				Type:               "SecretInheritance",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "ExplicitOptIn",
				Message:            "fork inherits the source's in-memory secrets by explicit opt-in",
			})
			if err := r.Status().Update(ctx, &fork); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if source.Status.Phase != v1alpha1.SandboxReady {
		logger.Info("source sandbox not ready, waiting", "source", source.Name, "phase", source.Status.Phase)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Husk fork path: the source VM is owned by the source husk pod's stub, not
	// forkd's engine, so the only way to live-fork it is for the owning stub to
	// snapshot it and N child husk pods to restore that snapshot. The raw-forkd
	// ForkRunning path below is left unchanged for the non-husk default.
	if r.EnableHuskPods {
		return r.reconcileHuskFork(ctx, &fork, &source)
	}

	// Find the node running the source sandbox. A live/standard fork is pinned
	// to the source sandbox's node by construction: ForkRunning copies the
	// source VM's already-resident guest memory in place, so the fork cannot be
	// placed on any other node and the capacity-aware SelectNode does not apply
	// here (it governs cold claim placement, where a node is genuinely chosen).
	// The node's own admission still guards the live fork at the forkd layer.
	node, ok := r.NodeRegistry.GetNode(source.Status.Node)
	if !ok {
		return ctrl.Result{}, fmt.Errorf("node %s not found in registry", source.Status.Node)
	}

	// Find the template for volume fork policies
	var pool v1alpha1.SandboxPool
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: fork.Namespace,
		Name:      source.Spec.PoolRef.Name,
	}, &pool); err != nil {
		return ctrl.Result{}, err
	}

	var template v1alpha1.SandboxTemplate
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: pool.Namespace,
		Name:      pool.Spec.TemplateRef.Name,
	}, &template); err != nil {
		return ctrl.Result{}, err
	}

	// Create forks
	needed := fork.Spec.Replicas - fork.Status.ReadyForks
	for i := int32(0); i < needed; i++ {
		forkID := fmt.Sprintf("%s-fork-%d", fork.Name, fork.Status.TotalForks+i)

		// Translate the template's volumes (with this fork's VolumeOverrides
		// applied) into the Fork RPC's VolumeMounts. A live fork (ForkRunning)
		// inherits the source's already-attached drives, so these are carried
		// for the node to reconcile rather than re-prepared here.
		volumes := volumeMounts(template.Spec.Volumes, fork.Spec.VolumeOverrides)

		// Per-fork bearer token: the source's token never opens the fork.
		// The value reaches exactly two places: the ForkRunningRequest and
		// the owned token Secret below. Never status, conditions, events,
		// or logs.
		apiToken, err := mintAPIToken()
		if err != nil {
			logger.Error(err, "token minting failed", "fork", forkID)
			continue
		}

		// Call forkd.ForkRunning on the source node
		result, err := r.forkRunningOnNode(ctx, node, source.Status.SandboxID, forkID, fork.Spec.PauseSource, volumes, apiToken)
		if err != nil {
			logger.Error(err, "fork failed", "fork", forkID)
			continue
		}

		// Hand the token to the fork's consumer via a Secret owned by the
		// SandboxFork (GC'd with it). A fork without its token Secret is
		// unusable, so it is not recorded as ready.
		if err := ensureSandboxTokenSecret(ctx, r.Client, &fork, forkID+tokenSecretSuffix, apiToken, result.Endpoint); err != nil {
			logger.Error(err, "token secret write failed", "fork", forkID)
			continue
		}

		fork.Status.Forks = append(fork.Status.Forks, v1alpha1.ForkInfo{
			Name:           forkID,
			SandboxID:      result.SandboxID,
			Endpoint:       result.Endpoint,
			Node:           node.Name,
			Phase:          v1alpha1.SandboxReady,
			ForkTimeMicros: int64(result.ForkTimeMs * 1000),
		})
		fork.Status.ReadyForks++
		fork.Status.TotalForks++

		logger.Info("fork created",
			"fork", forkID,
			"node", node.Name,
			"forkTime", fmt.Sprintf("%.2fms", result.ForkTimeMs),
		)
	}

	now := metav1.Now()
	fork.Status.CheckpointTime = &now
	setCondition(&fork.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(fork.Status.ReadyForks >= fork.Spec.Replicas),
		LastTransitionTime: now,
		Reason:             "ForksCreated",
		Message:            fmt.Sprintf("%d/%d forks ready", fork.Status.ReadyForks, fork.Spec.Replicas),
	})

	if err := r.Status().Update(ctx, &fork); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// huskForksInPodDir is the in-pod path a husk stub writes a fork snapshot to for
// forkID; it matches the --forks-dir mount the pod builder set (huskForksMountPath).
func huskForksInPodDir(forkID string) string { return filepath.Join(huskForksMountPath, forkID) }

// reconcileHuskFork forks a husk-backed source: it snapshots the source pod's
// running VM ONCE (fork-snapshot control op), then creates and activates N child
// husk pods from the fork snapshot, recording each Ready child in the fork
// status. It is the husk analog of the forkd ForkRunning loop. The fork snapshot
// is node-local and shared read-only by the children on the same node, while each
// child gets its own pod + VM + per-activation rootfs CoW clone (independence) and
// runs the same fail-closed RNG/clock reseed handshake a warm pod does.
func (r *SandboxForkReconciler) reconcileHuskFork(ctx context.Context, fork *v1alpha1.SandboxFork, source *v1alpha1.SandboxClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Resolve the source husk pod: a husk claim records Status.SandboxID = pod
	// name and Status.Node = the pod's node.
	srcPod, err := r.findHuskPod(ctx, fork.Namespace, source.Status.SandboxID)
	if err != nil {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}
	if srcPod.Status.PodIP == "" {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	controlPort := r.HuskControlPort
	if controlPort == 0 {
		controlPort = HuskControlPort
	}
	sandboxPort := r.HuskSandboxPort
	if sandboxPort == 0 {
		sandboxPort = huskSandboxPort
	}
	forkSnap := r.forkSnapshot
	if forkSnap == nil {
		forkSnap = ForkSnapshotOnHusk
	}
	activate := r.activate
	if activate == nil {
		activate = ActivateHuskPod
	}

	// One fork snapshot per SandboxFork, keyed by the fork name, taken EXACTLY
	// ONCE and reused for every child across reconcile passes. Children take
	// several passes to reach Ready; re-snapshotting on each pass would re-pause
	// the source and OVERWRITE the fork mem/vmstate, so a child activated in a
	// later pass would restore a NEWER source memory state than an earlier child:
	// the N children would not be a coherent single fork point. The guard is the
	// persisted Status.ForkSnapshotTaken flag, so it survives a controller restart
	// mid-fork (the source is never re-paused once the snapshot exists).
	forkID := fork.Name
	if !fork.Status.ForkSnapshotTaken {
		srcAddr := net.JoinHostPort(srcPod.Status.PodIP, strconv.Itoa(controlPort))
		snapCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		// The source stub writes the snapshot inside its OWN in-pod forks dir mount
		// (huskForksMountPath/<fork-id>); the child reads the same node dir mounted
		// read-only at HuskSnapshotDir.
		snapRes, err := forkSnap(snapCtx, srcAddr, r.HuskTLS, husk.ForkSnapshotRequest{
			ForkID:      forkID,
			SnapshotDir: huskForksInPodDir(forkID),
			PauseSource: fork.Spec.PauseSource,
		})
		if err != nil || !snapRes.OK {
			msg := "fork snapshot did not complete"
			if err != nil {
				msg = fmt.Sprintf("fork snapshot transport error: %v", err)
			} else if snapRes.Error != "" {
				msg = "fork snapshot failed: " + snapRes.Error
			}
			logger.Info("husk fork snapshot failed, requeueing", "source", srcPod.Name, "detail", msg)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		// Record the snapshot was taken BEFORE creating any child, so a crash
		// between here and the child loop does not re-snapshot (re-pause) the
		// source on the next pass. The children always re-read the same fork
		// snapshot dir, so persisting the flag first is safe.
		fork.Status.ForkSnapshotTaken = true
		if err := r.Status().Update(ctx, fork); err != nil {
			return ctrl.Result{}, err
		}
	}

	opts := HuskPodOptions{
		StubImage:       r.HuskStubImage,
		KVMResourceName: r.KVMResourceName,
		SnapshotID:      source.Spec.PoolRef.Name, // template id, for resource/kernel mounts
		DataDir:         r.DataDir,
		ForkSnapshotID:  forkID,
		ForkSourceNode:  source.Status.Node,
		// The husk PKI Secrets the child stub mounts for its --control-listen mTLS
		// channel (leaf at /etc/husk/tls, CA at /etc/husk/ca). buildHuskPod only
		// adds the TLS/CA volumes when these are set; omitting them (the previous
		// bug) leaves the child without its TLS material and the stub crash-loops
		// reading --tls-cert. They are the SAME Secrets the warm-pool path uses.
		TLSSecretName: r.HuskTLSSecretName,
		CASecretName:  r.HuskCASecretName,
		// BUG 1 fix: the child's per-activation rootfs CoW clone must be sourced
		// from the SOURCE sandbox's live rootfs (the disk the fork snapshot's
		// vmstate was baked against), NOT the pristine template rootfs. The source
		// pod name is its Status.SandboxID; its rootfs is visible to the child
		// through the shared husk-rootfs hostPath dir.
		ForkSourceRootfsPath: huskSourceRootfsInPodPath(source.Status.SandboxID),
	}

	// Fixed-slot, idempotent child set. The child pods are EXACTLY Replicas, with
	// STABLE names ("<fork>-fork-<i>" for i in [0, Replicas)) that never change
	// across reconcile passes. The previous count-driven loop derived the name from
	// (TotalForks + i) and the iteration count from (Replicas - ReadyForks): once a
	// child in a pass became Ready it bumped TotalForks mid-loop, so the next i
	// produced a NEW name (fork-2, fork-3, ...) and ensureForkChildPod created an
	// EXTRA pod instead of reusing an existing slot, overcommitting the node. With
	// fixed names the number of child pods can never exceed Replicas regardless of
	// how many passes run or how slowly children become Ready: ensureForkChildPod
	// is get-or-create by the stable name, so each slot maps to exactly one pod.
	//
	// ReadyForks is recomputed from scratch each pass (counting Ready slots) rather
	// than incremented, so a slow or transient child does not permanently inflate
	// the count.
	//
	// A slot already recorded Ready (and so already activated with its token Secret
	// written) is carried forward as-is and NOT re-activated: re-activating a live
	// child VM each pass would mint a fresh token and thrash the restored VM.
	recorded := make(map[string]v1alpha1.ForkInfo, len(fork.Status.Forks))
	for _, f := range fork.Status.Forks {
		recorded[f.Name] = f
	}

	var ready int32
	var forks []v1alpha1.ForkInfo
	for i := int32(0); i < fork.Spec.Replicas; i++ {
		childName := fmt.Sprintf("%s-fork-%d", fork.Name, i)

		// Get-or-create the child pod for this slot (idempotent by the stable name).
		child, err := r.ensureForkChildPod(ctx, fork, childName, opts)
		if err != nil {
			logger.Error(err, "create fork child pod failed", "child", childName)
			continue
		}

		// Already activated in a prior pass: carry the recorded info forward and
		// skip re-activation (idempotent per slot).
		if info, ok := recorded[childName]; ok {
			forks = append(forks, info)
			ready++
			continue
		}

		// The child must be Running+Ready before it can be activated. Not ready yet:
		// requeue this slot next pass WITHOUT creating any extra pod.
		if child.Status.PodIP == "" || !huskPodReady(child) {
			continue
		}

		apiToken, err := mintAPIToken()
		if err != nil {
			logger.Error(err, "token minting failed", "child", childName)
			continue
		}

		addr := net.JoinHostPort(child.Status.PodIP, strconv.Itoa(controlPort))
		actRes, err := activate(ctx, addr, r.HuskTLS, husk.ActivateRequest{
			// The child reads the FORK snapshot here (its <dataDir>/forks/<fork-id>
			// hostPath is mounted at HuskSnapshotDir). No ExpectedDigest: the fork
			// snapshot is node-local, not content-addressed.
			SnapshotDir: HuskSnapshotDir,
			Token:       apiToken,
		})
		if err != nil || !actRes.OK {
			logger.Info("fork child activation failed, will retry", "child", childName)
			continue
		}

		endpoint := net.JoinHostPort(child.Status.PodIP, strconv.Itoa(sandboxPort))
		if err := ensureSandboxTokenSecret(ctx, r.Client, fork, childName+tokenSecretSuffix, apiToken, endpoint); err != nil {
			logger.Error(err, "token secret write failed", "child", childName)
			continue
		}

		forks = append(forks, v1alpha1.ForkInfo{
			Name:      childName,
			SandboxID: child.Name,
			Endpoint:  endpoint,
			Node:      child.Spec.NodeName,
			Phase:     v1alpha1.SandboxReady,
		})
		ready++
	}
	fork.Status.Forks = forks
	fork.Status.ReadyForks = ready
	fork.Status.TotalForks = ready

	now := metav1.Now()
	fork.Status.CheckpointTime = &now
	setCondition(&fork.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(fork.Status.ReadyForks >= fork.Spec.Replicas),
		LastTransitionTime: now,
		Reason:             "ForksCreated",
		Message:            fmt.Sprintf("%d/%d husk forks ready", fork.Status.ReadyForks, fork.Spec.Replicas),
	})
	if err := r.Status().Update(ctx, fork); err != nil {
		return ctrl.Result{}, err
	}
	if fork.Status.ReadyForks < fork.Spec.Replicas {
		// Children still coming up; requeue to drive them Ready.
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// findHuskPod returns the husk pod named name in ns (a husk claim's
// Status.SandboxID is the pod name). It returns an error when not found so the
// caller can requeue.
func (r *SandboxForkReconciler) findHuskPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod); err != nil {
		return nil, fmt.Errorf("get source husk pod %s/%s: %w", ns, name, err)
	}
	return &pod, nil
}

// ensureForkChildPod creates the fork child pod if it does not exist and returns
// the current pod object. Idempotent across requeues (a child already created is
// fetched and returned).
func (r *SandboxForkReconciler) ensureForkChildPod(ctx context.Context, fork *v1alpha1.SandboxFork, name string, opts HuskPodOptions) (*corev1.Pod, error) {
	var existing corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: fork.Namespace, Name: name}, &existing)
	if err == nil {
		return &existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get fork child pod %s: %w", name, err)
	}
	pod := buildForkChildPod(fork, name, opts, r.Scheme())
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create fork child pod %s: %w", name, err)
	}
	// Re-get so the caller sees the server object (with any defaults applied).
	if err := r.Get(ctx, client.ObjectKey{Namespace: fork.Namespace, Name: name}, &existing); err != nil {
		return nil, fmt.Errorf("re-get fork child pod %s: %w", name, err)
	}
	return &existing, nil
}

// finalizeHuskFork removes the node-local fork snapshot from the source husk pod
// (best effort) and clears the finalizer so deletion proceeds. The child pods are
// owner-ref'd to the fork and reaped by Kubernetes GC; only the snapshot dir
// needs explicit cleanup. A transport failure does not block deletion: the dir is
// reclaimed when the source pod is recycled.
func (r *SandboxForkReconciler) finalizeHuskFork(ctx context.Context, fork *v1alpha1.SandboxFork) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(fork, huskForkFinalizer) {
		return ctrl.Result{}, nil
	}

	remove := r.removeForkSnapshot
	if remove == nil {
		remove = RemoveForkSnapshotOnHusk
	}

	// Resolve the source pod to dial; if it is gone the snapshot went with it.
	var source v1alpha1.SandboxClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: fork.Namespace, Name: fork.Spec.SourceRef.Name}, &source); err == nil && source.Status.SandboxID != "" {
		if srcPod, err := r.findHuskPod(ctx, fork.Namespace, source.Status.SandboxID); err == nil && srcPod.Status.PodIP != "" {
			controlPort := r.HuskControlPort
			if controlPort == 0 {
				controlPort = HuskControlPort
			}
			addr := net.JoinHostPort(srcPod.Status.PodIP, strconv.Itoa(controlPort))
			rmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if _, err := remove(rmCtx, addr, r.HuskTLS, husk.RemoveForkSnapshotRequest{
				ForkID:      fork.Name,
				SnapshotDir: huskForksInPodDir(fork.Name),
			}); err != nil {
				logger.Info("remove fork snapshot failed; proceeding with delete", "fork", fork.Name, "detail", err.Error())
			}
		}
	}

	controllerutil.RemoveFinalizer(fork, huskForkFinalizer)
	if err := r.Update(ctx, fork); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

type forkRunningResult struct {
	SandboxID    string
	Endpoint     string
	ForkTimeMs   float64
	CheckpointMs float64
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
func (r *SandboxForkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr)
	if r.eventFilter != nil {
		b = b.For(&v1alpha1.SandboxFork{}, builder.WithPredicates(r.eventFilter))
	} else {
		b = b.For(&v1alpha1.SandboxFork{})
	}
	// The husk fork path owns child husk pods (created + GC'd with the fork).
	b = b.Owns(&corev1.Pod{})
	if r.controllerName != "" {
		b = b.Named(r.controllerName)
	}
	return b.Complete(r)
}
