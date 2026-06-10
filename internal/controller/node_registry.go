package controller

import (
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// NodeRegistry tracks forkd instances across the cluster.
// Forkd pods register themselves via gRPC heartbeats.
// The controller uses this to select nodes for fork operations.
type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeInfo

	// TLS, when set, is the controller's mTLS client config used to dial
	// every node that does not carry its own NodeInfo.TLS. Set once at
	// startup before any dial; nil means insecure (tests, mock mode).
	TLS *tls.Config
}

type NodeInfo struct {
	Name     string
	Endpoint string
	// HTTPEndpoint is the forkd HTTP sandbox API (exec/files), e.g. "10.0.3.7:9091".
	// This is what claim status endpoints point at.
	HTTPEndpoint    string
	ActiveSandboxes int32
	MaxSandboxes    int32
	MemoryTotal     int64
	MemoryUsed      int64
	TemplateIDs     []string
	SnapshotIDs     []string
	LastHeartbeat   time.Time
	// TLS, when set, overrides the registry-level TLS config for dials to
	// this node; lets tests run mixed TLS/insecure fleets in one registry.
	TLS  *tls.Config
	conn *grpc.ClientConn
}

func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes: make(map[string]*NodeInfo),
	}
}

// Register adds or updates a node in the registry.
func (r *NodeRegistry) Register(info *NodeInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodes == nil {
		r.nodes = make(map[string]*NodeInfo)
	}
	if old, ok := r.nodes[info.Name]; ok && old.conn != nil {
		if old.Endpoint == info.Endpoint && info.conn == nil {
			info.conn = old.conn // carry the dialed connection forward
		} else {
			old.conn.Close()
		}
	}
	info.LastHeartbeat = time.Now()
	r.nodes[info.Name] = info
}

// Unregister removes a node from the registry.
func (r *NodeRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if node, ok := r.nodes[name]; ok {
		if node.conn != nil {
			node.conn.Close()
		}
		delete(r.nodes, name)
	}
}

// SelectNode picks the best node for a fork operation.
// Prefers nodes with: 1) the requested snapshot, 2) lowest active sandbox count.
func (r *NodeRegistry) SelectNode(snapshotID string, preferredNode string) (*NodeInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.nodes) == 0 {
		return nil, fmt.Errorf("no forkd nodes registered")
	}

	// If preferred node is specified and available, use it
	if preferredNode != "" {
		if node, ok := r.nodes[preferredNode]; ok {
			if node.isHealthy() {
				return node, nil
			}
		}
	}

	// Find nodes that have the requested snapshot
	var candidates []*NodeInfo
	for _, node := range r.nodes {
		if !node.isHealthy() {
			continue
		}
		if snapshotID == "" || node.hasSnapshot(snapshotID) {
			candidates = append(candidates, node)
		}
	}

	if len(candidates) == 0 {
		// Fall back to any healthy node
		for _, node := range r.nodes {
			if node.isHealthy() {
				candidates = append(candidates, node)
			}
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no healthy forkd nodes available")
	}

	// Pick the node with the lowest load
	best := candidates[0]
	for _, node := range candidates[1:] {
		if node.ActiveSandboxes < best.ActiveSandboxes {
			best = node
		}
	}

	return best, nil
}

// NodesWithTemplate returns healthy nodes that hold the given template snapshot.
func (r *NodeRegistry) NodesWithTemplate(templateID string) []*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*NodeInfo
	for _, n := range r.nodes {
		if n.isHealthy() && n.hasSnapshot(templateID) {
			out = append(out, n)
		}
	}
	return out
}

// AddTemplate records that a node now holds the given template snapshot.
// Takes the write lock so NodeInfo.TemplateIDs is never mutated concurrently
// with readers like NodesWithTemplate.
func (r *NodeRegistry) AddTemplate(nodeName, templateID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	node, ok := r.nodes[nodeName]
	if !ok {
		return
	}
	for _, t := range node.TemplateIDs {
		if t == templateID {
			return
		}
	}
	node.TemplateIDs = append(node.TemplateIDs, templateID)
}

// GetNode returns the registered node by name.
func (r *NodeRegistry) GetNode(name string) (*NodeInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	node, ok := r.nodes[name]
	return node, ok
}

// GetConnection returns a gRPC connection to a node's forkd, dialing once.
// Transport credentials are chosen per node: NodeInfo.TLS wins, then the
// registry-level TLS config, then insecure (tests and mock mode).
func (r *NodeRegistry) GetConnection(nodeName string) (*grpc.ClientConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	node, ok := r.nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("node %s not found", nodeName)
	}
	if node.conn != nil {
		return node.conn, nil
	}

	creds := insecure.NewCredentials()
	switch {
	case node.TLS != nil:
		creds = credentials.NewTLS(node.TLS)
	case r.TLS != nil:
		creds = credentials.NewTLS(r.TLS)
	}
	conn, err := grpc.NewClient(
		node.Endpoint,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to forkd on %s: %w", nodeName, err)
	}
	node.conn = conn
	return conn, nil
}

// ListNodes returns all registered nodes.
func (r *NodeRegistry) ListNodes() []*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	nodes := make([]*NodeInfo, 0, len(r.nodes))
	for _, n := range r.nodes {
		nodes = append(nodes, n)
	}
	return nodes
}

// PruneStale removes nodes that haven't sent a heartbeat recently.
func (r *NodeRegistry) PruneStale(maxAge time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	pruned := 0
	for name, node := range r.nodes {
		if time.Since(node.LastHeartbeat) >= maxAge {
			if node.conn != nil {
				node.conn.Close()
			}
			delete(r.nodes, name)
			pruned++
		}
	}
	return pruned
}

func (n *NodeInfo) isHealthy() bool {
	return time.Since(n.LastHeartbeat) < 2*time.Minute
}

func (n *NodeInfo) hasSnapshot(id string) bool {
	for _, s := range n.SnapshotIDs {
		if s == id {
			return true
		}
	}
	for _, t := range n.TemplateIDs {
		if t == id {
			return true
		}
	}
	return false
}
