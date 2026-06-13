package main

import (
	"context"
	"flag"
	"os"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/eventfeed"
	"github.com/paperclipinc/mitos/internal/kms"
	"github.com/paperclipinc/mitos/internal/observability"
	resourceapi "k8s.io/apimachinery/pkg/api/resource"
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

// resolveRunMode picks the single active controller run path from the flags.
// husk pods is the pod-native default; --enable-raw-forkd selects the
// fork-per-claim fallback, and --mock forces it too (the dev mock overlay has no
// KVM, so a husk pod's dormant VMM cannot run). It returns the resolved
// EnableHuskPods (husk on) and a rawForkd marker for logging. Exactly one path
// is active: huskPods == !rawForkd.
func resolveRunMode(enableHuskPods, enableRawForkd, mockMode bool) (huskPods, rawForkd bool) {
	rawForkd = enableRawForkd || mockMode
	huskPods = enableHuskPods && !rawForkd
	return huskPods, rawForkd
}

func main() {
	var metricsAddr string
	var probeAddr string
	var mockMode bool
	var disablePKIBootstrap bool
	var otlpEndpoint string
	var maxPendingDuration time.Duration
	var enableHuskPods bool
	var enableRawForkd bool
	var huskStubImage string
	var huskControlPort int
	var huskDataDir string
	var huskMemoryHeadroom string
	var huskMemoryHeadroomPercent int
	var eventSinkURL string
	var kekFile string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required, for local dev with kind)")
	flag.BoolVar(&disablePKIBootstrap, "disable-pki-bootstrap", false, "Skip creating the control plane CA and TLS Secrets; forkd dialing is then UNAUTHENTICATED unless the cluster brings its own certs")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP gRPC endpoint (host:port) for OpenTelemetry trace export. Empty disables tracing (zero cost). Spans carry ids, counts, and timings only; never secret values")
	flag.BoolVar(&enableHuskPods, "enable-husk-pods", true, "Pod-native default (issue #18): each SandboxPool builds the template snapshot AND maintains a warm pool of pre-scheduled husk pods pinned to the snapshot-holding nodes; claims activate a dormant husk pod in place. This is the default; pass --enable-raw-forkd to fall back to the fork-per-claim path. Ignored when --enable-raw-forkd or --mock is set (both force raw-forkd).")
	flag.BoolVar(&enableRawForkd, "enable-raw-forkd", false, "Fallback run path: build the snapshot and fork per claim on a holder node (no husk pods). Off by default (the husk pod-native path runs). --mock implies this. husk-pods needs real KVM nodes; raw-forkd is the path the mock/dev overlay uses.")
	flag.StringVar(&huskStubImage, "husk-stub-image", "mitos-husk-stub:latest", "Container image that runs the dormant-VMM stub in a husk pod. Only used with --enable-husk-pods.")
	flag.IntVar(&huskControlPort, "husk-control-port", controller.HuskControlPort, "TCP port the husk stub serves the mTLS network control on; the controller dials podIP:port to activate a dormant husk pod. Only used with --enable-husk-pods.")
	flag.StringVar(&huskDataDir, "husk-data-dir", "/var/lib/mitos", "forkd data directory on the node; the husk pod's read-only snapshot hostPath is rooted here (<dir>/templates/<id>/snapshot). Only used with --enable-husk-pods.")
	flag.StringVar(&huskMemoryHeadroom, "husk-memory-headroom", "256Mi", "Fixed-floor memory headroom added on top of a husk pod's memory request to size its memory LIMIT (production-blocker #2, cap 1). The limit must exceed the request because the cgroup holds MORE than the guest RAM: the Firecracker VMM, the husk-stub, and copy-on-write dirty-page slack. The effective headroom is max(this floor, --husk-memory-headroom-percent% of the request), so a large VM gets proportional slack and a small VM gets at least this floor. A too-tight limit OOM-kills a normal VM and destroys the activate latency; raise this if pods are OOM-killed at their configured RAM. Only used with --enable-husk-pods.")
	flag.IntVar(&huskMemoryHeadroomPercent, "husk-memory-headroom-percent", 25, "Proportional memory headroom (percent of the memory request) for a husk pod's memory LIMIT, considered alongside --husk-memory-headroom; the larger of the two is used. Only used with --enable-husk-pods.")
	flag.DurationVar(&maxPendingDuration, "max-pending-duration", controller.DefaultMaxPendingDuration, "How long a claim may stay Pending for lack of node capacity before it fails with a capacity-exhaustion error. Scale out nodes or raise the overcommit factor to admit more sandboxes.")
	flag.StringVar(&eventSinkURL, "event-sink-url", "", "Optional operator webhook the controller POSTs the workspace revision change feed to as CloudEvents 1.0 (workspace.revision.created, sandbox.phase.changed). Empty disables the webhook (Kubernetes Events are still always recorded). The feed carries names, content digests, lineage, and phases only; never secret values. The URL is operator config, the same trust class as a git rendezvous remote (see docs/threat-model.md).")
	flag.StringVar(&kekFile, "kek-file", "", "Path to the 32-byte AES-256 KEK file (mounted from a Kubernetes Secret) used to WRAP each Encrypted template's per-template DEK (envelope encryption). REQUIRED when any reconciled template sets Encrypted: true; without it EnsureEncKey fails closed. The KEK is a secret value: it is never logged. Cloud KMS providers (AWS/GCP/Vault) are a documented follow-up.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := ctrl.Log.WithName("setup")

	shutdownTracing, err := observability.Setup(context.Background(), "mitos-controller", otlpEndpoint)
	if err != nil {
		logger.Error(err, "tracing setup failed")
		os.Exit(1)
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	// Resolve the single active run path. husk pods is the pod-native default;
	// --enable-raw-forkd selects the fork-per-claim fallback. --mock forces
	// raw-forkd: the dev mock overlay cannot really run a husk pod's dormant VMM
	// (it has no KVM), so mock implies the raw-forkd path the dev overlay uses.
	// forkd-the-builder runs regardless in both modes (it builds the snapshots).
	enableHuskPods, rawForkd := resolveRunMode(enableHuskPods, enableRawForkd, mockMode)
	if rawForkd {
		logger.Info("run path: raw-forkd (fork per claim); husk pods disabled", "reason-mock", mockMode, "reason-flag", enableRawForkd)
	} else {
		logger.Info("run path: husk pods (pod-native default); the pool builds the snapshot and maintains a warm husk pod pool. husk-pods requires real KVM nodes")
	}

	if mockMode {
		logger.Info("--mock: the controller discovers mock forkd instances via pod discovery and forces the raw-forkd run path (no husk pods, which need real KVM nodes)")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		// Leader election: the Deployment runs multiple replicas for HA, so EXACTLY
		// ONE may run the reconcilers at a time. Without it every replica reconciles
		// every object, racing on status writes (optimistic-lock "object has been
		// modified") AND each independently selecting + claiming dormant husk pods
		// for the same claim, which under a refilling warm pool becomes a runaway
		// that drains the pool. The lease lives in the controller's own namespace.
		LeaderElection:   true,
		LeaderElectionID: "mitos-controller-leader",
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}

	nodeRegistry := controller.NewNodeRegistry()

	// The revision change feed sink. Empty --event-sink-url builds a NopSink, so
	// only Kubernetes Events are recorded (always-on). A non-empty URL POSTs each
	// feed CloudEvent to the operator webhook, at-least-once with a dedupe id.
	eventSink := eventfeed.NewWebhookSink(eventSinkURL)
	if eventSinkURL == "" {
		logger.Info("revision change feed: webhook disabled (Kubernetes Events only); set --event-sink-url to enable the CloudEvents egress")
	} else {
		logger.Info("revision change feed: posting CloudEvents to the operator webhook", "sink", eventSinkURL)
	}

	// The peer token forkd accepts on its token-gated CAS surface. The controller
	// passes it in every PullTemplate so a deficit node can pull a template from a
	// holder; it must match forkd's --peer-token. Sourced from the environment
	// (not a flag) so it is never exposed in the process argv. Empty disables
	// distribution by pull (every node builds its own snapshot). A credential:
	// never logged.
	peerToken := os.Getenv("FORKD_PEER_TOKEN")

	poolControllerNamespace := os.Getenv("FORKD_NAMESPACE")
	if poolControllerNamespace == "" {
		poolControllerNamespace = "mitos"
	}

	// Build the envelope-encryption KMS from --kek-file. The KEK is loaded by
	// PATH (never a value in argv) and its bytes are never logged; only the
	// non-secret KEK id is. When --kek-file is empty no KMS is wired and
	// EnsureEncKey fails closed for any Encrypted template (a plaintext-only
	// deployment is unaffected). Cloud KMS providers are a documented follow-up.
	var encKMS kms.Wrapper
	if kekFile != "" {
		w, kerr := kms.LoadLocalKEKFromFile(kekFile)
		if kerr != nil {
			logger.Error(kerr, "load KEK file")
			os.Exit(1)
		}
		encKMS = w
		logger.Info("envelope encryption KMS loaded", "kekID", w.KEKID())
	}

	huskHeadroomQty, err := resourceapi.ParseQuantity(huskMemoryHeadroom)
	if err != nil {
		logger.Error(err, "invalid --husk-memory-headroom", "value", huskMemoryHeadroom)
		os.Exit(1)
	}

	if err := (&controller.SandboxPoolReconciler{
		Client:                    mgr.GetClient(),
		NodeRegistry:              nodeRegistry,
		PeerToken:                 peerToken,
		EnableHuskPods:            enableHuskPods,
		HuskStubImage:             huskStubImage,
		KVMResourceName:           "mitos.run/kvm",
		DataDir:                   huskDataDir,
		HuskMemoryHeadroom:        huskHeadroomQty,
		HuskMemoryHeadroomPercent: huskMemoryHeadroomPercent,
		HuskTLSSecretName:         controller.ForkdTLSSecretName,
		HuskCASecretName:          controller.CASecretName,
		ControllerNamespace:       poolControllerNamespace,
		KMS:                       encKMS,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "SandboxPool")
		os.Exit(1)
	}

	// The claim reconciler holds the husk fields. Its HuskTLS (the controller
	// client mTLS config used to dial a husk stub's network control) is the SAME
	// config EnsurePKI returns for forkd dialing; it is assigned below after
	// bootstrap, exactly like nodeRegistry.TLS.
	claimReconciler := &controller.SandboxClaimReconciler{
		Client:             mgr.GetClient(),
		APIReader:          mgr.GetAPIReader(),
		NodeRegistry:       nodeRegistry,
		MaxPendingDuration: maxPendingDuration,
		EnableHuskPods:     enableHuskPods,
		HuskControlPort:    huskControlPort,
		KMS:                encKMS,
		Feed: controller.NewEmitFeed(
			// record.EventRecorder (the v1 events API) is still supported; the v2
			// events API GetEventRecorder returns a different type with a different
			// signature, so migrating is a separate change. controller-runtime's own
			// tests carry the same nolint on this deprecation.
			mgr.GetEventRecorderFor("mitos-controller"), //nolint:staticcheck // v1 events API supported; v2 migration out of scope
			eventSink,
			nil,
		),
	}
	if err := claimReconciler.SetupWithManager(mgr); err != nil {
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

	// The Workspace reconciler (W4) is core, not behind the husk flag: it manages
	// the declarative Workspace model (the revision DAG, retention, lineage, and
	// head/revisions/resumable status). No data moves yet; hydrate/dehydrate is a
	// later W4 slice.
	if err := (&controller.WorkspaceReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "Workspace")
		os.Exit(1)
	}

	discoveryNamespace := os.Getenv("FORKD_NAMESPACE")
	if discoveryNamespace == "" {
		discoveryNamespace = "mitos"
	}
	discovery := &controller.ForkdDiscovery{
		Client:    mgr.GetClient(),
		Registry:  nodeRegistry,
		Namespace: discoveryNamespace,
	}

	if disablePKIBootstrap {
		logger.Info("PKI bootstrap disabled; forkd dialing will be insecure unless the cluster brings its own certs")
		if enableHuskPods {
			// The husk activate channel delivers tenant secrets and refuses to send
			// them over an unauthenticated channel (ActivateHuskPod rejects a nil
			// TLS config). Without PKI there is no controller client cert to present,
			// so husk activation would fail closed; make that explicit at startup.
			logger.Info("PKI bootstrap disabled with --enable-husk-pods: husk activation requires the controller mTLS client cert and will fail closed until certs are provided")
		}
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
		// The husk control channel uses the SAME controller client config.
		claimReconciler.HuskTLS = tlsConf
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
