package controller

import (
	"context"
	"errors"
	"fmt"

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

// forkOnNode asks the forkd on the given node to fork a sandbox from a snapshot.
// The returned endpoint is the node's HTTP sandbox API: what clients (SDKs)
// actually talk to.
func (r *SandboxClaimReconciler) forkOnNode(ctx context.Context, node *NodeInfo, snapshotID, sandboxID string, env, secrets map[string]string) (*forkResult, error) {
	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		return nil, err
	}
	resp, err := forkdpb.NewForkDaemonClient(conn).Fork(ctx, &forkdpb.ForkRequest{
		SnapshotId: snapshotID,
		SandboxId:  sandboxID,
		Env:        toEnvVars(env),
		Secrets:    toSecretVars(secrets),
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
func (r *SandboxForkReconciler) forkRunningOnNode(ctx context.Context, node *NodeInfo, sourceSandboxID, newSandboxID string, pauseSource bool) (*forkRunningResult, error) {
	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		return nil, err
	}
	resp, err := forkdpb.NewForkDaemonClient(conn).ForkRunning(ctx, &forkdpb.ForkRunningRequest{
		SourceSandboxId: sourceSandboxID,
		NewSandboxId:    newSandboxID,
		PauseSource:     pauseSource,
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

func toSecretVars(m map[string]string) []*forkdpb.SecretVar {
	vars := make([]*forkdpb.SecretVar, 0, len(m))
	for k, v := range m {
		vars = append(vars, &forkdpb.SecretVar{Key: k, Value: v})
	}
	return vars
}
