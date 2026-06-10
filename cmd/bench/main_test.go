package main

import "testing"

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig([]string{"--template", "default"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modeForkExec {
		t.Errorf("mode = %q, want %q", cfg.mode, modeForkExec)
	}
	if cfg.iterations != 50 {
		t.Errorf("iterations = %d, want 50", cfg.iterations)
	}
	if cfg.warmup != 5 {
		t.Errorf("warmup = %d, want 5", cfg.warmup)
	}
	if cfg.template != "default" {
		t.Errorf("template = %q, want default", cfg.template)
	}
}

func TestParseConfigInvalidMode(t *testing.T) {
	_, err := parseConfig([]string{"--mode", "bogus", "--template", "default"})
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestParseConfigMissingTemplate(t *testing.T) {
	_, err := parseConfig([]string{"--mode", "fork-exec"})
	if err == nil {
		t.Fatal("expected error for missing template")
	}
}

func TestParseConfigExecRT(t *testing.T) {
	cfg, err := parseConfig([]string{"--mode", "exec-rt", "--template", "t", "--iterations", "10", "--warmup", "2"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.mode != modeExecRT {
		t.Errorf("mode = %q, want %q", cfg.mode, modeExecRT)
	}
	if cfg.iterations != 10 {
		t.Errorf("iterations = %d, want 10", cfg.iterations)
	}
}
