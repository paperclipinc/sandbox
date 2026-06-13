package daemon

import (
	"context"

	"github.com/paperclipinc/mitos/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RequireControllerIdentity rejects RPCs whose mTLS peer is not the
// controller. Installed only when forkd serves TLS.
func RequireControllerIdentity(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	name, ok := pki.PeerDNSName(ctx)
	if !ok {
		// Unreachable while the server enforces RequireAndVerifyClientCert; kept as defense in depth.
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	if name != pki.ControllerName {
		return nil, status.Error(codes.PermissionDenied, "peer "+name+" may not call forkd")
	}
	return handler(ctx, req)
}

// RequireControllerIdentityStream is the streaming twin of
// RequireControllerIdentity; without it, streaming RPCs (ExecStream) would
// bypass the identity check entirely.
func RequireControllerIdentityStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	name, ok := pki.PeerDNSName(ss.Context())
	if !ok {
		// Unreachable while the server enforces RequireAndVerifyClientCert; kept as defense in depth.
		return status.Error(codes.Unauthenticated, "client certificate required")
	}
	if name != pki.ControllerName {
		return status.Error(codes.PermissionDenied, "peer "+name+" may not call forkd")
	}
	return handler(srv, ss)
}
