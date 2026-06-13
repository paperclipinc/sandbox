package snapcompat

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/paperclipinc/mitos/internal/cas"
)

// Runner runs an argv and returns its combined stdout. It is injected so
// DetectEnvironment is unit-testable on darwin without a real Firecracker
// binary or a Linux kernel.
type Runner func(argv []string) ([]byte, error)

// CPUInfoReader returns the raw contents of the CPU model source (/proc/cpuinfo
// on Linux). Injected for the same reason as Runner.
type CPUInfoReader func() (string, error)

// ExecRunner is the production Runner: it execs argv[0] with argv[1:] and
// returns combined output.
func ExecRunner(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv built from the configured firecracker path and fixed flags
	return cmd.CombinedOutput()
}

// ProcCPUInfoReader is the production CPUInfoReader: it reads /proc/cpuinfo.
func ProcCPUInfoReader() (string, error) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", fmt.Errorf("read /proc/cpuinfo: %w", err)
	}
	return string(b), nil
}

// DetectEnvironment captures the host environment that the snapshot
// compatibility contract checks against: the Firecracker (VMM) version, the
// host CPU model, and the kernel version. FormatVersions is set to the single
// version this build can restore. The runner and cpuinfo reader are injected so
// the detection logic is testable off-Linux.
//
// VMM version comes from "<firecrackerPath> --version"; CPU model from the first
// "model name" line of cpuinfo; kernel version from "uname -r" via the runner.
// A failure to detect any field is surfaced (not silently swallowed) so the
// caller can decide whether to refuse to start rather than running with an
// unknown environment.
func DetectEnvironment(firecrackerPath string, runner Runner, cpuinfo CPUInfoReader) (Environment, error) {
	verOut, err := runner([]string{firecrackerPath, "--version"})
	if err != nil {
		return Environment{}, fmt.Errorf("run %s --version: %w", firecrackerPath, err)
	}
	vmm := parseFirecrackerVersion(string(verOut))
	if vmm == "" {
		return Environment{}, fmt.Errorf("could not parse Firecracker version from %q", strings.TrimSpace(string(verOut)))
	}

	info, err := cpuinfo()
	if err != nil {
		return Environment{}, fmt.Errorf("read CPU info: %w", err)
	}
	cpu := parseCPUModel(info)
	if cpu == "" {
		return Environment{}, fmt.Errorf("could not find CPU model name in CPU info")
	}

	unameOut, err := runner([]string{"uname", "-r"})
	if err != nil {
		return Environment{}, fmt.Errorf("run uname -r: %w", err)
	}
	kernel := strings.TrimSpace(string(unameOut))
	if kernel == "" {
		return Environment{}, fmt.Errorf("uname -r returned no kernel version")
	}

	return Environment{
		FormatVersions: []int{cas.CurrentSnapshotFormatVersion},
		VMMVersion:     vmm,
		CPUModel:       cpu,
		KernelVersion:  kernel,
	}, nil
}

// parseFirecrackerVersion extracts the version token from "firecracker
// --version" output, e.g. "Firecracker v1.15.0" -> "v1.15.0". It returns the
// first whitespace-separated token that starts with a digit or a 'v' followed by
// a digit, so it tolerates extra build-info lines.
func parseFirecrackerVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		for _, tok := range strings.Fields(line) {
			t := strings.TrimSpace(tok)
			if t == "" {
				continue
			}
			if t[0] >= '0' && t[0] <= '9' {
				return t
			}
			if (t[0] == 'v' || t[0] == 'V') && len(t) > 1 && t[1] >= '0' && t[1] <= '9' {
				return t
			}
		}
	}
	return ""
}

// parseCPUModel returns the value of the first "model name" line in cpuinfo.
func parseCPUModel(info string) string {
	sc := bufio.NewScanner(strings.NewReader(info))
	for sc.Scan() {
		line := sc.Text()
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// ConfigHash returns a sha256 hex over a canonical encoding of the microvm
// machine config that the snapshot was captured under. It binds a snapshot to
// the vcpu count, memory size, and kernel/rootfs identity it was built with, so
// the value is stable across processes given the same inputs (it is part of the
// content-addressed manifest). The encoding is a fixed-order newline-joined
// key=value list so two equal configs always hash identically.
func ConfigHash(vcpus, memMiB int, kernelIdentity, rootfsIdentity string) string {
	canonical := fmt.Sprintf("vcpus=%d\nmem_mib=%d\nkernel=%s\nrootfs=%s\n",
		vcpus, memMiB, kernelIdentity, rootfsIdentity)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}
