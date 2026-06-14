package husk

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/paperclipinc/mitos/internal/pki"
)

// AuthorizeControllerIdentity is the authorize hook for ServeTLS that mirrors
// forkd's RequireControllerIdentity interceptor: a control connection is
// accepted only when its VERIFIED mTLS peer presents the controller leaf's DNS
// SAN. The identity is read from VerifiedChains (set by the TLS handshake under
// RequireAndVerifyClientCert), never from the certificates a peer merely
// presented, so an unverified or wrong-CA peer can never satisfy it.
//
// An activate request delivers tenant SECRETS to a VM; this is the gate that
// ensures only the controller may drive that path. It returns an error (no
// secret material is in scope here) when the peer is missing an identity or is
// not the controller.
func AuthorizeControllerIdentity(state *tls.ConnectionState) error {
	if state == nil {
		return errors.New("husk control: no TLS state on connection")
	}
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		// Unreachable while the listener enforces RequireAndVerifyClientCert; kept
		// as defense in depth so a misconfigured TLS config still fails closed.
		return errors.New("husk control: client certificate required")
	}
	leaf := state.VerifiedChains[0][0]
	if len(leaf.DNSNames) == 0 || leaf.DNSNames[0] != pki.ControllerName {
		name := "<none>"
		if len(leaf.DNSNames) > 0 {
			name = leaf.DNSNames[0]
		}
		return fmt.Errorf("husk control: peer %q may not activate this husk", name)
	}
	return nil
}

// ServeTLS serves the line-delimited JSON control protocol (control.go's
// ReadRequest / WriteResult) over an mTLS net.Listener, dispatching each
// accepted connection to stub.Activate.
//
// SECURITY: this is the network control channel that activates a dormant VM
// with tenant secrets. tlsConf MUST require and verify client certificates
// (build it with pki.ServerTLSConfig). authorize MUST verify the peer identity
// (use AuthorizeControllerIdentity so only the controller may activate); a nil
// authorize is rejected because an unauthenticated activate channel that
// delivers secrets is unacceptable. A connection whose handshake fails, or
// whose verified peer is not authorized, is closed without ever reading an
// ActivateRequest, so secrets are never accepted from an unauthenticated peer.
//
// Like Stub.Serve, a husk pod is LONG-LIVED: a successful activate does not end
// ServeTLS. It keeps holding the active VM and rejecting further activate
// attempts (via Activate's state check) until ctx is cancelled or the listener
// closes. It never tears the VM down; the caller (cmd/husk-stub) calls
// stub.Close on shutdown.
//
// Secret and entropy VALUES are never logged here: per-connection failures are
// reported to stderr by operation only, with the transport error, never the
// request payload.
func ServeTLS(ctx context.Context, ln net.Listener, stub *Stub, tlsConf *tls.Config, authorize func(*tls.ConnectionState) error) error {
	if tlsConf == nil {
		return errors.New("husk control: refusing to serve network control without TLS")
	}
	if authorize == nil {
		return errors.New("husk control: refusing to serve network control without an authorize hook")
	}

	tlsLn := tls.NewListener(ln, tlsConf)

	// Unblock Accept when the context is cancelled.
	go func() {
		<-ctx.Done()
		_ = tlsLn.Close()
	}()

	for {
		conn, err := tlsLn.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("husk control: accept connection: %w", err)
		}
		serveControlConn(ctx, conn, stub, authorize)
	}
}

// serveControlConn completes the mTLS handshake, authorizes the verified peer,
// then reads one ActivateRequest, runs Activate, and writes the result. Any
// handshake or authorization failure closes the connection without reading the
// request, so a secret-bearing request is never accepted from an
// unauthenticated or unauthorized peer. Errors are logged by operation only;
// the request payload (env/secrets/entropy) is never logged.
func serveControlConn(ctx context.Context, conn net.Conn, stub *Stub, authorize func(*tls.ConnectionState) error) {
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		fmt.Fprintln(os.Stderr, "husk control: non-TLS connection rejected")
		return
	}
	// Force the handshake now so VerifiedChains is populated before authorize.
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "husk control: TLS handshake: %v\n", err)
		return
	}
	state := tlsConn.ConnectionState()
	if err := authorize(&state); err != nil {
		// Authorization failed: do NOT read the request, so no secret material is
		// accepted from this peer. The error names only the identity.
		fmt.Fprintf(os.Stderr, "husk control: %v\n", err)
		return
	}

	br := bufio.NewReader(conn)
	op, err := ReadControlOp(br)
	if err != nil {
		fmt.Fprintf(os.Stderr, "husk control: read control op: %v\n", err)
		return
	}
	switch op {
	case OpActivate:
		req, rerr := readActivateRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read activate request: %v\n", rerr)
			return
		}
		res, _ := stub.Activate(ctx, req)
		if werr := WriteResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write activate result: %v\n", werr)
		}
	case OpForkSnapshot:
		req, rerr := readForkSnapshotRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read fork-snapshot request: %v\n", rerr)
			return
		}
		res, _ := stub.ForkSnapshot(ctx, req)
		if werr := WriteForkSnapshotResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write fork-snapshot result: %v\n", werr)
		}
	case OpRemoveForkSnapshot:
		req, rerr := readRemoveForkSnapshotRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read remove-fork-snapshot request: %v\n", rerr)
			return
		}
		rmErr := stub.RemoveForkSnapshot(ForkSnapshotRequest{ForkID: req.ForkID, SnapshotDir: req.SnapshotDir})
		out := ForkSnapshotResult{OK: rmErr == nil, SnapshotDir: req.SnapshotDir}
		if rmErr != nil {
			out.Error = rmErr.Error()
		}
		if werr := WriteForkSnapshotResult(conn, out); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write remove-fork-snapshot result: %v\n", werr)
		}
	case OpDehydrateWorkspace:
		req, rerr := readDehydrateWorkspaceRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read dehydrate-workspace request: %v\n", rerr)
			return
		}
		res, _ := stub.DehydrateWorkspace(ctx, req)
		if werr := WriteDehydrateWorkspaceResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write dehydrate-workspace result: %v\n", werr)
		}
	case OpHydrateWorkspace:
		req, rerr := readHydrateWorkspaceRequest(br)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "husk control: read hydrate-workspace request: %v\n", rerr)
			return
		}
		res, _ := stub.HydrateWorkspace(ctx, req)
		if werr := WriteHydrateWorkspaceResult(conn, res); werr != nil {
			fmt.Fprintf(os.Stderr, "husk control: write hydrate-workspace result: %v\n", werr)
		}
	default:
		fmt.Fprintf(os.Stderr, "husk control: unknown control op %q\n", op)
	}
}
