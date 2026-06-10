package netconf

import (
	"net"
	"strings"
	"testing"

	"github.com/paperclipinc/sandbox/api/v1alpha1"
)

func TestParseAllowEntryIPPort(t *testing.T) {
	hp, isName, err := ParseAllowEntry("10.0.0.5:443")
	if err != nil {
		t.Fatalf("ParseAllowEntry: %v", err)
	}
	if isName {
		t.Error("expected isName=false for IP:port")
	}
	if !hp.IP.Equal(net.ParseIP("10.0.0.5")) {
		t.Errorf("IP = %v, want 10.0.0.5", hp.IP)
	}
	if hp.Port != 443 {
		t.Errorf("Port = %d, want 443", hp.Port)
	}
}

func TestParseAllowEntryName(t *testing.T) {
	_, isName, err := ParseAllowEntry("api.anthropic.com:443")
	if err != nil {
		t.Fatalf("ParseAllowEntry: %v", err)
	}
	if !isName {
		t.Error("expected isName=true for hostname:port")
	}
}

func TestParseAllowEntryInvalid(t *testing.T) {
	for _, s := range []string{"noport", "10.0.0.5:notaport", "10.0.0.5:70000", ":443", "host:"} {
		if _, _, err := ParseAllowEntry(s); err == nil {
			t.Errorf("ParseAllowEntry(%q) expected error, got nil", s)
		}
	}
}

func TestSplitAllowList(t *testing.T) {
	hps, skipped, err := SplitAllowList([]string{
		"10.0.0.5:443",
		"api.anthropic.com:443",
		"192.168.1.1:80",
	})
	if err != nil {
		t.Fatalf("SplitAllowList: %v", err)
	}
	if len(hps) != 2 {
		t.Errorf("enforceable = %d, want 2", len(hps))
	}
	if len(skipped) != 1 || skipped[0] != "api.anthropic.com:443" {
		t.Errorf("skipped = %v, want [api.anthropic.com:443]", skipped)
	}
}

func TestRenderEgressRulesetContents(t *testing.T) {
	allow := []HostPort{
		{IP: net.ParseIP("10.0.0.5"), Port: 443},
		{IP: net.ParseIP("192.168.1.10"), Port: 80},
	}
	out := RenderEgressRuleset("sbabcd1234", net.ParseIP("10.200.0.2"),
		v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1"))

	wantContains := []string{
		"sbabcd1234",          // scoped to the tap
		"ip saddr 10.200.0.2", // from the guest IP
		"ct state established,related accept",
		"ip daddr 10.0.0.5 tcp dport 443 accept",
		"ip daddr 192.168.1.10 tcp dport 80 accept",
		"ip daddr 10.200.0.1 udp dport 53 accept", // DNS to resolver only
		"ip daddr 10.200.0.1 tcp dport 53 accept",
		"policy drop", // default drop
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("ruleset missing %q\n---\n%s", w, out)
		}
	}
	// Exactly the two allowlisted accepts (count "dport" accepts for non-DNS).
	if got := strings.Count(out, "tcp dport 443 accept"); got != 1 {
		t.Errorf("expected exactly 1 accept for :443, got %d", got)
	}
	if got := strings.Count(out, "tcp dport 80 accept"); got != 1 {
		t.Errorf("expected exactly 1 accept for :80, got %d", got)
	}
}

func TestRenderEgressRulesetDeterministic(t *testing.T) {
	allow := []HostPort{
		{IP: net.ParseIP("10.0.0.5"), Port: 443},
		{IP: net.ParseIP("192.168.1.10"), Port: 80},
	}
	a := RenderEgressRuleset("sbx", net.ParseIP("10.200.0.2"), v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1"))
	b := RenderEgressRuleset("sbx", net.ParseIP("10.200.0.2"), v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1"))
	if a != b {
		t.Errorf("ruleset not deterministic:\n%s\n---\n%s", a, b)
	}
}

func TestRenderEgressRulesetNoResolverOmitsDNS(t *testing.T) {
	out := RenderEgressRuleset("sbx", net.ParseIP("10.200.0.2"), v1alpha1.EgressDeny, nil, nil)
	if strings.Contains(out, "dport 53") {
		t.Errorf("expected no DNS rule without a resolver IP\n%s", out)
	}
	if !strings.Contains(out, "policy drop") {
		t.Errorf("expected default drop\n%s", out)
	}
}
