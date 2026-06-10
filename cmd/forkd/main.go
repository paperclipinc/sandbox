package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/paperclipinc/sandbox/internal/daemon"
	"github.com/paperclipinc/sandbox/internal/fork"
	"github.com/paperclipinc/sandbox/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	var (
		listenAddr     string
		httpAddr       string
		dataDir        string
		firecrackerBin string
		kernelPath     string
		mockMode       bool
		tlsCert        string
		tlsKey         string
		tlsCA          string
	)

	flag.StringVar(&listenAddr, "listen", ":9090", "gRPC listen address (controller communication)")
	flag.StringVar(&httpAddr, "http", ":9091", "HTTP listen address (metrics + sandbox exec/files API)")
	flag.StringVar(&dataDir, "data-dir", "/var/lib/agent-run", "Data directory for snapshots and sandboxes")
	flag.StringVar(&firecrackerBin, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	flag.StringVar(&kernelPath, "kernel", "/var/lib/agent-run/vmlinux", "Guest kernel path")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required)")
	flag.StringVar(&tlsCert, "tls-cert", "", "Path to the forkd server certificate PEM (mTLS)")
	flag.StringVar(&tlsKey, "tls-key", "", "Path to the forkd server key PEM (mTLS)")
	flag.StringVar(&tlsCA, "tls-ca", "", "Path to the control plane CA certificate PEM (mTLS)")
	flag.Parse()

	grpcOpts, err := grpcServerOptions(tlsCert, tlsKey, tlsCA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: %v\n", err)
		os.Exit(1)
	}

	var engine daemon.ForkEngine

	if mockMode {
		fmt.Println("forkd: running in mock mode")
		mock := fork.NewMockEngine()
		if err := mock.CreateTemplate("default", "python:3.12-slim", 0); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: mock template: %v\n", err)
			os.Exit(1)
		}
		engine = mock
	} else {
		real, err := fork.NewEngine(dataDir, firecrackerBin, kernelPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forkd: failed to initialize: %v\n", err)
			fmt.Fprintf(os.Stderr, "forkd: use --mock for local development without KVM\n")
			os.Exit(1)
		}
		engine = real
	}

	sandboxAPI := daemon.NewSandboxAPI(dataDir)
	server := daemon.NewServer(engine, sandboxAPI)

	// Start gRPC server (controller communication)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: failed to listen on %s: %v\n", listenAddr, err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	daemon.RegisterForkDaemonServer(grpcServer, server)

	// Start HTTP server (metrics + sandbox exec/files API)
	go daemon.ServeHTTP(httpAddr, engine, sandboxAPI)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		fmt.Printf("forkd: gRPC on %s, HTTP on %s\n", listenAddr, httpAddr)
		if err := grpcServer.Serve(lis); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: gRPC error: %v\n", err)
			os.Exit(1)
		}
	}()

	<-stop
	fmt.Println("forkd: shutting down")
	grpcServer.GracefulStop()
}

// grpcServerOptions builds transport security for the controller-facing
// gRPC listener. All three TLS flags set means mTLS with controller
// identity enforcement; none set means insecure with a loud warning;
// a partial set is a configuration error.
func grpcServerOptions(certPath, keyPath, caPath string) ([]grpc.ServerOption, error) {
	set := 0
	for _, p := range []string{certPath, keyPath, caPath} {
		if p != "" {
			set++
		}
	}
	switch set {
	case 0:
		fmt.Fprintln(os.Stderr, "forkd: gRPC is UNAUTHENTICATED; supply --tls-cert/--tls-key/--tls-ca (threat model section 3)")
		return nil, nil
	case 3:
		// fall through to TLS setup below
	default:
		return nil, fmt.Errorf("--tls-cert, --tls-key, and --tls-ca must be set together")
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read --tls-cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read --tls-key: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read --tls-ca: %w", err)
	}
	cfg, err := pki.ServerTLSConfig(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, fmt.Errorf("build server TLS config: %w", err)
	}
	return []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(cfg)),
		grpc.UnaryInterceptor(daemon.RequireControllerIdentity),
		grpc.StreamInterceptor(daemon.RequireControllerIdentityStream),
	}, nil
}
