package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/paperclipinc/mitos/internal/daemon"
	"github.com/paperclipinc/mitos/internal/dnsproxy"
	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/netconf"
	"github.com/paperclipinc/mitos/internal/network"
	"github.com/paperclipinc/mitos/internal/observability"
	"github.com/paperclipinc/mitos/internal/pki"
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
		enableEncryption  bool
		auditLog          string
		otlpEndpoint      string
		memReserveBytes   int64
		casListen         string
	)
	// peerToken is read from the environment, NOT a flag: a flag is visible in
	// /proc/<pid>/cmdline, and the token is a credential. The controller already
	// reads the same FORKD_PEER_TOKEN env var, so the two sides match by config.
	peerToken := os.Getenv("FORKD_PEER_TOKEN")

	flag.StringVar(&listenAddr, "listen", ":9090", "gRPC listen address (controller communication)")
	flag.StringVar(&httpAddr, "http", ":9091", "HTTP listen address (metrics + sandbox exec/files API)")
	flag.StringVar(&dataDir, "data-dir", "/var/lib/mitos", "Data directory for snapshots and sandboxes")
	flag.StringVar(&firecrackerBin, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	flag.StringVar(&kernelPath, "kernel", "/var/lib/mitos/vmlinux", "Guest kernel path")
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
	flag.BoolVar(&enableEncryption, "enable-encryption", false, "Encrypt template snapshots at rest: each template is built inside a per-template LUKS2 container (requires cryptsetup) and crypto-shred at delete. Default false (plaintext snapshots on disk, exactly as before). KEY CUSTODY: the per-template encryption key is supplied by the controller over the mTLS gRPC request (CreateTemplate/Fork), held in forkd memory only for the lifetime of an open container, and never written to the node data disk. REQUIRES mTLS: this flag refuses to start unless the gRPC server is configured with --tls-cert/--tls-key/--tls-ca (and the controller runs PKI bootstrap), so the key is never sent over an insecure channel")
	flag.StringVar(&auditLog, "audit-log", "", "Structured audit log of exec and file operations. A file path, or '-'/'stderr' for stderr. Empty disables auditing. Records command strings, paths, and byte counts only; never file content or secret values")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP gRPC endpoint (host:port) for OpenTelemetry trace export. Empty disables tracing (zero cost). Spans carry ids, counts, and timings only; never secret values")
	flag.Int64Var(&memReserveBytes, "memory-reserve-bytes", 2*1024*1024*1024, "Bytes of host memory withheld from the schedulable budget for the OS and forkd itself. GetCapacity reports MemoryTotal = max(0, /proc/meminfo MemTotal - this reserve), the budget the controller bin-packs forks against. Default 2 GiB")
	flag.StringVar(&casListen, "cas-listen", ":9092", "Listen address for the DEDICATED token-gated TLS CAS listener used for peer template distribution. The CAS surface is served here, on its OWN port, NOT on the sandbox HTTP port (--http): the sandbox exec/files/metrics/healthz API keeps its existing scheme so SDK clients are unaffected. Effective only when CAS distribution is enabled (FORKD_PEER_TOKEN set together with mTLS). The controller derives this port to build each holder's CAS source URL")
	// peerToken (FORKD_PEER_TOKEN env) is the shared bearer token a peer forkd
	// (driven by the controller) must present to pull templates from this node's
	// content-addressed store. It is read from the ENVIRONMENT, not a flag, so it
	// is never exposed in /proc/<pid>/cmdline (the token is a credential and is
	// never logged). When set together with mTLS (--tls-cert/--tls-key/--tls-ca),
	// the token-gated CAS surface is served on the dedicated --cas-listen TLS
	// port and template distribution is enabled. REQUIRES mTLS: the surface is
	// served over TLS only so the token stays confidential; chunks are
	// digest-addressed so integrity is channel-independent, but the token gates
	// enumeration/pull. The controller must be configured with the SAME token
	// (it reads the same FORKD_PEER_TOKEN env var). SIMPLEST defensible model; a
	// per-pull minted token / forkd-peer mTLS identity is a follow-up.
	flag.Parse()

	shutdownTracing, err := observability.Setup(context.Background(), "mitos-forkd", otlpEndpoint)
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
	// Fail closed: at-rest encryption delivers the per-template key over the
	// gRPC request, so it must only run over an mTLS channel. The gRPC server is
	// secure only when all three TLS flags are set (see grpcServerOptions); any
	// other state leaves the channel insecure and would leak the key in
	// cleartext. Refuse to serve in that case.
	tlsConfigured := tlsCert != "" && tlsKey != "" && tlsCA != ""
	if err := requireTLSForEncryption(enableEncryption, tlsConfigured); err != nil {
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
	// reqKeyProvider is the request-scoped encryption key provider, set only when
	// --enable-encryption is on. The same instance is wired into the engine and
	// the daemon server so the handlers can hand the controller-delivered key to
	// the engine. Nil otherwise.
	var reqKeyProvider *fork.RequestKeyProvider
	// casServing, when set, enables the token-gated TLS CAS surface for peer
	// template distribution. Set only on the real engine when mTLS and a peer
	// token are both configured. Nil leaves the HTTP server plaintext as before.
	var casServing *daemon.CASServing

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
			CASDir:             casDir,
			AllowUnverified:    allowUnverified,
			AllowIncompatible:  allowIncompatible,
			AgentBinPath:       agentBin,
			BusyboxPath:        busyboxBin,
			EnableVolumes:      enableVolumes,
			MemoryReserveBytes: memReserveBytes,
		}
		// Template distribution: when mTLS and a peer token are configured, build
		// the HTTP client PullTemplate dials a holder forkd's CAS with. It presents
		// this forkd's own client identity (the same cert pair the gRPC server
		// uses) and trusts the control-plane CA, so the pull rides forkd-to-forkd
		// mTLS; the peer token is the additional gate the holder enforces. The
		// token, not the client, carries the credential.
		if tlsConfigured && peerToken != "" {
			pullClient, perr := pullHTTPClient(tlsCert, tlsKey, tlsCA)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "forkd: build template-pull client: %v\n", perr)
				os.Exit(1)
			}
			engineOpts.PullHTTPClient = pullClient
		}
		if enableVolumes {
			fmt.Println("forkd: per-fork volumes ENABLED")
		}
		if enableEncryption {
			engineOpts.EnableEncryption = true
			// PR2 key custody: the controller owns the per-template key (a
			// Kubernetes Secret in etcd) and delivers it on each mTLS RPC. The
			// node neither generates nor persists the key; the RequestKeyProvider
			// holds it only for the duration of a CreateTemplate/Fork call. The
			// SAME provider instance is wired into both the engine (it reads the
			// key via KeyFor) and the daemon server (the handlers stash the
			// request key via SetKey and forget it after).
			reqKeyProvider = fork.NewRequestKeyProvider()
			engineOpts.KeyProvider = reqKeyProvider
			fmt.Println("forkd: at-rest snapshot encryption ENABLED (PR2: key custody is the controller/etcd; the key arrives over the mTLS RPC, never generated or persisted on the node)")
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

		// Enable CAS peer distribution only when mTLS AND a peer token are set:
		// the surface serves digest-addressed bytes over TLS, gated by the shared
		// token. It is served on its OWN listener (--cas-listen), NOT the sandbox
		// HTTP port, so the sandbox API scheme is unchanged. The CAS listener
		// serves HTTPS using the same cert pair as the gRPC server.
		if tlsConfigured && peerToken != "" {
			httpTLS, terr := serverHTTPTLSConfig(tlsCert, tlsKey, tlsCA)
			if terr != nil {
				fmt.Fprintf(os.Stderr, "forkd: build CAS server TLS: %v\n", terr)
				os.Exit(1)
			}
			casServing = &daemon.CASServing{Store: real.CASStore(), Token: peerToken, TLS: httpTLS, Addr: casListen}
		} else if peerToken != "" {
			// A token without mTLS would serve it in cleartext: refuse to enable.
			fmt.Fprintln(os.Stderr, "forkd: FORKD_PEER_TOKEN set without mTLS (--tls-cert/--tls-key/--tls-ca); CAS distribution stays DISABLED so the token is never served in cleartext")
		}
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
	// Wire the request-scoped key provider so the gRPC handlers can hand the
	// controller-delivered encryption key to the engine for the duration of a
	// CreateTemplate/Fork call. Same instance the engine reads from.
	if reqKeyProvider != nil {
		server.SetKeyProvider(reqKeyProvider)
	}

	// Start gRPC server (controller communication)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forkd: failed to listen on %s: %v\n", listenAddr, err)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	daemon.RegisterForkDaemonServer(grpcServer, server)

	// Start HTTP server (metrics + sandbox exec/files API). When CAS
	// distribution is enabled, ServeHTTP also starts the dedicated token-gated
	// TLS CAS listener (--cas-listen) in its own goroutine; the sandbox HTTP API
	// scheme stays unchanged so SDK clients are unaffected.
	go daemon.ServeHTTP(httpAddr, engine, sandboxAPI, casServing)

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

// requireTLSForEncryption is the fail-closed guard for at-rest encryption: the
// controller delivers the per-template key over the gRPC request, so the
// channel must be mTLS. It returns a fatal error when encryption is enabled but
// the gRPC server is not TLS-configured (the --tls-cert/--tls-key/--tls-ca
// flags that drive the mTLS server are absent), and nil otherwise (encryption
// off, or encryption on with TLS configured). The error carries actionable
// remediation: configure the TLS flags and enable PKI bootstrap.
func requireTLSForEncryption(enableEnc, tlsConfigured bool) error {
	if enableEnc && !tlsConfigured {
		return fmt.Errorf("--enable-encryption requires mTLS: the controller delivers the encryption key over the gRPC request, which must not travel over an insecure channel; set --tls-cert, --tls-key, and --tls-ca (and run the controller with PKI bootstrap) or disable encryption")
	}
	return nil
}

// serverHTTPTLSConfig builds the TLS config the HTTP server uses for the
// token-gated CAS surface. It reuses forkd's own mTLS cert pair and requires a
// verified client certificate (a peer forkd or the controller), so CAS pulls
// ride forkd-to-forkd mTLS and the peer token is the additional gate. A bad
// pull token is rejected by the middleware; a peer without a CA-signed cert is
// rejected at the TLS handshake.
func serverHTTPTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	certPEM, keyPEM, caPEM, err := readTLSFiles(certPath, keyPath, caPath)
	if err != nil {
		return nil, err
	}
	return pki.ServerTLSConfig(certPEM, keyPEM, caPEM)
}

