package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FinalizerTerminate guards a claim (and, later, a fork) so its backing VM is
// reaped via forkd Terminate before the object is removed from the API,
// regardless of how deletion was triggered.
const FinalizerTerminate = "mitos.run/forkd-terminate"

// terminateOnNode asks the forkd on nodeName to terminate sandboxID. It is
// bounded and tolerant so a claim's deletion always makes progress: the
// finalizer must never wedge an object on an unreachable node.
//
// It treats the following as already-terminated and returns nil:
//   - the node has left the registry (or cannot be dialed): the VM left with
//     the node, so there is nothing to reap;
//   - the node is present but unhealthy (no recent heartbeat): the VM is on a
//     node we can no longer reach, so the orphan sweep / node-death path owns
//     reaping it;
//   - forkd answers NotFound: the sandbox is already gone;
//   - the Terminate RPC returns Unavailable or DeadlineExceeded: forkd cannot
//     confirm termination on an unreachable node, so the orphan sweep will reap
//     the VM if forkd recovers; the object must not wedge in the meantime.
//
// Internal and other unexpected errors are returned so a genuinely-reachable
// forkd that rejects the call is retried. The RPC is bounded by a 10s timeout,
// so even a forkd that hangs surfaces DeadlineExceeded and yields success.
func terminateOnNode(ctx context.Context, registry *NodeRegistry, nodeName, sandboxID string) error {
	if _, ok := registry.GetNode(nodeName); !ok {
		// Node left the registry: the VM left with it. Already terminated.
		return nil
	}
	if !registry.NodeHealthy(nodeName) {
		// Present but no recent heartbeat: the VM is on a node we can no longer
		// reach. The orphan sweep / node-death path reaps it; do not wedge.
		return nil
	}
	conn, err := registry.GetConnection(nodeName)
	if err != nil {
		// The node record exists but we cannot dial it; treat as gone so a
		// deletion never hangs on a vanished node.
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = forkdpb.NewForkDaemonClient(conn).Terminate(cctx, &forkdpb.TerminateRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		if isAlreadyTerminated(err) {
			return nil
		}
		return fmt.Errorf("forkd terminate %s on %s: %w", sandboxID, nodeName, err)
	}
	return nil
}

// isAlreadyTerminated reports whether err (possibly wrapped) means the sandbox
// can be treated as gone for deletion purposes: NotFound (already reaped) or a
// node we cannot confirm-terminate on (Unavailable, DeadlineExceeded).
func isAlreadyTerminated(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := status.FromError(e); ok {
			switch s.Code() {
			case codes.NotFound, codes.Unavailable, codes.DeadlineExceeded:
				return true
			}
		}
	}
	return false
}
