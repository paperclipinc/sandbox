package controller

import "errors"

// ErrNoCapacity is returned by SelectNode when the registry has healthy nodes
// but none of them can admit the projected fork under the current overcommit
// policy. Callers use errors.Is to distinguish a transient capacity shortage
// (scale out or raise the overcommit factor) from a hard scheduling error. It
// is deliberately distinct from the empty-registry and no-healthy-nodes errors.
var ErrNoCapacity = errors.New("no node has capacity")

// Default cold-start estimates used when no node reports a per-template
// estimate for the requested template (the template has never been forked
// anywhere). These keep a cold placement's marginal cost non-zero so the
// scheduler does not treat an unknown template as free.
const (
	defaultColdSharedBytes int64 = 256 * 1024 * 1024 // 256 MiB shared set
	defaultForkUniqueBytes int64 = 8 * 1024 * 1024   // 8 MiB per-fork unique
)

// available returns the schedulable headroom on a node under the overcommit
// factor: total*factor - used. A node reporting MemoryTotal 0 has an UNKNOWN
// budget (the forkd meminfo read failed, e.g. darwin/dev, or mock without a
// total); such nodes are treated as effectively unlimited so dev and mock
// paths keep scheduling. The bool reports whether the budget is known.
func (r *NodeRegistry) available(node *NodeInfo) (int64, bool) {
	if node.MemoryTotal <= 0 {
		return 0, false
	}
	factor := r.overcommitFactor
	if factor <= 0 {
		factor = 1.0
	}
	return int64(float64(node.MemoryTotal)*factor) - node.MemoryUsed, true
}

// isWarmFor reports whether the node already runs forks of templateID: it holds
// the snapshot and its per-template estimate records at least one fork. A warm
// node pays only the per-fork unique cost for an additional fork (the shared
// set is already resident); a cold node pays the shared set too.
func (n *NodeInfo) isWarmFor(templateID string) bool {
	if templateID == "" {
		return false
	}
	if est, ok := n.TemplateEstimates[templateID]; ok && est.ForkCount > 0 {
		return true
	}
	return false
}

// forkCountFor returns how many forks of templateID the node runs, per its
// per-template estimate (0 when unknown). Used to rank warm holders by density.
func (n *NodeInfo) forkCountFor(templateID string) int32 {
	if est, ok := n.TemplateEstimates[templateID]; ok {
		return est.ForkCount
	}
	return 0
}

// templateEstimateAcross returns a per-template estimate for templateID,
// preferring the candidate node's own estimate and falling back to ANY healthy
// node that has one. The bool reports whether any node knew the template.
// Caller must hold at least the read lock.
func (r *NodeRegistry) templateEstimateAcross(node *NodeInfo, templateID string) (TemplateCapacity, bool) {
	if templateID == "" {
		return TemplateCapacity{}, false
	}
	if est, ok := node.TemplateEstimates[templateID]; ok {
		return est, true
	}
	for _, n := range r.nodes {
		if est, ok := n.TemplateEstimates[templateID]; ok {
			return est, true
		}
	}
	return TemplateCapacity{}, false
}

// projectedCost is the marginal memory a fork of templateID would add to node.
// Warm node: only the average per-fork unique footprint (shared set already
// resident). Cold node: the shared set paid once plus the per-fork unique.
// Estimates come from this node first, then any node, then a configured
// default. Caller must hold at least the read lock.
func (r *NodeRegistry) projectedCost(node *NodeInfo, templateID string) int64 {
	est, known := r.templateEstimateAcross(node, templateID)

	unique := defaultForkUniqueBytes
	if known && est.AvgForkUniqueBytes > 0 {
		unique = est.AvgForkUniqueBytes
	}

	if node.isWarmFor(templateID) {
		return unique
	}

	shared := defaultColdSharedBytes
	if known && est.SharedOnceBytes > 0 {
		shared = est.SharedOnceBytes
	}
	return shared + unique
}

// admits reports whether node can take a fork of templateID: its known budget
// has room for the projected cost. Nodes with an unknown budget (MemoryTotal 0)
// always admit (dev/mock). Caller must hold at least the read lock.
func (r *NodeRegistry) admits(node *NodeInfo, templateID string) bool {
	avail, known := r.available(node)
	if !known {
		return true
	}
	return r.projectedCost(node, templateID) <= avail
}
