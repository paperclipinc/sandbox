package main

import "testing"

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
