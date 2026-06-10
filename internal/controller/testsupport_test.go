package controller

// Test support: used by envtest suites. Kept in the main package so external
// test packages (controller_test) can start fake forkd nodes.

import (
	"net"
	"os"

	"github.com/paperclipinc/sandbox/internal/daemon"
	"github.com/paperclipinc/sandbox/internal/fork"
	"google.golang.org/grpc"
)

// StartFakeForkdNode runs an in-process forkd gRPC server backed by a
// MockEngine with the given templates, registers it in the registry, and
// returns a stop function.
func StartFakeForkdNode(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), err error) {
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	for _, tmpl := range templates {
		if err := engine.CreateTemplate(tmpl, tmpl, 0); err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp("", "fake-forkd-*")
	if err != nil {
		return nil, err
	}
	srv := daemon.NewServer(engine, daemon.NewSandboxAPI(dir))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	gs := grpc.NewServer()
	daemon.RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)

	registry.Register(&NodeInfo{
		Name:         nodeName,
		Endpoint:     lis.Addr().String(),
		HTTPEndpoint: lis.Addr().String(), // tests only need a non-empty reachable-shaped value
		TemplateIDs:  templates,
		MaxSandboxes: 100,
	})
	return func() {
		gs.Stop()
		os.RemoveAll(dir)
		registry.Unregister(nodeName)
	}, nil
}
