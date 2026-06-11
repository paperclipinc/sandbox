package controller

// Test support: used by envtest suites. Kept in the main package so external
// test packages (controller_test) can start fake forkd nodes.

import (
	"crypto/tls"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/paperclipinc/sandbox/internal/daemon"
	"github.com/paperclipinc/sandbox/internal/fork"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// StartFakeForkdNode runs an in-process forkd gRPC server backed by a
// MockEngine with the given templates, registers it in the registry, and
// returns a stop function.
func StartFakeForkdNode(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), err error) {
	stop, _, _, err = startFakeForkdNode(registry, nodeName, nil, nil, templates...)
	return stop, err
}

// StartFakeForkdNodeRecording is StartFakeForkdNode that also returns the
// backing MockEngine, so tests can read engine.TerminatedIDs() to assert a
// VM was reaped via forkd Terminate, and a setActivity closure that stamps a
// sandbox's last-activity time on the node's SandboxAPI (for idle-reap tests).
func StartFakeForkdNodeRecording(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(sandboxID string, t time.Time), err error) {
	return startFakeForkdNode(registry, nodeName, nil, nil, templates...)
}

// StartFakeForkdNodeTLS is StartFakeForkdNode with mTLS: the gRPC listener
// is terminated by serverTLS and the registered NodeInfo carries clientTLS,
// so only dials to THIS node use TLS; other registered fakes stay insecure.
func StartFakeForkdNodeTLS(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, templates ...string) (stop func(), err error) {
	stop, _, _, err = startFakeForkdNode(registry, nodeName, serverTLS, clientTLS, templates...)
	return stop, err
}

func startFakeForkdNode(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(string, time.Time), err error) {
	engine = fork.NewMockEngine()
	engine.ForkDelay = 0
	for _, tmpl := range templates {
		if err := engine.CreateTemplate(tmpl, tmpl, nil, nil); err != nil {
			return nil, nil, nil, err
		}
	}
	dir, err := os.MkdirTemp("", "fake-forkd-*")
	if err != nil {
		return nil, nil, nil, err
	}
	sandboxAPI := daemon.NewSandboxAPI(dir)
	srv := daemon.NewServer(engine, sandboxAPI)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, nil, err
	}
	var opts []grpc.ServerOption
	if serverTLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}
	gs := grpc.NewServer(opts...)
	daemon.RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)

	// Real HTTP sandbox API on a real listener, exactly the handler forkd
	// serves on :9091, so envtest claims can exercise bearer-token auth
	// end to end against the registered HTTPEndpoint.
	httpSrv := httptest.NewServer(sandboxAPI.Handler())

	registry.Register(&NodeInfo{
		Name:         nodeName,
		Endpoint:     lis.Addr().String(),
		HTTPEndpoint: strings.TrimPrefix(httpSrv.URL, "http://"),
		TemplateIDs:  templates,
		MaxSandboxes: 100,
		TLS:          clientTLS,
	})
	setActivity = func(sandboxID string, t time.Time) {
		sandboxAPI.RecordActivity(sandboxID, t)
	}
	return func() {
		gs.Stop()
		httpSrv.Close()
		os.RemoveAll(dir)
		registry.Unregister(nodeName)
	}, engine, setActivity, nil
}
