package daemon

import (
	"context"

	"github.com/paperclipinc/sandbox/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RequireControllerIdentity rejects RPCs whose mTLS peer is not the
// controller. Installed only when forkd serves TLS.
func RequireControllerIdentity(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	name, ok := pki.PeerDNSName(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	if name != pki.ControllerName {
		return nil, status.Error(codes.PermissionDenied, "peer "+name+" may not call forkd")
	}
	return handler(ctx, req)
}
