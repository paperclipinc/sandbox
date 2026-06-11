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
	"github.com/paperclipinc/sandbox/internal/netconf"
	"github.com/paperclipinc/sandbox/internal/network"
	"github.com/paperclipinc/sandbox/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	var (
		listenAddr      string
		httpAddr        string
		dataDir         string
		firecrackerBin  string
		kernelPath      string
		mockMode        bool
		tlsCert         string
		tlsKey          string
		tlsCA           string
		jailerBin       string
		chrootBase      string
		uidRange        string
		casDir          string
		allowUnverified bool
		enableNet       bool
		sandboxSubnet   string
		uplink          string
		dnsResolver     string
		agentBin        string
		busyboxBin      string
		enableVolumes   bool
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
	flag.StringVar(&jailerBin, "jailer", "", "Jailer binary path; every VM is launched through it with a per-VM uid and chroot. Empty disables the jailer (development only)")
	flag.StringVar(&chrootBase, "chroot-base", "/srv/jailer", "Jailer chroot base directory; must share a filesystem with --data-dir")
	flag.StringVar(&uidRange, "uid-range", "64000-64999", "Inclusive uid/gid range for per-VM jailer users, formatted low-high")
	flag.StringVar(&casDir, "cas-dir", "", "Content-addressed store directory for snapshot integrity and transfer. Empty means <data-dir>/cas")
	flag.BoolVar(&allowUnverified, "allow-unverified-snapshots", false, "Allow forking snapshots that fail or skip integrity verification (development only; refused by default)")
	flag.BoolVar(&enableNet, "enable-networking", false, "Enable per-sandbox guest networking (tap device, egress nftables, NIC attach). Default false until proven on KVM CI")
	flag.StringVar(&sandboxSubnet, "sandbox-subnet", "10.200.0.0/16", "IPv4 subnet carved into per-sandbox /30 point-to-point links; requires --enable-networking")
	flag.StringVar(&uplink, "uplink", "", "Host egress interface for the optional sandbox-subnet MASQUERADE rule. Empty relies on the node's existing NAT")
	flag.StringVar(&dnsResolver, "dns-resolver", "", "DNS resolver IP guests may reach; adds a DNS allow rule to each fork's egress ruleset. Empty omits the rule")
	flag.StringVar(&agentBin, "agent-bin", "", "Path to the guest agent binary injected as /init when a template is built from an OCI image. Required for image builds; unused for file-path rootfs templates. For now this binary must be present in the forkd image (a follow-up will go:embed it)")
	flag.StringVar(&busyboxBin, "busybox-bin", "", "Optional path to a static busybox providing /bin/sh, injected when an image ships no shell. Empty means images without a shell cannot run init")
	flag.BoolVar(&enableVolumes, "enable-volumes", false, "Enable per-fork volume drives: the template build bakes a placeholder drive per template volume and each fork prepares its own backing and rebinds the drive. Default false until proven on KVM CI")
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
		if err := mock.CreateTemplate("default", "python:3.12-slim", nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: mock template: %v\n", err)
			os.Exit(1)
		}
		engine = mock
	} else {
		jailerCfg, err := buildJailerConfig(jailerBin, chrootBase, uidRange, dataDir, os.Geteuid(), sameDevice)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forkd: %v\n", err)
			os.Exit(1)
		}
		if !jailerCfg.Enabled() {
			fmt.Fprintln(os.Stderr, "forkd: jailer DISABLED; Firecracker runs unjailed as forkd's user (threat model section 1); supply --jailer for any non-development deployment")
		}
		engineOpts := fork.EngineOpts{
			CASDir:          casDir,
			AllowUnverified: allowUnverified,
			AgentBinPath:    agentBin,
			BusyboxPath:     busyboxBin,
			EnableVolumes:   enableVolumes,
		}
		if enableVolumes {
			fmt.Println("forkd: per-fork volumes ENABLED")
		}
		if enableNet {
			alloc, err := netconf.NewAllocator(sandboxSubnet, "sbtap")
			if err != nil {
				fmt.Fprintf(os.Stderr, "forkd: invalid --sandbox-subnet: %v\n", err)
				os.Exit(1)
			}
			engineOpts.NetManager = network.NewManager(network.Options{
				SubnetCIDR: sandboxSubnet,
				Uplink:     uplink,
				// The node is assumed to forward already; the optional uplink
				// MASQUERADE covers SNAT when set. Forwarding is not toggled
				// here to avoid surprising the host's sysctl state.
			})
			engineOpts.NetAllocator = alloc
			if dnsResolver != "" {
				ip := net.ParseIP(dnsResolver)
				if ip == nil {
					fmt.Fprintf(os.Stderr, "forkd: invalid --dns-resolver %q\n", dnsResolver)
					os.Exit(1)
				}
				engineOpts.ResolverIP = ip
			}
			fmt.Printf("forkd: per-sandbox networking ENABLED (subnet %s, uplink %q)\n", sandboxSubnet, uplink)
		}
		real, err := fork.NewEngine(dataDir, firecrackerBin, kernelPath, jailerCfg, engineOpts)
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
