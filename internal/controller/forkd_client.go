package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// isNotFound reports whether err (possibly wrapped) carries gRPC NotFound.
func isNotFound(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := status.FromError(e); ok && s.Code() == codes.NotFound {
			return true
		}
	}
	return false
}

// sandboxActivity queries the forkd on nodeName via ListSandboxes for
// sandboxID and returns its created-at and last-activity times. ok is false
// when the node is unreachable or the sandbox is not in the listing; the
// caller must then treat the idle check as not-yet-evaluable and requeue. The
// dial and RPC are bounded by a short timeout so a slow or dead forkd cannot
// block the reconcile. lastActivity is the zero time when the sandbox has
// never been accessed.
func sandboxActivity(ctx context.Context, registry *NodeRegistry, nodeName, sandboxID string) (created, lastActivity time.Time, ok bool) {
	conn, err := registry.GetConnection(nodeName)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := forkdpb.NewForkDaemonClient(conn).ListSandboxes(cctx, &forkdpb.ListSandboxesRequest{})
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	for _, s := range resp.Sandboxes {
		if s.SandboxId != sandboxID {
			continue
		}
		var last time.Time
		if s.LastActivityUnix != 0 {
			last = time.Unix(s.LastActivityUnix, 0)
		}
		return time.Unix(s.CreatedAtUnix, 0), last, true
	}
	return time.Time{}, time.Time{}, false
}

// forkOnNode asks the forkd on the given node to fork a sandbox from a snapshot.
// The returned endpoint is the node's HTTP sandbox API: what clients (SDKs)
// actually talk to. apiToken is the bearer token forkd registers for the
// sandbox's HTTP API; it is never logged. network is the template's egress
// policy (egress mode + allowlist); nil leaves the ForkRequest's NetworkConfig
// unset and forkd applies no per-fork egress ruleset. The policy and allowlist
// entries (IPs/ports/names) are safe to log.
func (r *SandboxClaimReconciler) forkOnNode(ctx context.Context, node *NodeInfo, snapshotID, sandboxID string, env, secrets map[string]string, network *v1alpha1.NetworkPolicy, apiToken string) (*forkResult, error) {
	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		return nil, err
	}
	resp, err := forkdpb.NewForkDaemonClient(conn).Fork(ctx, &forkdpb.ForkRequest{
		SnapshotId: snapshotID,
		SandboxId:  sandboxID,
		Env:        toEnvVars(env),
		Secrets:    toSecretVars(secrets),
		Network:    toNetworkConfig(network),
		ApiToken:   apiToken,
	})
	if err != nil {
		return nil, fmt.Errorf("forkd fork on %s: %w", node.Name, err)
	}
	return &forkResult{
		SandboxID:  resp.SandboxId,
		Endpoint:   clientEndpoint(node, resp.Endpoint),
		ForkTimeMs: resp.ForkTimeMs,
	}, nil
}

// forkRunningOnNode asks forkd to checkpoint a running sandbox and fork it.
// apiToken is the NEW sandbox's own bearer token (the source's token does
// not open the fork); it is never logged.
func (r *SandboxForkReconciler) forkRunningOnNode(ctx context.Context, node *NodeInfo, sourceSandboxID, newSandboxID string, pauseSource bool, apiToken string) (*forkRunningResult, error) {
	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		return nil, err
	}
	resp, err := forkdpb.NewForkDaemonClient(conn).ForkRunning(ctx, &forkdpb.ForkRunningRequest{
		SourceSandboxId: sourceSandboxID,
		NewSandboxId:    newSandboxID,
		PauseSource:     pauseSource,
		ApiToken:        apiToken,
	})
	if err != nil {
		return nil, fmt.Errorf("forkd fork-running on %s: %w", node.Name, err)
	}
	return &forkRunningResult{
		SandboxID:    resp.SandboxId,
		Endpoint:     clientEndpoint(node, resp.Endpoint),
		ForkTimeMs:   resp.ForkTimeMs,
		CheckpointMs: resp.CheckpointTimeMs,
	}, nil
}

// clientEndpoint prefers the node's HTTP sandbox API; the engine-reported
// endpoint is an internal placeholder until guest networking exists.
func clientEndpoint(node *NodeInfo, engineEndpoint string) string {
	if node.HTTPEndpoint != "" {
		return node.HTTPEndpoint
	}
	return engineEndpoint
}

func toEnvVars(m map[string]string) []*forkdpb.EnvVar {
	vars := make([]*forkdpb.EnvVar, 0, len(m))
	for k, v := range m {
		vars = append(vars, &forkdpb.EnvVar{Key: k, Value: v})
	}
	return vars
}

// toNetworkConfig maps the template's NetworkPolicy onto the proto
// NetworkConfig forkd consumes. A nil policy (template declares no network)
// yields nil, leaving the fork un-networked. The allowlist is passed through
// verbatim; forkd splits it into enforceable IP:port entries and name-based
// entries (the latter logged as not-yet-enforced), so the controller does not
// validate or filter it here.
func toNetworkConfig(n *v1alpha1.NetworkPolicy) *forkdpb.NetworkConfig {
	if n == nil {
		return nil
	}
	return &forkdpb.NetworkConfig{
		EgressPolicy: string(n.Egress),
		AllowList:    n.Allow,
	}
}

func toSecretVars(m map[string]string) []*forkdpb.SecretVar {
	vars := make([]*forkdpb.SecretVar, 0, len(m))
	for k, v := range m {
		vars = append(vars, &forkdpb.SecretVar{Key: k, Value: v})
	}
	return vars
}