// pullHTTPClient builds the HTTP client PullTemplate dials a holder forkd's CAS
// with. It presents forkd's own client identity and trusts the control-plane
// CA. The pinned ServerName (pki.ServerName) means the holder's serving cert
// must carry that SAN; per-node SAN pinning is a follow-up tracked with the
// per-pull token work.
func pullHTTPClient(certPath, keyPath, caPath string) (*http.Client, error) {
	certPEM, keyPEM, caPEM, err := readTLSFiles(certPath, keyPath, caPath)
	if err != nil {
		return nil, err
	}
	clientTLS, err := pki.ClientTLSConfig(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientTLS},
		Timeout:   30 * time.Minute,
	}, nil
}

// readTLSFiles reads the three mTLS PEM files. It does not log their contents.
func readTLSFiles(certPath, keyPath, caPath string) (certPEM, keyPEM, caPEM []byte, err error) {
	if certPEM, err = os.ReadFile(certPath); err != nil {
		return nil, nil, nil, fmt.Errorf("read --tls-cert: %w", err)
	}
	if keyPEM, err = os.ReadFile(keyPath); err != nil {
		return nil, nil, nil, fmt.Errorf("read --tls-key: %w", err)
	}
	if caPEM, err = os.ReadFile(caPath); err != nil {
		return nil, nil, nil, fmt.Errorf("read --tls-ca: %w", err)
	}
	return certPEM, keyPEM, caPEM, nil
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
