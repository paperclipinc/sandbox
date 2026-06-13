package main

import (
	"flag"
	"fmt"
	"time"
)

const (
	// poolAnnotation is the facade bridge annotation (mirrors
	// internal/facade.PoolAnnotation) that binds an upstream Sandbox to one of
	// our mitos.run pools.
	poolAnnotation = "mitos.run/pool"

	// pollInterval is how often the harness polls the cluster for the bridged
	// claim transitioning.
	pollInterval = 50 * time.Millisecond
)

// config holds the harness flags.
type config struct {
	kubeconfig string
	namespace  string
	name       string
	pool       string
	image      string
	iterations int
	timeout    time.Duration
}

// parseConfig parses the harness flags. Defaults match the facade-conformance
// kind job (default pool, default namespace) so the harness runs against the
// same fixture the CI deploys.
func parseConfig(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("facade-bench", flag.ContinueOnError)
	fs.StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to the kubeconfig for the target cluster (required)")
	fs.StringVar(&cfg.namespace, "namespace", "default", "namespace to apply the Sandbox in")
	fs.StringVar(&cfg.name, "name", "facade-bench", "name of the Sandbox + bridged claim")
	fs.StringVar(&cfg.pool, "pool", "default", "mitos.run pool the Sandbox binds to (the mitos.run/pool bridge annotation)")
	fs.StringVar(&cfg.image, "image", "busybox:stable", "container image for the Sandbox podTemplate (the husk pool pins the real image; this only keeps the manifest valid)")
	fs.IntVar(&cfg.iterations, "iterations", 20, "number of pause/resume toggles to time")
	fs.DurationVar(&cfg.timeout, "timeout", 60*time.Second, "per-step timeout waiting for the bridged claim to transition")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.kubeconfig == "" {
		return config{}, fmt.Errorf("--kubeconfig is required")
	}
	if cfg.iterations < 1 {
		return config{}, fmt.Errorf("--iterations must be >= 1, got %d", cfg.iterations)
	}
	return cfg, nil
}
