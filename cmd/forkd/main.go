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
	"google.golang.org/grpc"
)

func main() {
	var (
		listenAddr     string
		dataDir        string
		firecrackerBin string
		kernelPath     string
		metricsAddr    string
		mockMode       bool
	)

	flag.StringVar(&listenAddr, "listen", ":9090", "gRPC listen address")
	flag.StringVar(&dataDir, "data-dir", "/var/lib/agent-run", "Data directory for snapshots and sandboxes")
	flag.StringVar(&firecrackerBin, "firecracker", "/usr/local/bin/firecracker", "Path to firecracker binary")
	flag.StringVar(&kernelPath, "kernel", "/var/lib/agent-run/vmlinux", "Path to guest kernel")
	flag.StringVar(&metricsAddr, "metrics", ":9091", "Prometheus metrics address")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required)")
	flag.Parse()

	var engine daemon.ForkEngine

	if mockMode {
		fmt.Println("running in mock mode — fork operations are simulated")
		mock := fork.NewMockEngine()
		mock.CreateTemplate("default", "python:3.12-slim")
		engine = mock
	} else {
		real, err := fork.NewEngine(dataDir, firecrackerBin, kernelPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to initialize fork engine: %v\n", err)
			fmt.Fprintf(os.Stderr, "hint: use --mock for local development without KVM\n")
			os.Exit(1)
		}
		engine = real
	}

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen on %s: %v\n", listenAddr, err)
		os.Exit(1)
	}

	server := grpc.NewServer()
	daemon.RegisterForkDaemonServer(server, daemon.NewServer(engine))

	go daemon.ServeMetrics(metricsAddr, engine)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		fmt.Printf("forkd listening on %s\n", listenAddr)
		if err := server.Serve(lis); err != nil {
			fmt.Fprintf(os.Stderr, "gRPC server error: %v\n", err)
			os.Exit(1)
		}
	}()

	<-stop
	fmt.Println("shutting down forkd")
	server.GracefulStop()
}
