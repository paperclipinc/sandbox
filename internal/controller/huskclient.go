package controller

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/paperclipinc/mitos/internal/husk"
)

// ActivateHuskPod dials a husk stub's network control server at addr over mTLS
// and runs the line-delimited JSON Activate exchange (husk.WriteRequest /
// husk.ReadResult), returning the stub's ActivateResult.
//
// SECURITY: req carries tenant SECRETS, so the channel MUST be mTLS. tlsConf is
// the controller client config (build it with pki.ClientTLSConfig from the
// controller leaf and the control plane CA); it pins the husk server identity
// and presents the controller client certificate the husk server authorizes. A
// nil tlsConf is refused so secrets are never sent over an unauthenticated
// channel.
//
// Secret and entropy VALUES are never logged here: errors carry only the
// operation, the address, and the transport error, never the request payload.
// The returned ActivateResult.Error likewise never carries secrets (the stub
// guarantees this).
func ActivateHuskPod(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error) {
	if tlsConf == nil {
		return husk.ActivateResult{}, fmt.Errorf("activate husk pod %s: refusing to send activation secrets over an unauthenticated channel", addr)
	}

	dialer := &tls.Dialer{Config: tlsConf}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return husk.ActivateResult{}, fmt.Errorf("dial husk control %s: %w", addr, err)
	}
	defer conn.Close()

	// Bound the exchange by the caller's context deadline so a wedged husk
	// cannot block the reconcile.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := husk.WriteControlOp(conn, husk.OpActivate); err != nil {
		return husk.ActivateResult{}, fmt.Errorf("send activate op to %s: %w", addr, err)
	}
	if err := husk.WriteRequest(conn, req); err != nil {
		return husk.ActivateResult{}, fmt.Errorf("send activate request to %s: %w", addr, err)
	}
	res, err := husk.ReadResult(conn)
	if err != nil {
		return husk.ActivateResult{}, fmt.Errorf("read activate result from %s: %w", addr, err)
	}
	return res, nil
}

// ForkSnapshotOnHusk dials a husk stub's network control at addr over mTLS and
// runs the fork-snapshot op: it asks the stub holding the SOURCE sandbox's
// running VM to pause it, write a Full snapshot to req.SnapshotDir, and resume
// it (unless req.PauseSource). The resulting node-local snapshot is the restore
// image the controller activates child husk pods from. req carries NO secrets
// (a fork id and a snapshot path); the channel is still mTLS because the same
// control surface delivers secrets on activate and must not accept an
// unauthenticated peer for any op.
//
// A nil tlsConf is refused so the control surface is never driven unauthenticated.
func ForkSnapshotOnHusk(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
	if tlsConf == nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("fork-snapshot on husk %s: refusing to drive the control channel unauthenticated", addr)
	}
	dialer := &tls.Dialer{Config: tlsConf}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("dial husk control %s: %w", addr, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := husk.WriteControlOp(conn, husk.OpForkSnapshot); err != nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("send fork-snapshot op to %s: %w", addr, err)
	}
	if err := husk.WriteForkSnapshotRequest(conn, req); err != nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("send fork-snapshot request to %s: %w", addr, err)
	}
	res, err := husk.ReadForkSnapshotResult(conn)
	if err != nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("read fork-snapshot result from %s: %w", addr, err)
	}
	return res, nil
}

// RemoveForkSnapshotOnHusk dials the source husk stub and asks it to delete a
// fork snapshot dir it previously created (the GC counterpart of
// ForkSnapshotOnHusk). It is best-effort from the caller's perspective: the
// SandboxFork finalizer logs but does not block deletion on a transient failure,
// because the dir is reclaimed when the source pod is itself recycled. A nil
// tlsConf is refused.
func RemoveForkSnapshotOnHusk(ctx context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
	if tlsConf == nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("remove fork-snapshot on husk %s: refusing to drive the control channel unauthenticated", addr)
	}
	dialer := &tls.Dialer{Config: tlsConf}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("dial husk control %s: %w", addr, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := husk.WriteControlOp(conn, husk.OpRemoveForkSnapshot); err != nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("send remove-fork-snapshot op to %s: %w", addr, err)
	}
	if err := husk.WriteRemoveForkSnapshotRequest(conn, req); err != nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("send remove-fork-snapshot request to %s: %w", addr, err)
	}
	res, err := husk.ReadForkSnapshotResult(conn)
	if err != nil {
		return husk.ForkSnapshotResult{}, fmt.Errorf("read remove-fork-snapshot result from %s: %w", addr, err)
	}
	return res, nil
}
