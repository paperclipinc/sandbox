package snapcompat

import (
	"fmt"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
)

const cannedCPUInfo = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) CPU @ 2.20GHz
stepping	: 7
processor	: 1
model name	: Intel(R) Xeon(R) CPU @ 2.20GHz
`

func fakeRunner(version, kernel string) Runner {
	return func(argv []string) ([]byte, error) {
		switch {
		case len(argv) >= 2 && argv[1] == "--version":
			return []byte(version), nil
		case len(argv) >= 1 && argv[0] == "uname":
			return []byte(kernel + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected argv %v", argv)
		}
	}
}

func TestDetectEnvironmentParsesCannedInputs(t *testing.T) {
	runner := fakeRunner("Firecracker v1.15.0\n", "6.1.0-22-cloud-amd64")
	cpuinfo := func() (string, error) { return cannedCPUInfo, nil }

	env, err := DetectEnvironment("/usr/local/bin/firecracker", runner, cpuinfo)
	if err != nil {
		t.Fatalf("DetectEnvironment: %v", err)
	}
	if env.VMMVersion != "v1.15.0" {
		t.Fatalf("VMMVersion = %q, want v1.15.0", env.VMMVersion)
	}
	if env.CPUModel != "Intel(R) Xeon(R) CPU @ 2.20GHz" {
		t.Fatalf("CPUModel = %q", env.CPUModel)
	}
	if env.KernelVersion != "6.1.0-22-cloud-amd64" {
		t.Fatalf("KernelVersion = %q", env.KernelVersion)
	}
	if len(env.FormatVersions) != 1 || env.FormatVersions[0] != cas.CurrentSnapshotFormatVersion {
		t.Fatalf("FormatVersions = %v, want [%d]", env.FormatVersions, cas.CurrentSnapshotFormatVersion)
	}
}

func TestDetectEnvironmentVersionParseVariants(t *testing.T) {
	cpuinfo := func() (string, error) { return cannedCPUInfo, nil }
	for _, tc := range []struct{ out, want string }{
		{"Firecracker v1.15.0\n", "v1.15.0"},
		{"1.10.1\n", "1.10.1"},
		{"Firecracker v1.7.0-beta\nsome build info\n", "v1.7.0-beta"},
	} {
		env, err := DetectEnvironment("fc", fakeRunner(tc.out, "6.1.0"), cpuinfo)
		if err != nil {
			t.Fatalf("DetectEnvironment(%q): %v", tc.out, err)
		}
		if env.VMMVersion != tc.want {
			t.Fatalf("version %q parsed to %q, want %q", tc.out, env.VMMVersion, tc.want)
		}
	}
}

func TestDetectEnvironmentSurfacesVersionFailure(t *testing.T) {
	runner := func(argv []string) ([]byte, error) {
		return nil, fmt.Errorf("boom")
	}
	cpuinfo := func() (string, error) { return cannedCPUInfo, nil }
	if _, err := DetectEnvironment("fc", runner, cpuinfo); err == nil {
		t.Fatal("expected error when --version fails")
	}
}

func TestDetectEnvironmentSurfacesUnparseableVersion(t *testing.T) {
	runner := fakeRunner("no version here\n", "6.1.0")
	cpuinfo := func() (string, error) { return cannedCPUInfo, nil }
	if _, err := DetectEnvironment("fc", runner, cpuinfo); err == nil {
		t.Fatal("expected error when version is unparseable")
	}
}

func TestConfigHashStableAndSensitive(t *testing.T) {
	a := ConfigHash(1, 512, "vmlinux", "rootfs.ext4")
	b := ConfigHash(1, 512, "vmlinux", "rootfs.ext4")
	if a != b {
		t.Fatal("ConfigHash not stable for equal inputs")
	}
	if a == ConfigHash(2, 512, "vmlinux", "rootfs.ext4") {
		t.Fatal("ConfigHash insensitive to vcpus")
	}
	if a == ConfigHash(1, 1024, "vmlinux", "rootfs.ext4") {
		t.Fatal("ConfigHash insensitive to mem")
	}
	if a == ConfigHash(1, 512, "other", "rootfs.ext4") {
		t.Fatal("ConfigHash insensitive to kernel identity")
	}
}
