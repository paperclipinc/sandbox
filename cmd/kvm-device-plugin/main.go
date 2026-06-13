// Command kvm-device-plugin is a Kubernetes device plugin that advertises the
// KVM device (mitos.run/kvm) to the kubelet and injects /dev/kvm (and
// /dev/net/tun) into containers that request it.
//
// It lets husk pods get /dev/kvm as a SCHEDULED resource instead of running
// privileged: true. A pod requests mitos.run/kvm like any extended resource;
// the scheduler only places it on a node whose plugin advertised healthy
// capacity (a node with no /dev/kvm advertises zero and never gets the pod),
// and the plugin injects the device node on Allocate. This is the
// PSA-restricted path husk pods need, with the device exception documented.
//
// It runs as a DaemonSet on every node and reflects scheduler truth: where
// /dev/kvm exists it advertises --device-count slots, elsewhere zero. All
// lifecycle logging goes to stderr.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/paperclipinc/mitos/internal/deviceplugin"
)

// defaultKVMDevicePath is the default container path the plugin stat(2)s to
// decide whether KVM is present. The DaemonSet mounts the host /dev read-only
// at /host-dev (NOT at the container's own /dev: mounting a read-only /dev over
// the container root shadows the kubelet-created /dev/termination-log and makes
// the container fail to start), so the host /dev/kvm is visible at
// /host-dev/kvm. Overridable with --kvm-device-path.
const defaultKVMDevicePath = "/host-dev/kvm"

// defaultDeviceCount is the number of synthetic slots advertised when /dev/kvm
// is present and --device-count is left at 0 (auto). /dev/kvm is shareable, so
// the count is a soft concurrency cap on husk pods per node, not a physical
// device count.
const defaultDeviceCount = 100

func main() {
	var (
		resourceName  string
		deviceCount   int
		devicePaths   string
		kubeletDir    string
		kvmDevicePath string
	)
	flag.StringVar(&resourceName, "resource-name", "mitos.run/kvm", "Extended resource name the plugin advertises; pods request it under resources.limits")
	flag.IntVar(&deviceCount, "device-count", 0, "Number of synthetic device slots to advertise when /dev/kvm is present; 0 means auto (a sane default). /dev/kvm is shareable, so this is a soft per-node concurrency cap, not a physical device count")
	flag.StringVar(&devicePaths, "device-paths", "/dev/kvm,/dev/net/tun", "Comma-separated host device nodes injected into a requesting container on Allocate (each at the same container path, rw)")
	flag.StringVar(&kubeletDir, "kubelet-dir", "/var/lib/kubelet/device-plugins", "Kubelet device-plugins directory: the plugin serves its socket here and dials the kubelet registry socket (kubelet.sock) here")
	flag.StringVar(&kvmDevicePath, "kvm-device-path", defaultKVMDevicePath, "Container path the plugin stat(2)s to decide whether KVM is present; the DaemonSet mounts the host /dev read-only here (not over the container /dev)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// kvmPresent probes the host /dev/kvm (visible at kvmDevicePath via the
	// read-only /dev mount) at each ListAndWatch call. A node without it
	// advertises zero (honest scheduler truth).
	kvmPresent := func() bool {
		_, err := os.Stat(kvmDevicePath)
		return err == nil
	}

	if deviceCount <= 0 {
		deviceCount = defaultDeviceCount
	}

	paths := splitPaths(devicePaths)
	if len(paths) == 0 {
		logger.Error("device plugin: no --device-paths configured")
		os.Exit(1)
	}

	plugin := deviceplugin.NewPlugin(resourceName, deviceCount, paths, kvmPresent)
	registrar := deviceplugin.NewRegistrar(plugin, kubeletDir, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("kvm device plugin starting",
		"resource", resourceName,
		"device_count", deviceCount,
		"device_paths", strings.Join(paths, ","),
		"kubelet_dir", kubeletDir,
		"kvm_present", kvmPresent(),
	)

	if err := registrar.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("device plugin exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("kvm device plugin shut down")
}

// splitPaths splits a comma-separated path list, trimming whitespace and
// dropping empty entries.
func splitPaths(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
