package main

import (
	"strings"
	"testing"
)

func TestRenderLogsShowsStubConsoleAndGuestNote(t *testing.T) {
	out := renderLogs("alpha", podConsole{
		PodName: "web-husk-abc",
		Logs:    "husk-stub: prepared dormant VMM\nhusk-stub: ready\n",
		Found:   true,
	})
	if !strings.Contains(out, "web-husk-abc") {
		t.Errorf("stub console should name the husk pod: %q", out)
	}
	if !strings.Contains(out, "husk-stub: ready") {
		t.Errorf("stub console should include the pod log body: %q", out)
	}
	// The guest console note is always present, with the #18 boundary.
	if !strings.Contains(out, "guest console") || !strings.Contains(out, "#18") {
		t.Errorf("output should carry the guest console #18 note: %q", out)
	}
}

func TestRenderLogsNoHuskPodIsHandledGracefully(t *testing.T) {
	out := renderLogs("alpha", podConsole{Found: false})
	// No error: a plain statement that no husk pod backs the claim.
	if !strings.Contains(out, "no husk pod backs claim") {
		t.Errorf("absent husk pod should be stated plainly: %q", out)
	}
	if !strings.Contains(out, "guest console needs a running sandbox") {
		t.Errorf("guest console note should still appear: %q", out)
	}
}

func TestRenderLogsEmptyStubBody(t *testing.T) {
	out := renderLogs("alpha", podConsole{PodName: "p", Logs: "   ", Found: true})
	if !strings.Contains(out, "no stub console output yet") {
		t.Errorf("empty stub body should be reported, got %q", out)
	}
}
