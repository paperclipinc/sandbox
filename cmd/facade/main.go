// Command facade runs the agents.x-k8s.io conformance facade controller
// (issue #19). It is a SEPARATE binary from cmd/controller so the facade is
// opt-in and does not entangle our core controller: deploy it only when you
// want upstream agents.x-k8s.io/v1alpha1 Sandbox objects fulfilled on our fork
// engine.
//
// The facade watches upstream Sandbox objects and maps each onto our
// husk-backed run path (a SandboxClaim in our mitos.run group bound to one
// of our pools via the mitos.run/pool bridge annotation), mirroring the
// claim's readiness back into the Sandbox status. See
// docs/adr/0001-facade-and-naming.md and docs/facade-conformance.md.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"

	runv1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/facade"
)

var scheme = runtime.NewScheme()

func init() {
	// Register the upstream agents.x-k8s.io scheme (the core Sandbox we watch),
	// the upstream extensions.agents.x-k8s.io scheme (the SandboxTemplate /
	// SandboxWarmPool / SandboxClaim extension kinds we map), and our mitos.run
	// scheme (the template/pool/claim objects we create), plus core/v1.
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(extv1alpha1.AddToScheme(scheme))
	utilruntime.Must(runv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var defaultPool string
	var clusterDomain string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8083", "The address the probe endpoint binds to.")
	flag.StringVar(&defaultPool, "default-pool", "", "mitos.run pool an upstream Sandbox binds to when it carries no mitos.run/pool bridge annotation. Required to fulfil annotation-less Sandboxes; a Sandbox with neither the annotation nor a default reports NotReady with remediation text.")
	flag.StringVar(&clusterDomain, "cluster-domain", "cluster.local", "DNS cluster domain used to derive the upstream Sandbox status.serviceFQDN (<name>.<namespace>.svc.<domain>). Empty disables the derived FQDN.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		// Leader election off for now: the facade is a single opt-in instance.
		LeaderElection: false,
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&facade.SandboxReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		DefaultPool:   defaultPool,
		ClusterDomain: clusterDomain,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to set up facade Sandbox reconciler")
		os.Exit(1)
	}

	// The extension reconcilers map the upstream extensions.agents.x-k8s.io kinds
	// onto our mitos.run objects: their SandboxTemplate to our template and
	// their SandboxWarmPool to our pool. They run in the same opt-in facade
	// manager as the core Sandbox reconciler.
	if err := (&facade.SandboxTemplateReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to set up facade SandboxTemplate reconciler")
		os.Exit(1)
	}

	if err := (&facade.SandboxWarmPoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to set up facade SandboxWarmPool reconciler")
		os.Exit(1)
	}

	if err := (&facade.SandboxClaimReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		ClusterDomain: clusterDomain,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to set up facade SandboxClaim reconciler")
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

	logger.Info("starting agents.x-k8s.io facade controller", "default-pool", defaultPool, "cluster-domain", clusterDomain)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
