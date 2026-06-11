package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/paperclipinc/sandbox/internal/daemon"
	"github.com/paperclipinc/sandbox/internal/dnsproxy"
	"github.com/paperclipinc/sandbox/internal/fork"
	"github.com/paperclipinc/sandbox/internal/netconf"
	"github.com/paperclipinc/sandbox/internal/network"
	"github.com/paperclipinc/sandbox/internal/observability"
	"github.com/paperclipinc/sandbox/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	var (
		listenAddr        string
		httpAddr          string
		dataDir           string
		firecrackerBin    string
		kernelPath        string
		mockMode          bool
		tlsCert           string
		tlsKey            string
		tlsCA             string
		jailerBin         string
		chrootBase        string
		uidRange          string
		casDir            string
		allowUnverified   bool
		allowIncompatible bool
		enableNet         bool
		sandboxSubnet     string
		uplink            string
		dnsResolver       string
		enableDNSEgress   bool
		dnsUpstream       string
		agentBin          string
		busyboxBin        string
		enableVolumes     bool
		auditLog          string
		otlpEndpoint      string
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
	flag.BoolVar(&allowIncompatible, "allow-incompatible-snapshots", false, "Allow forking snapshots whose recorded environment (Firecracker version, CPU model, or snapshot format) is incompatible with this host (development only; refused by default)")
	flag.BoolVar(&enableNet, "enable-networking", false, "Enable per-sandbox guest networking (tap device, egress nftables, NIC attach). Default false until proven on KVM CI")
	flag.StringVar(&sandboxSubnet, "sandbox-subnet", "10.200.0.0/16", "IPv4 subnet carved into per-sandbox /30 point-to-point links; requires --enable-networking")
	flag.StringVar(&uplink, "uplink", "", "Host egress interface for the optional sandbox-subnet MASQUERADE rule. Empty relies on the node's existing NAT")
	flag.StringVar(&dnsResolver, "dns-resolver", "", "DNS resolver IP guests may reach; adds a DNS allow rule to each fork's egress ruleset. Empty omits the rule. With --enable-dns-egress this is the address the controlled resolver binds and every guest is pointed at; it defaults to 169.254.1.1 when unset")
	flag.BoolVar(&enableDNSEgress, "enable-dns-egress", false, "Enable name-based egress: run a controlled DNS resolver that resolves only allowlisted names and pins each resolved IP into the sandbox's egress set, and point guests at it. Requires --enable-networking. Default false until proven on KVM CI; when off, name-based allow entries stay unenforced as today")
	flag.StringVar(&dnsUpstream, "dns-upstream", "", "Upstream resolver (host:port) the controlled DNS proxy forwards allowed queries to. Empty derives the first nameserver from /etc/resolv.conf, falling back to 1.1.1.1:53")
	flag.StringVar(&agentBin, "agent-bin", "", "Path to the guest agent binary injected as /init when a template is built from an OCI image. Required for image builds; unused for file-path rootfs templates. For now this binary must be present in the forkd image (a follow-up will go:embed it)")
	flag.StringVar(&busyboxBin, "busybox-bin", "", "Optional path to a static busybox providing /bin/sh, injected when an image ships no shell. Empty means images without a shell cannot run init")
	flag.BoolVar(&enableVolumes, "enable-volumes", false, "Enable per-fork volume drives: the template build bakes a placeholder drive per template volume and each fork prepares its own backing and rebinds the drive. Default false until proven on KVM CI")
	flag.StringVar(&auditLog, "audit-log", "", "Structured audit log of exec and file operations. A file path, or '-'/'stderr' for stderr. Empty disables auditing. Records command strings, paths, and byte counts only; never file content or secret values")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP gRPC endpoint (host:port) for OpenTelemetry trace export. Empty disables tracing (zero cost). Spans carry ids, counts, and timings only; never secret values")
	flag.Parse()

	shutdownTracing, err := observability.Setup(context.Background(), "agentrun-forkd", otlpEndpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: tracing setup: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	grpcOpts, err := grpcServerOptions(tlsCert, tlsKey, tlsCA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: %v\n", err)
		os.Exit(1)
	}
	// The otelgrpc server handler receives the controller's propagated trace
	// context so forkd spans join the controller's trace. Harmless when
	// tracing is disabled (global no-op provider).
	grpcOpts = append(grpcOpts, grpc.StatsHandler(observability.GRPCServerStatsHandler()))

	var engine daemon.ForkEngine
	// dnsProxyServer is the node-level controlled resolver, set only when
	// --enable-dns-egress and networking are both on. Nil otherwise.
	var dnsProxyServer *dnsproxy.Server

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
			CASDir:            casDir,
			AllowUnverified:   allowUnverified,
			AllowIncompatible: allowIncompatible,
			AgentBinPath:      agentBin,
			BusyboxPath:       busyboxBin,
			EnableVolumes:     enableVolumes,
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
			// With DNS egress on, default the resolver IP to a node-wide
			// link-local address the proxy binds and every chain allows on 53.
			if enableDNSEgress && dnsResolver == "" {
				dnsResolver = defaultDNSResolverIP
			}
			if dnsResolver != "" {
				ip := net.ParseIP(dnsResolver)
				if ip == nil {
					fmt.Fprintf(os.Stderr, "forkd: invalid --dns-resolver %q\n", dnsResolver)
					os.Exit(1)
				}
				engineOpts.ResolverIP = ip
			}
			fmt.Printf("forkd: per-sandbox networking ENABLED (subnet %s, uplink %q)\n", sandboxSubnet, uplink)

			// Name-based egress: a controlled DNS resolver bound to the resolver
			// IP, registering each fork's name allowlist (by guest IP) and
			// pinning resolved IPs into that sandbox's nft set. Requires the
			// resolver IP (defaulted above) and networking (the registry keys on
			// the per-fork guest IP).
			if enableDNSEgress {
				registry := dnsproxy.NewRegistry()
				engineOpts.DNSRegistry = registry
				engineOpts.EnableDNSEgress = true
				dnsProxyServer = buildDNSProxy(registry, alloc, dnsResolver, dnsUpstream)
				fmt.Printf("forkd: name-based DNS egress ENABLED (resolver %s, upstream %s)\n", dnsResolver, resolvedUpstream(dnsUpstream))
			}
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
	auditor, auditCloser, err := daemon.AuditorFromFlag(auditLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: %v\n", err)
		os.Exit(1)
	}
	if auditCloser != nil {
		defer auditCloser.Close()
	}
	sandboxAPI.SetAuditor(auditor)
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

	// Start the controlled DNS resolver (node-level Runnable) when enabled. It
	// binds the resolver IP on port 53 (udp + tcp); a listen failure is fatal
	// because name-based egress would silently not work otherwise.
	if dnsProxyServer != nil {
		dnsAddr := net.JoinHostPort(dnsResolver, "53")
		go func() {
			fmt.Printf("forkd: DNS resolver on %s\n", dnsAddr)
			if err := dnsProxyServer.ListenAndServe(dnsAddr); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: DNS resolver error: %v\n", err)
				os.Exit(1)
			}
		}()
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := dnsProxyServer.Shutdown(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "forkd: DNS resolver shutdown: %v\n", err)
			}
		}()
	}

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

