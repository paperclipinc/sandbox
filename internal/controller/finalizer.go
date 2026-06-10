package controller

import (
	"context"
	"fmt"

	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
)

// FinalizerTerminate guards a claim (and, later, a fork) so its backing VM is
// reaped via forkd Terminate before the object is removed from the API,
// regardless of how deletion was triggered.
const FinalizerTerminate = "agentrun.dev/forkd-terminate"

// terminateOnNode asks the forkd on nodeName to terminate sandboxID. It treats
// two outcomes as already-terminated and returns nil:
//   - the node is gone from the registry (or its connection cannot be dialed):
//     the VM died with the node, so there is nothing left to reap;
//   - forkd answers NotFound: the sandbox is already gone.
//
// Any other error (an unhealthy but present node, a transient RPC failure) is
// returned so the caller keeps the finalizer and retries.
func terminateOnNode(ctx context.Context, registry *NodeRegistry, nodeName, sandboxID string) error {
	if _, ok := registry.GetNode(nodeName); !ok {
		// Node left the registry: the VM left with it. Already terminated.
		return nil
	}
	conn, err := registry.GetConnection(nodeName)
	if err != nil {
		// The node record exists but we cannot dial it; treat as gone so a
		// deletion never hangs on a vanished node.
		return nil
	}
	_, err = forkdpb.NewForkDaemonClient(conn).Terminate(ctx, &forkdpb.TerminateRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("forkd terminate %s on %s: %w", sandboxID, nodeName, err)
	}
	return nil
}
