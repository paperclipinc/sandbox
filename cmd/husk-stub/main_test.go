package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestServingArgsAreAllDefined is the regression guard for the husk fork
// regression: the controller's husk pod builder appends --forks-dir (and the
// other serving flags) to the husk-stub container args, but the flag was never
// defined here, so flag.Parse rejected it with "flag provided but not defined:
// -forks-dir" and the pod crash-looped. That broke BOTH warm-pool prepare and
// claim activation (no dormant pod ever came up).
//
// This test builds the husk-stub binary and runs it with the exact serving args
// the controller emits, asserting it gets PAST flag parsing (it then fails later
// for an unrelated reason, e.g. no firecracker binary, which is fine). A flag the
// controller emits that this binary does not define would fail flag parsing and
// is the bug we are guarding against.
func TestServingArgsAreAllDefined(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "husk-stub")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build husk-stub: %v\n%s", err, out)
	}

	// The serving args the controller's buildHuskPod emits for a husk pod (see
	// internal/controller/huskpod.go). The values are placeholders; only that the
	// FLAGS are recognized matters here.
	args := []string{
		"--firecracker", "/usr/local/bin/firecracker",
		"--kernel", "/var/lib/mitos/kernel/vmlinux",
		"--workdir", "/run/husk/vm",
		"--control-listen", ":9443",
		"--sandbox-listen", ":9091",
		"--tls-cert", "/etc/husk/tls/tls.crt",
		"--tls-key", "/etc/husk/tls/tls.key",
		"--tls-ca", "/etc/husk/ca/ca.crt",
		"--vm-id", "test-pod",
		"--manifest", "/var/lib/mitos/manifests/deadbeef",
		"--snapshot-dir", "/var/lib/mitos/snapshot",
		"--expected-digest", "deadbeef",
		"--rootfs-cow-dir", "/var/lib/mitos/husk-rootfs",
		"--template-rootfs", "/var/lib/mitos/templates/t/rootfs.ext4",
		"--forks-dir", "/var/lib/mitos/forks",
	}

	out, _ := exec.Command(bin, args...).CombinedOutput()
	// flag.ExitOnError prints this exact prefix and exits 2 when a flag the
	// controller emits is not defined here. The fix is to define every such flag.
	if strings.Contains(string(out), "flag provided but not defined") {
		t.Fatalf("husk-stub rejected a controller-emitted serving flag (the husk fork regression):\n%s", out)
	}
}
