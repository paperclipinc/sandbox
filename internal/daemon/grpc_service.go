package daemon

import (
	"context"
	"strings"

	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcService implements forkdpb.ForkDaemonServer over Server.
// Exec returns Unimplemented with a pointer to the HTTP sandbox API on the
// forkd HTTP port (default :9091), which serves exec and file traffic today;
// the remaining unimplemented RPCs fall through to the embedded stub.
type grpcService struct {
	forkdpb.UnimplementedForkDaemonServer
	srv *Server
}

func (g *grpcService) Fork(ctx context.Context, req *forkdpb.ForkRequest) (*forkdpb.ForkResponse, error) {
	result, err := g.srv.Fork(ctx, req.SnapshotId, req.SandboxId, envMap(req.Env), secretMap(req.Secrets), req.ApiToken)
	if err != nil {
		return nil, grpcError(err)
	}
	return &forkdpb.ForkResponse{
		SandboxId:         result.SandboxID,
		Endpoint:          result.Endpoint,
		ForkTimeMs:        result.ForkTimeMs,
		MemoryUniqueBytes: result.MemoryUnique,
		MemorySharedBytes: result.MemoryShared,
	}, nil
}

func (g *grpcService) ForkRunning(ctx context.Context, req *forkdpb.ForkRunningRequest) (*forkdpb.ForkRunningResponse, error) {
	result, err := g.srv.ForkRunning(ctx, req.SourceSandboxId, req.NewSandboxId, req.PauseSource, req.ApiToken)
	if err != nil {
		return nil, grpcError(err)
	}
	return &forkdpb.ForkRunningResponse{
		SandboxId:  result.SandboxID,
		Endpoint:   result.Endpoint,
		ForkTimeMs: result.ForkTimeMs,
	}, nil
}

func (g *grpcService) Terminate(ctx context.Context, req *forkdpb.TerminateRequest) (*forkdpb.TerminateResponse, error) {
	if err := g.srv.Terminate(ctx, req.SandboxId); err != nil {
		return nil, grpcError(err)
	}
	return &forkdpb.TerminateResponse{}, nil
}

func (g *grpcService) GetCapacity(ctx context.Context, _ *forkdpb.GetCapacityRequest) (*forkdpb.GetCapacityResponse, error) {
	c := g.srv.engine.GetCapacity()
	return &forkdpb.GetCapacityResponse{
		ActiveSandboxes:   c.ActiveSandboxes,
		MaxSandboxes:      c.MaxSandboxes,
		MemoryTotalBytes:  c.MemoryTotal,
		MemoryUsedBytes:   c.MemoryUsed,
		MemorySharedBytes: c.MemoryShared,
		TemplateIds:       c.TemplateIDs,
		SnapshotIds:       c.SnapshotIDs,
		KvmAvailable:      c.KVMAvailable,
	}, nil
}

func (g *grpcService) CreateTemplate(ctx context.Context, req *forkdpb.CreateTemplateRequest) (*forkdpb.CreateTemplateResponse, error) {
	if err := g.srv.engine.CreateTemplate(req.TemplateId, req.Image, 0); err != nil {
		return nil, grpcError(err)
	}
	return &forkdpb.CreateTemplateResponse{TemplateId: req.TemplateId}, nil
}

func (g *grpcService) Exec(ctx context.Context, _ *forkdpb.ExecRequest) (*forkdpb.ExecResponse, error) {
	return nil, status.Error(codes.Unimplemented, "exec is served by the HTTP sandbox API on the forkd HTTP port")
}

func envMap(vars []*forkdpb.EnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Key] = v.Value
	}
	return m
}

func secretMap(vars []*forkdpb.SecretVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Key] = v.Value
	}
	return m
}

// grpcError maps engine errors to gRPC status codes.
func grpcError(err error) error {
	if strings.Contains(err.Error(), "not found") {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
