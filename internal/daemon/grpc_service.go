package daemon

import (
	"context"
	"strings"

	"github.com/paperclipinc/mitos/internal/observability"
	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// tracer is the forkd component tracer; no-op unless tracing is configured.
var tracer = observability.Tracer("mitos-forkd")

// grpcService implements forkdpb.ForkDaemonServer over Server.
// Exec returns Unimplemented with a pointer to the HTTP sandbox API on the
// forkd HTTP port (default :9091), which serves exec and file traffic today;
// the remaining unimplemented RPCs fall through to the embedded stub.
type grpcService struct {
	forkdpb.UnimplementedForkDaemonServer
	srv *Server
}

func (g *grpcService) Fork(ctx context.Context, req *forkdpb.ForkRequest) (*forkdpb.ForkResponse, error) {
	if err := validateIDs(req.SnapshotId, req.SandboxId); err != nil {
		return nil, err
	}
	// forkd.Fork is a child of the controller's forkOnNode span when the
	// trace context propagated over gRPC (otelgrpc server handler). Only ids
	// are recorded; env, secrets, and the api token are never attributes.
	ctx, span := tracer.Start(ctx, "forkd.Fork", trace.WithAttributes(
		attribute.String("snapshot.id", req.SnapshotId),
		attribute.String("sandbox.id", req.SandboxId),
	))
	defer span.End()

	// If the controller delivered an at-rest WRAPPED DEK over the mTLS RPC, stash
	// it (plus its KEK id) for the engine to unwrap and open the source
	// template's encrypted container, and forget it once the fork returns.
	// Neither the wrapped DEK nor the (eventual) plaintext is ever logged or
	// recorded as a span attribute. The engine unwraps via the KMS and zeroizes
	// the plaintext.
	if len(req.EncryptionKey) > 0 && g.srv.keyProvider != nil {
		g.srv.keyProvider.SetWrappedKey(req.SnapshotId, req.EncryptionKey, req.KekId)
		defer g.srv.keyProvider.ForgetKey(req.SnapshotId)
	}

	result, err := g.srv.Fork(ctx, req.SnapshotId, req.SandboxId, envMap(req.Env), secretMap(req.Secrets), req.Network, req.Volumes, req.ApiToken)
	if err != nil {
		span.RecordError(err)
		return nil, grpcError(err)
	}
	span.SetAttributes(attribute.Float64("fork_time_ms", result.ForkTimeMs))
	return &forkdpb.ForkResponse{
		SandboxId:         result.SandboxID,
		Endpoint:          result.Endpoint,
		ForkTimeMs:        result.ForkTimeMs,
		MemoryUniqueBytes: result.MemoryUnique,
		MemorySharedBytes: result.MemoryShared,
	}, nil
}

func (g *grpcService) ForkRunning(ctx context.Context, req *forkdpb.ForkRunningRequest) (*forkdpb.ForkRunningResponse, error) {
	if err := validateIDs(req.SourceSandboxId, req.NewSandboxId); err != nil {
		return nil, err
	}
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
	if err := validateIDs(req.SandboxId); err != nil {
		return nil, err
	}
	if err := g.srv.Terminate(ctx, req.SandboxId); err != nil {
		return nil, grpcError(err)
	}
	return &forkdpb.TerminateResponse{}, nil
}

func (g *grpcService) ListSandboxes(ctx context.Context, _ *forkdpb.ListSandboxesRequest) (*forkdpb.ListSandboxesResponse, error) {
	return &forkdpb.ListSandboxesResponse{Sandboxes: g.srv.ListSandboxes()}, nil
}

func (g *grpcService) GetCapacity(ctx context.Context, _ *forkdpb.GetCapacityRequest) (*forkdpb.GetCapacityResponse, error) {
	c := g.srv.engine.GetCapacity()
	templates := make([]*forkdpb.TemplateCapacity, 0, len(c.TemplateEstimates))
	for _, t := range c.TemplateEstimates {
		templates = append(templates, &forkdpb.TemplateCapacity{
			TemplateId:         t.TemplateID,
			SnapshotDigest:     t.SnapshotDigest,
			SharedOnceBytes:    t.SharedOnceBytes,
			AvgForkUniqueBytes: t.AvgForkUniqueBytes,
			ForkCount:          t.ForkCount,
		})
	}
	return &forkdpb.GetCapacityResponse{
		ActiveSandboxes:   c.ActiveSandboxes,
		MaxSandboxes:      c.MaxSandboxes,
		MemoryTotalBytes:  c.MemoryTotal,
		MemoryUsedBytes:   c.MemoryUsed,
		MemorySharedBytes: c.MemoryShared,
		TemplateIds:       c.TemplateIDs,
		SnapshotIds:       c.SnapshotIDs,
		KvmAvailable:      c.KVMAvailable,
		TemplateDigests:   c.TemplateDigests,
		Templates:         templates,
	}, nil
}

func (g *grpcService) CreateTemplate(ctx context.Context, req *forkdpb.CreateTemplateRequest) (*forkdpb.CreateTemplateResponse, error) {
	if err := validateIDs(req.TemplateId); err != nil {
		return nil, err
	}
	vols, err := volumeSpecs(req.Volumes)
	if err != nil {
		return nil, grpcError(err)
	}
	// If the controller delivered an at-rest WRAPPED DEK over the mTLS RPC, stash
	// it (plus its KEK id) for the engine to unwrap and build the snapshot inside
	// an encrypted container, and forget it once the build returns. Neither the
	// wrapped DEK nor the (eventual) plaintext is ever logged or recorded as a
	// span attribute. The engine unwraps via the KMS and zeroizes the plaintext.
	if len(req.EncryptionKey) > 0 && g.srv.keyProvider != nil {
		g.srv.keyProvider.SetWrappedKey(req.TemplateId, req.EncryptionKey, req.KekId)
		defer g.srv.keyProvider.ForgetKey(req.TemplateId)
	}
	if err := g.srv.engine.CreateTemplate(req.TemplateId, req.Image, req.InitCommands, vols); err != nil {
		return nil, grpcError(err)
	}
	// Report the content-addressed digest the engine just recorded so the
	// controller can store it in the SandboxPool status. The mock engine does
	// not produce one; an empty digest is acceptable there.
	digest := g.srv.engine.GetCapacity().TemplateDigests[req.TemplateId]
	return &forkdpb.CreateTemplateResponse{
		TemplateId:     req.TemplateId,
		TemplateDigest: digest,
	}, nil
}

// PullTemplate fetches a template's snapshot from a peer forkd's CAS and
// records it locally. The PullToken in the request is a credential: it is
// passed straight to the engine and is NEVER logged or recorded as a span
// attribute. Only the template id, source URL, and digest (content addresses,
// safe to log) are surfaced.
func (g *grpcService) PullTemplate(ctx context.Context, req *forkdpb.PullTemplateRequest) (*forkdpb.PullTemplateResponse, error) {
	if err := validateIDs(req.TemplateId); err != nil {
		return nil, err
	}
	ctx, span := tracer.Start(ctx, "forkd.PullTemplate", trace.WithAttributes(
		attribute.String("template.id", req.TemplateId),
		attribute.String("source.url", req.SourceUrl),
		attribute.String("manifest.digest", req.ManifestDigest),
	))
	defer span.End()

	if err := g.srv.engine.PullTemplate(ctx, req.TemplateId, req.ManifestDigest, req.SourceUrl, req.PullToken); err != nil {
		span.RecordError(err)
		return nil, grpcError(err)
	}
	digest := g.srv.engine.GetCapacity().TemplateDigests[req.TemplateId]
	return &forkdpb.PullTemplateResponse{
		TemplateId:     req.TemplateId,
		TemplateDigest: digest,
	}, nil
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

// validateIDs runs validateSandboxID over every caller-supplied id of a
// request and maps the first failure to InvalidArgument. Ids flow into
// host filesystem paths (workspaces, snapshots, jailer chroots), so they
// are rejected here before any engine code runs (C1).
func validateIDs(ids ...string) error {
	for _, id := range ids {
		if err := validateSandboxID(id); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	return nil
}

// grpcError maps engine errors to gRPC status codes. An error that already
// carries a gRPC status (e.g. the InvalidArgument from volume-name validation)
// is passed through unchanged so its code is not flattened to Internal.
func grpcError(err error) error {
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	if strings.Contains(err.Error(), "not found") {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
