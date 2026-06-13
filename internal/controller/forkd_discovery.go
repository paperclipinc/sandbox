package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const forkdComponentLabel = "app.kubernetes.io/component"

// ForkdDiscovery keeps the NodeRegistry in sync with running forkd pods.
// It lists labeled pods periodically, registers them, refreshes capacity via
// GetCapacity, and prunes nodes that stop heartbeating.
type ForkdDiscovery struct {
	Client    client.Client
	Registry  *NodeRegistry
	Namespace string        // namespace forkd runs in, e.g. "mitos"
	Interval  time.Duration // default 15s
	GRPCPort  int           // default 9090
	HTTPPort  int           // default 9091
	CASPort   int           // default 9092 (dedicated token-gated TLS CAS listener)
	// TLS, when set, is the controller's mTLS client config; discovery uses
	// it for its own capacity dials and stamps it onto every NodeInfo it
	// registers so registry dials to discovered nodes use mTLS too. Nil
	// means insecure (tests, mock mode).
	TLS *tls.Config
}

// ForkdDiscovery lists the labeled forkd Pods in the control plane namespace to
// learn each node's pod IP and feed the NodeRegistry. Read-only.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
func (d *ForkdDiscovery) Start(ctx context.Context) error {
	if d.Interval == 0 {
		d.Interval = 15 * time.Second
	}
	if d.GRPCPort == 0 {
		d.GRPCPort = 9090
	}
	if d.HTTPPort == 0 {
		d.HTTPPort = 9091
	}
	if d.CASPort == 0 {
		d.CASPort = 9092
	}
	ticker := time.NewTicker(d.Interval)
	defer ticker.Stop()
	for {
		d.sync(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (d *ForkdDiscovery) sync(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("forkd-discovery")

	var pods corev1.PodList
	if err := d.Client.List(ctx, &pods,
		client.InNamespace(d.Namespace),
		client.MatchingLabels{forkdComponentLabel: "forkd"},
	); err != nil {
		logger.Error(err, "list forkd pods")
		return
	}

	d.syncPods(ctx, pods.Items)

	d.Registry.PruneStale(2 * time.Minute)
}

// syncPods registers every running forkd pod and refreshes its capacity.
func (d *ForkdDiscovery) syncPods(ctx context.Context, pods []corev1.Pod) {
	logger := log.FromContext(ctx).WithName("forkd-discovery")
	for _, pod := range pods {
		info, ok := NodeInfoFromPod(pod, d.GRPCPort, d.HTTPPort, d.CASPort)
		if !ok {
			continue
		}
		info.TLS = d.TLS
		// Populate capacity before the registry ever sees the struct;
		// registered NodeInfo fields are read under the registry's RLock and
		// must never be mutated afterwards outside it.
		probeOK := d.refreshCapacity(ctx, info)
		// Tie health to the LIVENESS probe: a failed GetCapacity (forkd hung or
		// host dead while the pod is still Running) increments the consecutive
		// failure count carried across re-registers; a success resets it. A node
		// past the threshold is dropped from scheduling by isHealthy.
		if probeOK {
			info.probeFailures = 0
		} else {
			info.probeFailures = d.Registry.priorProbeFailures(info.Name) + 1
			logger.Info("forkd liveness probe failed", "node", info.Name, "consecutiveFailures", info.probeFailures, "threshold", probeFailureThreshold)
		}
		d.Registry.Register(info)
	}
}

// refreshCapacity fills template/capacity fields via forkd's GetCapacity,
// dialing the node directly (the node is not registered yet; the registry
// must only ever see fully-populated NodeInfo structs; see AddTemplate's
// locking contract). It returns whether the probe SUCCEEDED: a failed probe is
// the liveness signal the caller uses to mark a node unhealthy, so a hung forkd
// (pod still Running) is not registered as schedulable on stale capacity.
func (d *ForkdDiscovery) refreshCapacity(ctx context.Context, info *NodeInfo) bool {
	creds := insecure.NewCredentials()
	if d.TLS != nil {
		creds = credentials.NewTLS(d.TLS)
	}
	conn, err := grpc.NewClient(info.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return false
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := forkdpb.NewForkDaemonClient(conn).GetCapacity(cctx, &forkdpb.GetCapacityRequest{})
	if err != nil {
		return false
	}
	info.ActiveSandboxes = resp.ActiveSandboxes
	info.MaxSandboxes = resp.MaxSandboxes
	info.MemoryTotal = resp.MemoryTotalBytes
	info.MemoryUsed = resp.MemoryUsedBytes
	info.TemplateIDs = resp.TemplateIds
	info.SnapshotIDs = resp.SnapshotIds
	info.TemplateDigests = resp.TemplateDigests
	if len(resp.Templates) > 0 {
		estimates := make(map[string]TemplateCapacity, len(resp.Templates))
		for _, t := range resp.Templates {
			estimates[t.TemplateId] = TemplateCapacity{
				TemplateID:         t.TemplateId,
				SnapshotDigest:     t.SnapshotDigest,
				SharedOnceBytes:    t.SharedOnceBytes,
				AvgForkUniqueBytes: t.AvgForkUniqueBytes,
				ForkCount:          t.ForkCount,
			}
		}
		info.TemplateEstimates = estimates
	}
	return true
}

// NodeInfoFromPod maps a forkd pod to a NodeInfo. Returns false when the pod
// is not running, has no IP, or has no node assignment yet. The CAS endpoint is
// the same pod IP as the HTTP endpoint with the dedicated CAS port (casPort):
// CAS distribution is served on its own TLS listener, a separate port from the
// sandbox HTTP API.
func NodeInfoFromPod(pod corev1.Pod, grpcPort, httpPort, casPort int) (*NodeInfo, bool) {
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || pod.Spec.NodeName == "" {
		return nil, false
	}
	return &NodeInfo{
		Name:         pod.Spec.NodeName,
		Endpoint:     fmt.Sprintf("%s:%d", pod.Status.PodIP, grpcPort),
		HTTPEndpoint: fmt.Sprintf("%s:%d", pod.Status.PodIP, httpPort),
		CASEndpoint:  fmt.Sprintf("%s:%d", pod.Status.PodIP, casPort),
	}, true
}
