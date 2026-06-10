package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// GarbageCollector is a manager Runnable that periodically reconciles forkd
// actuals against CRD-desired state. In one pass it sweeps orphan VMs: a forkd
// sandbox on a healthy node with no backing Ready claim or fork child, older
// than OrphanGrace, is terminated. This is also controller-restart
// reconciliation: after a restart the desired set is rebuilt from CRD state and
// any VM not accounted for is reaped.
type GarbageCollector struct {
	Client   client.Client
	Registry *NodeRegistry

	// Interval is the period between GC passes. Default 30s.
	Interval time.Duration
	// OrphanGrace is the minimum uptime a forkd sandbox must have before the
	// orphan sweep will terminate it. This protects a freshly-forked VM whose
	// claim status has not been written yet. Default 60s.
	OrphanGrace time.Duration
}

func (g *GarbageCollector) Start(ctx context.Context) error {
	g.applyDefaults()
	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()
	for {
		g.runOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce executes a single GC pass. It exists so tests can drive one pass
// deterministically instead of waiting on the ticker.
func (g *GarbageCollector) RunOnce(ctx context.Context) {
	g.applyDefaults()
	g.runOnce(ctx)
}

// applyDefaults fills zero-valued tunables so a GarbageCollector driven via
// RunOnce (without Start) still uses the documented defaults.
func (g *GarbageCollector) applyDefaults() {
	if g.Interval == 0 {
		g.Interval = 30 * time.Second
	}
	if g.OrphanGrace == 0 {
		g.OrphanGrace = 60 * time.Second
	}
}

func (g *GarbageCollector) runOnce(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("gc")

	var claims v1alpha1.SandboxClaimList
	if err := g.Client.List(ctx, &claims); err != nil {
		logger.Error(err, "list claims")
		return
	}
	var forks v1alpha1.SandboxForkList
	if err := g.Client.List(ctx, &forks); err != nil {
		logger.Error(err, "list forks")
		return
	}

	desired := g.desiredAlive(claims.Items, forks.Items)

	// Order matters only loosely: markNodeLost only touches claims whose node is
	// unhealthy/absent, and sweepOrphans only visits healthy nodes, so the two
	// never act on the same node. A claim just marked NodeLost stamps
	// FinishedAt=now, so it is too fresh for any later TTL pass to delete.
	g.markNodeLost(ctx, logger, claims.Items)
	g.sweepOrphans(ctx, logger, desired)
}

// desiredAlive builds the set of VMs the control plane expects alive, keyed by
// node then sandbox id: Ready claims (status.Node + status.SandboxID) and
// Ready fork children (fork.Status.Forks entries with a Node, SandboxID, and
// Ready phase).
func (g *GarbageCollector) desiredAlive(claims []v1alpha1.SandboxClaim, forks []v1alpha1.SandboxFork) map[string]map[string]bool {
	desired := make(map[string]map[string]bool)
	add := func(node, id string) {
		if node == "" || id == "" {
			return
		}
		if desired[node] == nil {
			desired[node] = make(map[string]bool)
		}
		desired[node][id] = true
	}
	for i := range claims {
		c := &claims[i]
		if c.Status.Phase == v1alpha1.SandboxReady {
			add(c.Status.Node, c.Status.SandboxID)
		}
	}
	for i := range forks {
		f := &forks[i]
		for _, fi := range f.Status.Forks {
			if fi.Phase == v1alpha1.SandboxReady {
				add(fi.Node, fi.SandboxID)
			}
		}
	}
	return desired
}

// sweepOrphans terminates forkd sandboxes on healthy nodes that are not in the
// desired-alive set and whose uptime exceeds OrphanGrace. Only healthy nodes
// are swept: a VM on an unreachable node is owned by the NodeLost path.
func (g *GarbageCollector) sweepOrphans(ctx context.Context, logger logr.Logger, desired map[string]map[string]bool) {
	for _, node := range g.Registry.ListNodes() {
		if !g.Registry.NodeHealthy(node.Name) {
			continue
		}
		live := g.listSandboxes(ctx, node.Name)
		for _, sb := range live {
			if desired[node.Name][sb.SandboxId] {
				continue
			}
			if sb.UptimeSeconds < int64(g.OrphanGrace.Seconds()) {
				// Freshly forked, status not yet written: leave it alone.
				continue
			}
			if err := terminateOnNode(ctx, g.Registry, node.Name, sb.SandboxId); err != nil {
				logger.Error(err, "terminate orphan sandbox", "node", node.Name, "sandbox", sb.SandboxId)
				continue
			}
			logger.Info("terminated orphan sandbox", "node", node.Name, "sandbox", sb.SandboxId)
		}
	}
}

// markNodeLost transitions Ready claims whose node is no longer a healthy
// registered node to a terminal Failed phase with a NodeLost condition.
//
// We reuse the existing SandboxFailed phase with a NodeLost reason rather than
// adding a dedicated phase const: the phase set stays small and a NodeLost
// claim is, for every consumer, just a failed claim with a specific reason.
// The node is gone, so there is nothing to terminate; we only stamp state,
// bounded by the GC interval.
func (g *GarbageCollector) markNodeLost(ctx context.Context, logger logr.Logger, claims []v1alpha1.SandboxClaim) {
	for i := range claims {
		c := &claims[i]
		if c.Status.Phase != v1alpha1.SandboxReady {
			continue
		}
		if c.Status.Node == "" || g.Registry.NodeHealthy(c.Status.Node) {
			continue
		}
		now := metav1.Now()
		c.Status.Phase = v1alpha1.SandboxFailed
		c.Status.FinishedAt = &now
		setCondition(&c.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             "NodeLost",
			Message:            "node running this sandbox is no longer healthy or registered",
		})
		if err := g.Client.Status().Update(ctx, c); err != nil {
			logger.Error(err, "mark claim NodeLost", "claim", c.Name, "node", c.Status.Node)
			continue
		}
		logger.Info("claim transitioned to NodeLost", "claim", c.Name, "node", c.Status.Node)
	}
}

// listSandboxes calls forkd ListSandboxes on the node with a bounded timeout,
// returning nil on any error (the node will be revisited next pass).
func (g *GarbageCollector) listSandboxes(ctx context.Context, nodeName string) []*forkdpb.SandboxInfo {
	conn, err := g.Registry.GetConnection(nodeName)
	if err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := forkdpb.NewForkDaemonClient(conn).ListSandboxes(cctx, &forkdpb.ListSandboxesRequest{})
	if err != nil {
		return nil
	}
	return resp.Sandboxes
}
