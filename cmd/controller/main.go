package main

import (
	"flag"
	"os"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/controller"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
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

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required, for local dev with kind)")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := ctrl.Log.WithName("setup")

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

	if err := (&controller.SandboxPoolReconciler{
		Client:       mgr.GetClient(),
		NodeRegistry: nodeRegistry,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "SandboxPool")
		os.Exit(1)
	}

	if err := (&controller.SandboxClaimReconciler{
		Client:       mgr.GetClient(),
		NodeRegistry: nodeRegistry,
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
	if err := mgr.Add(&controller.ForkdDiscovery{
		Client:    mgr.GetClient(),
		Registry:  nodeRegistry,
		Namespace: discoveryNamespace,
	}); err != nil {
		logger.Error(err, "unable to add forkd discovery")
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
