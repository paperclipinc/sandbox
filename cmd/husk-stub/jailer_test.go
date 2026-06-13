package main

import (
	"testing"

	"github.com/paperclipinc/mitos/internal/firecracker"
)

func TestBuildHuskJailerConfigDisabled(t *testing.T) {
	// An empty jailer binary disables the jailer (the development direct-exec
	// path). The returned config must be the zero (disabled) value.
	cfg, err := buildHuskJailerConfig("", "/run/husk/jail", "64000-64999", 0)
	if err != nil {
		t.Fatalf("buildHuskJailerConfig disabled: %v", err)
	}
	if cfg.Enabled() {
		t.Fatal("empty jailer binary must yield a disabled JailerConfig")
	}
}

func TestBuildHuskJailerConfigRequiresRoot(t *testing.T) {
	// The jailer needs root (CAP_SYS_ADMIN/CHOWN/SETUID/SETGID/MKNOD) to build a
	// jail; refuse fail-closed when not root.
	_, err := buildHuskJailerConfig("/usr/local/bin/jailer", "/run/husk/jail", "64000-64999", 1000)
	if err == nil {
		t.Fatal("buildHuskJailerConfig accepted a non-root euid")
	}
}

func TestBuildHuskJailerConfigBadRangeFailsClosed(t *testing.T) {
	if _, err := buildHuskJailerConfig("/usr/local/bin/jailer", "/run/husk/jail", "0-10", 0); err == nil {
		t.Fatal("buildHuskJailerConfig accepted a uid range including 0 (root)")
	}
	if _, err := buildHuskJailerConfig("/usr/local/bin/jailer", "/run/husk/jail", "garbage", 0); err == nil {
		t.Fatal("buildHuskJailerConfig accepted a malformed uid range")
	}
}

func TestBuildHuskJailerConfigValid(t *testing.T) {
	cfg, err := buildHuskJailerConfig("/usr/local/bin/jailer", "/run/husk/jail", "64000-64999", 0)
	if err != nil {
		t.Fatalf("buildHuskJailerConfig valid: %v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("valid jailer config must be enabled")
	}
	if cfg.ChrootBaseDir != "/run/husk/jail" {
		t.Fatalf("ChrootBaseDir = %q, want /run/husk/jail", cfg.ChrootBaseDir)
	}
	if cfg.UIDRange != [2]uint32{64000, 64999} {
		t.Fatalf("UIDRange = %v, want [64000 64999]", cfg.UIDRange)
	}
}

func TestHuskVMConfigJailerWiring(t *testing.T) {
	// huskVMConfig assembles the VMConfig the stub launches, attaching the jailer
	// config and the kernel as a chroot file when jailed. With a jailer it must
	// set cfg.Jailer (enabled) and include the kernel in ChrootFiles; without one
	// it must leave both unset (direct exec).
	jailed := buildHuskVMConfigForTest(t, "/usr/local/bin/jailer", "/run/husk/jail", "64000-64999")
	if !jailed.Jailer.Enabled() {
		t.Fatal("expected jailed VMConfig to carry an enabled jailer")
	}
	foundKernel := false
	for _, f := range jailed.ChrootFiles {
		if f == "/var/lib/mitos/kernel/vmlinux" {
			foundKernel = true
		}
	}
	if !foundKernel {
		t.Fatalf("expected kernel in ChrootFiles, got %v", jailed.ChrootFiles)
	}

	direct := buildHuskVMConfigForTest(t, "", "/run/husk/jail", "64000-64999")
	if direct.Jailer.Enabled() {
		t.Fatal("expected direct-exec VMConfig to have a disabled jailer")
	}
	if len(direct.ChrootFiles) != 0 {
		t.Fatalf("expected no ChrootFiles for direct exec, got %v", direct.ChrootFiles)
	}
}

// buildHuskVMConfigForTest exercises huskVMConfig with a forced root euid so the
// jailer validation passes off-root in CI; it fatals on any build error.
func buildHuskVMConfigForTest(t *testing.T, jailerBin, chrootBase, uidRange string) firecracker.VMConfig {
	t.Helper()
	cfg, err := huskVMConfig(huskVMParams{
		firecrackerBin: "/usr/local/bin/firecracker",
		kernel:         "/var/lib/mitos/kernel/vmlinux",
		workdir:        "/run/husk/vm",
		vcpus:          1,
		memMiB:         512,
		jailerBin:      jailerBin,
		chrootBase:     chrootBase,
		uidRange:       uidRange,
		euid:           0,
	})
	if err != nil {
		t.Fatalf("huskVMConfig: %v", err)
	}
	return cfg
}
