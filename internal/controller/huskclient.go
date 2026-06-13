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

	if err := husk.WriteRequest(conn, req); err != nil {
		return husk.ActivateResult{}, fmt.Errorf("send activate request to %s: %w", addr, err)
	}
	res, err := husk.ReadResult(conn)
	if err != nil {
		return husk.ActivateResult{}, fmt.Errorf("read activate result from %s: %w", addr, err)
	}
	return res, nil
}