// defaultDNSResolverIP is the node-wide resolver address used when DNS egress
// is enabled and --dns-resolver is not set. It is an IPv4 link-local address:
// the host binds the controlled resolver here and every sandbox chain allows
// udp/tcp 53 to it, so a single address serves every per-/30 sandbox (the
// proxy attributes each query by the source guest IP, not by the resolver IP).
// The operator must ensure this address is reachable from the sandbox subnet
// (for example bound on the host so the per-sandbox gateway routes to it).
const defaultDNSResolverIP = "169.254.1.1"

// dnsProxyTTLFloor is the minimum lifetime of a pinned (ip . port) element. A
// very short upstream TTL is raised to this floor so a pin does not expire
// before the guest opens its connection.
const dnsProxyTTLFloor = 30 * time.Second

// buildDNSProxy constructs the controlled resolver: it pins resolved IPs into
// each sandbox's nft set via an exec-based nft runner, attributes queries to a
// tap through the allocator's guest-IP lookup, and forwards allowed queries to
// the resolved upstream.
func buildDNSProxy(registry *dnsproxy.Registry, alloc *netconf.Allocator, resolverIP, upstream string) *dnsproxy.Server {
	pinner := dnsproxy.NewNftPinner(func(argv []string) error {
		if len(argv) == 0 {
			return fmt.Errorf("empty nft command")
		}
		cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // fixed nft argv built from validated addresses/ports
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", argv[0], err, string(out))
		}
		return nil
	})
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return dnsproxy.NewServer(registry, pinner, resolvedUpstream(upstream), dnsProxyTTLFloor, alloc.TapForGuestIP, logger)
}

// resolvedUpstream returns the upstream resolver address for the proxy. An
// explicit --dns-upstream wins; otherwise the first nameserver from
// /etc/resolv.conf is used, falling back to 1.1.1.1:53. A nameserver without a
// port gets :53 appended.
func resolvedUpstream(upstream string) string {
	if upstream != "" {
		return upstream
	}
	if ns := firstResolvConfNameserver("/etc/resolv.conf"); ns != "" {
		return ns
	}
	return "1.1.1.1:53"
}

// firstResolvConfNameserver returns the first `nameserver <ip>` from path as a
// host:port (appending :53 when no port is present), or "" when none is found
// or the file cannot be read.
func firstResolvConfNameserver(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			ns := fields[1]
			if _, _, err := net.SplitHostPort(ns); err != nil {
				ns = net.JoinHostPort(ns, "53")
			}
			return ns
		}
	}
	return ""
}
