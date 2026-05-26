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
		httpAddr       string
		dataDir        string
		firecrackerBin string
		kernelPath     string
		mockMode       bool
	)

	flag.StringVar(&listenAddr, "listen", ":9090", "gRPC listen address (controller communication)")
	flag.StringVar(&httpAddr, "http", ":9091", "HTTP listen address (metrics + sandbox exec/files API)")
	flag.StringVar(&dataDir, "data-dir", "/var/lib/agent-run", "Data directory for snapshots and sandboxes")
	flag.StringVar(&firecrackerBin, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	flag.StringVar(&kernelPath, "kernel", "/var/lib/agent-run/vmlinux", "Guest kernel path")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required)")
	flag.Parse()

	var engine daemon.ForkEngine

	if mockMode {
		fmt.Println("forkd: running in mock mode")
		mock := fork.NewMockEngine()
		mock.CreateTemplate("default", "python:3.12-slim", 0)
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

	grpcServer := grpc.NewServer()
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
