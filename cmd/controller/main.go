package main

import (
	"context"
	"flag"
	"os"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/controller"
	"github.com/paperclipinc/sandbox/internal/observability"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var mockMode bool
	var disablePKIBootstrap bool
	var otlpEndpoint string
	var maxPendingDuration time.Duration
	var enableHuskPods bool
	var huskStubImage string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required, for local dev with kind)")
	flag.BoolVar(&disablePKIBootstrap, "disable-pki-bootstrap", false, "Skip creating the control plane CA and TLS Secrets; forkd dialing is then UNAUTHENTICATED unless the cluster brings its own certs")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP gRPC endpoint (host:port) for OpenTelemetry trace export. Empty disables tracing (zero cost). Spans carry ids, counts, and timings only; never secret values")
	flag.BoolVar(&enableHuskPods, "enable-husk-pods", false, "Maintain a warm pool of pre-scheduled husk pods per SandboxPool instead of building node-local snapshots (issue #18, slice 1). Default false: the raw-forkd snapshot path is used.")
	flag.StringVar(&huskStubImage, "husk-stub-image", "agent-run-husk-stub:latest", "Container image that runs the dormant-VMM stub in a husk pod. Only used with --enable-husk-pods.")
	flag.DurationVar(&maxPendingDuration, "max-pending-duration", controller.DefaultMaxPendingDuration, "How long a claim may stay Pending for lack of node capacity before it fails with a capacity-exhaustion error. Scale out nodes or raise the overcommit factor to admit more sandboxes.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := ctrl.Log.WithName("setup")

	shutdownTracing, err := observability.Setup(context.Background(), "agentrun-controller", otlpEndpoint)
	if err != nil {
		logger.Error(err, "tracing setup failed")
		os.Exit(1)
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	if mockMode {
		logger.Info("--mock on the controller is a no-op: mock mode now lives in forkd (run `forkd --mock`); the controller discovers mock forkd instances via pod discovery")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}

	nodeRegistry := controller.NewNodeRegistry()

	// The peer token forkd accepts on its token-gated CAS surface. The controller
	// passes it in every PullTemplate so a deficit node can pull a template from a
	// holder; it must match forkd's --peer-token. Sourced from the environment
	// (not a flag) so it is never exposed in the process argv. Empty disables
	// distribution by pull (every node builds its own snapshot). A credential:
	// never logged.
	peerToken := os.Getenv("FORKD_PEER_TOKEN")

	if err := (&controller.SandboxPoolReconciler{
		Client:          mgr.GetClient(),
		NodeRegistry:    nodeRegistry,
		PeerToken:       peerToken,
		EnableHuskPods:  enableHuskPods,
		HuskStubImage:   huskStubImage,
		KVMResourceName: "agentrun.dev/kvm",
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "SandboxPool")
		os.Exit(1)
	}

	if err := (&controller.SandboxClaimReconciler{
		Client:             mgr.GetClient(),
		NodeRegistry:       nodeRegistry,
		MaxPendingDuration: maxPendingDuration,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "SandboxClaim")
		os.Exit(1)
	}

	if err := (&controller.SandboxForkReconciler{
		Client:       mgr.GetClient(),
		NodeRegistry: nodeRegistry,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "SandboxFork")
		os.Exit(1)
	}

	discoveryNamespace := os.Getenv("FORKD_NAMESPACE")
	if discoveryNamespace == "" {
		discoveryNamespace = "agent-run"
	}
	discovery := &controller.ForkdDiscovery{
		Client:    mgr.GetClient(),
		Registry:  nodeRegistry,
		Namespace: discoveryNamespace,
	}

	if disablePKIBootstrap {
		logger.Info("PKI bootstrap disabled; forkd dialing will be insecure unless the cluster brings its own certs")
	} else {
		// mgr.GetClient() is cache-backed and the cache only starts with
		// mgr.Start, so bootstrap uses a direct client. Failure is fatal:
		// the control plane must not silently fall back to insecure dials.
		bootstrapClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			logger.Error(err, "unable to create PKI bootstrap client")
			os.Exit(1)
		}
		tlsConf, err := controller.EnsurePKI(context.Background(), bootstrapClient, discoveryNamespace)
		if err != nil {
			logger.Error(err, "PKI bootstrap failed; refusing to start with unauthenticated forkd dialing (use --disable-pki-bootstrap to bring your own certs)")
			os.Exit(1)
		}
		nodeRegistry.TLS = tlsConf
		discovery.TLS = tlsConf
		logger.Info("PKI bootstrap complete; dialing forkd with mTLS", "namespace", discoveryNamespace)
	}

	if err := mgr.Add(discovery); err != nil {
		logger.Error(err, "unable to add forkd discovery")
		os.Exit(1)
	}

	if err := mgr.Add(&controller.GarbageCollector{
		Client:   mgr.GetClient(),
		Registry: nodeRegistry,
	}); err != nil {
		logger.Error(err, "unable to add garbage collector")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	logger.Info("starting controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "controller exited with error")
		os.Exit(1)
	}
}
