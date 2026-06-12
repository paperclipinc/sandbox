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

// TestRenderSharedTableShape asserts the shared table holds ONE base chain
// hooked forward with policy ACCEPT (so non-sandbox host forwarding is
// untouched) plus a verdict map keyed by interface for per-sandbox dispatch.
// There must be no policy-drop base chain: sandbox drops live only inside the
// per-sandbox regular chains.
func TestRenderSharedTableShape(t *testing.T) {
	out := RenderSharedTable()

	wantContains := []string{
		"add table inet " + SharedTableName(),
		"add chain inet " + SharedTableName() + " " + BaseChainName(),
		"type filter hook forward priority 0",
		"policy accept", // base chain never drops
		"add map inet " + SharedTableName() + " " + DispatchMapName(),
		"type ifname : verdict",              // dispatch by interface
		"iifname vmap @" + DispatchMapName(), // base chain dispatches
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("shared table missing %q\n---\n%s", w, out)
		}
	}
	if strings.Contains(out, "policy drop") {
		t.Errorf("shared base chain must not carry policy drop\n%s", out)
	}
}

// TestRenderSandboxChainContents asserts a per-sandbox regular chain (no hook,
// no policy) that ends in drop, plus the dispatch element that routes this
// tap's traffic into it. The drop is a verdict for THIS packet only (reached
// only via the per-tap jump), so it cannot affect other sandboxes.
func TestRenderSandboxChainContents(t *testing.T) {
	allow := []HostPort{
		{IP: net.ParseIP("10.0.0.5"), Port: 443},
		{IP: net.ParseIP("192.168.1.10"), Port: 80},
	}
	out := RenderSandboxChain("sbabcd1234", net.ParseIP("10.200.0.2"),
		v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1"))

	chain := SandboxChainName("sbabcd1234")
	wantContains := []string{
		"add chain inet " + SharedTableName() + " " + chain, // regular chain, no hook
		"ip saddr 10.200.0.2",                               // anti-spoof: from guest IP
		"ct state established,related accept",
		"ip daddr 10.0.0.5 tcp dport 443 accept",
		"ip daddr 192.168.1.10 tcp dport 80 accept",
		"ip daddr 10.200.0.1 udp dport 53 accept", // DNS to resolver only
		"ip daddr 10.200.0.1 tcp dport 53 accept",
		// dispatch element routes this tap into the chain.
		"add element inet " + SharedTableName() + " " + DispatchMapName(),
		`"sbabcd1234" : jump ` + chain,
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("sandbox chain missing %q\n---\n%s", w, out)
		}
	}
	// The regular chain must not be a hooked base chain and must not set policy.
	if strings.Contains(out, "type filter hook") {
		t.Errorf("per-sandbox chain must be a regular chain, not hooked\n%s", out)
	}
	if strings.Contains(out, "policy") {
		t.Errorf("per-sandbox chain must not set a policy\n%s", out)
	}
	// The final verdict in the chain is drop (terminal for this packet only).
	addRules := []string{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "add rule inet "+SharedTableName()+" "+chain) {
			addRules = append(addRules, line)
		}
	}
	if len(addRules) == 0 {
		t.Fatalf("no rules added to chain\n%s", out)
	}
	last := addRules[len(addRules)-1]
	if !strings.HasSuffix(last, " drop") {
		t.Errorf("final chain rule must be drop, got %q\n%s", last, out)
	}
	// Exactly the two allowlisted accepts.
	if got := strings.Count(out, "tcp dport 443 accept"); got != 1 {
		t.Errorf("expected exactly 1 accept for :443, got %d", got)
	}
	if got := strings.Count(out, "tcp dport 80 accept"); got != 1 {
		t.Errorf("expected exactly 1 accept for :80, got %d", got)
	}
}

// TestRenderSandboxChainsIndependent renders for TWO sandboxes and asserts each
// gets its own regular chain ending in drop and its own dispatch element, with
// no shared policy-drop base chain. This is the regression guard for the
// cross-fork drop: sandbox B's drop lives in chain sb_B reached only by tapB,
// so it can never terminate sandbox A's allowed traffic on tapA.
func TestRenderSandboxChainsIndependent(t *testing.T) {
	a := RenderSandboxChain("sbtapA", net.ParseIP("10.200.0.2"),
		v1alpha1.EgressDeny, []HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 443}}, net.ParseIP("10.200.0.1"))
	b := RenderSandboxChain("sbtapB", net.ParseIP("10.200.0.6"),
		v1alpha1.EgressDeny, []HostPort{{IP: net.ParseIP("10.0.0.9"), Port: 8080}}, net.ParseIP("10.200.0.5"))

	if SandboxChainName("sbtapA") == SandboxChainName("sbtapB") {
		t.Fatal("chain names collide")
	}
	if !strings.Contains(a, SandboxChainName("sbtapA")) || strings.Contains(a, SandboxChainName("sbtapB")) {
		t.Errorf("sandbox A render leaks into B's chain\n%s", a)
	}
	if !strings.Contains(b, SandboxChainName("sbtapB")) || strings.Contains(b, SandboxChainName("sptapA")) {
		t.Errorf("sandbox B render leaks into A's chain\n%s", b)
	}
	// Neither per-sandbox render touches the base chain policy.
	for _, out := range []string{a, b} {
		if strings.Contains(out, "policy drop") || strings.Contains(out, "hook forward") {
			t.Errorf("per-sandbox render must not redefine the base chain\n%s", out)
		}
	}
	// Each render's dispatch element keys on its own tap only.
	if !strings.Contains(a, `"sptapA"`) && !strings.Contains(a, `"sbtapA"`) {
		t.Errorf("A missing its dispatch element\n%s", a)
	}
	if strings.Contains(a, `"sbtapB"`) {
		t.Errorf("A must not dispatch B's tap\n%s", a)
	}
}

// TestRenderSandboxChainDynamicSet asserts the per-sandbox dynamic allow set is
// declared and that an accept rule matching (ip daddr . tcp dport) against it
// is present, placed after the static IP:port allows and the DNS-to-resolver
// rules but before the final verdict, and still saddr anti-spoof pinned.
func TestRenderSandboxChainDynamicSet(t *testing.T) {
	allow := []HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 443}}
	out := RenderSandboxChain("sbabcd1234", net.ParseIP("10.200.0.2"),
		v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1"))

	table := SharedTableName()
	chain := SandboxChainName("sbabcd1234")
	set := SandboxAllowSetName("sbabcd1234")

	setDecl := "add set inet " + table + " " + set + " { type ipv4_addr . inet_service ; flags timeout ; }"
	if !strings.Contains(out, setDecl) {
		t.Errorf("missing dynamic set declaration %q\n---\n%s", setDecl, out)
	}
	acceptRule := "ip saddr 10.200.0.2 ip daddr . tcp dport @" + set + " accept"
	if !strings.Contains(out, acceptRule) {
		t.Errorf("missing dynamic set accept rule %q\n---\n%s", acceptRule, out)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	idx := func(substr string) int {
		for i, l := range lines {
			if strings.Contains(l, substr) {
				return i
			}
		}
		return -1
	}
	rulePrefix := "add rule inet " + table + " " + chain
	staticAllow := idx("ip daddr 10.0.0.5 tcp dport 443 accept")
	dnsRule := idx("udp dport 53 accept")
	dynAccept := idx("@" + set + " accept")
	// The final verdict is the last add rule line for this chain.
	finalVerdict := -1
	for i, l := range lines {
		if strings.HasPrefix(l, rulePrefix) {
			finalVerdict = i
		}
	}
	if staticAllow == -1 || dnsRule == -1 || dynAccept == -1 || finalVerdict == -1 {
		t.Fatalf("could not locate ordered rules\n%s", out)
	}
	if !(dynAccept > staticAllow && dynAccept > dnsRule) {
		t.Errorf("dynamic accept must come after static allows and DNS rules\n%s", out)
	}
	if dynAccept >= finalVerdict {
		t.Errorf("dynamic accept must come before the final verdict\n%s", out)
	}
	// Every v4 accept (including the dynamic one) must be saddr-pinned. The v6
	// rules are deliberately not saddr-pinned: the guest has no v6 source address
	// to anti-spoof against, and the v6 default-deny is the boundary there.
	for _, l := range lines {
		if !strings.HasPrefix(l, rulePrefix) {
			continue
		}
		if strings.Contains(l, "ip6 ") || strings.Contains(l, "meta nfproto ipv6") {
			continue
		}
		if !strings.Contains(l, "ip saddr 10.200.0.2") {
			t.Errorf("v4 rule not saddr-pinned: %q", l)
		}
	}
}

// TestRenderSandboxChainV6DynamicSet asserts the per-sandbox chain declares a
// SEPARATE v6 dynamic allow set (ipv6_addr . inet_service) and accepts traffic
// whose (ip6 daddr . tcp dport) is present in it, and that the chain ends with
// a v6 default-deny so any unpinned v6 destination is dropped under EgressDeny.
func TestRenderSandboxChainV6DynamicSet(t *testing.T) {
	out := RenderSandboxChain("sbabcd1234", net.ParseIP("10.200.0.2"),
		v1alpha1.EgressDeny, nil, net.ParseIP("10.200.0.1"))

	table := SharedTableName()
	set6 := SandboxAllowSet6Name("sbabcd1234")

	setDecl := "add set inet " + table + " " + set6 + " { type ipv6_addr . inet_service ; flags timeout ; }"
	if !strings.Contains(out, setDecl) {
		t.Errorf("missing v6 dynamic set declaration %q\n---\n%s", setDecl, out)
	}
	acceptRule := "ip6 daddr . tcp dport @" + set6 + " accept"
	if !strings.Contains(out, acceptRule) {
		t.Errorf("missing v6 dynamic set accept rule %q\n---\n%s", acceptRule, out)
	}
	// v6 default-deny: an unpinned v6 destination is dropped.
	v6Drop := "meta nfproto ipv6 drop"
	if !strings.Contains(out, v6Drop) {
		t.Errorf("missing v6 default-deny %q\n---\n%s", v6Drop, out)
	}
}

// TestRenderSandboxChainV6AllowPolicy asserts that under EgressAllow the v6
// final verdict is accept, mirroring v4, so a permissive sandbox is not boxed
// in on v6 either.
func TestRenderSandboxChainV6AllowPolicy(t *testing.T) {
	out := RenderSandboxChain("sbx", net.ParseIP("10.200.0.2"), v1alpha1.EgressAllow, nil, nil)
	if !strings.Contains(out, "meta nfproto ipv6 accept") {
		t.Errorf("EgressAllow chain must end its v6 path in accept\n%s", out)
	}
}

func TestRenderSandboxChainDeterministic(t *testing.T) {
	allow := []HostPort{
		{IP: net.ParseIP("10.0.0.5"), Port: 443},
		{IP: net.ParseIP("192.168.1.10"), Port: 80},
	}
	a := RenderSandboxChain("sbx", net.ParseIP("10.200.0.2"), v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1"))
	b := RenderSandboxChain("sbx", net.ParseIP("10.200.0.2"), v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1"))
	if a != b {
		t.Errorf("render not deterministic:\n%s\n---\n%s", a, b)
	}
}

// lastChainRule returns the last `add rule ... <chain>` line in a render, used
// to assert the chain's final verdict regardless of trailing element lines.
func lastChainRule(t *testing.T, out, chain string) string {
	t.Helper()
	var last string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "add rule inet "+SharedTableName()+" "+chain) {
			last = line
		}
	}
	if last == "" {
		t.Fatalf("no rule found for chain %s\n%s", chain, out)
	}
	return last
}

func TestRenderSandboxChainNoResolverOmitsDNS(t *testing.T) {
	out := RenderSandboxChain("sbx", net.ParseIP("10.200.0.2"), v1alpha1.EgressDeny, nil, nil)
	if strings.Contains(out, "dport 53") {
		t.Errorf("expected no DNS rule without a resolver IP\n%s", out)
	}
	if !strings.HasSuffix(lastChainRule(t, out, SandboxChainName("sbx")), " drop") {
		t.Errorf("chain must still end in drop\n%s", out)
	}
}

func TestRenderSandboxChainAllowPolicy(t *testing.T) {
	// With EgressAllow the per-sandbox chain ends in accept, not drop, so a
	// permissive sandbox is not boxed in by its own chain.
	out := RenderSandboxChain("sbx", net.ParseIP("10.200.0.2"), v1alpha1.EgressAllow, nil, nil)
	if !strings.HasSuffix(lastChainRule(t, out, SandboxChainName("sbx")), " accept") {
		t.Errorf("EgressAllow chain must end in accept\n%s", out)
	}
}

func TestParseNameAllowList(t *testing.T) {
	names, err := ParseNameAllowList([]string{
		"10.0.0.5:443",         // IP:port, ignored (statically enforced)
		"API.Example.com:443",  // mixed case + dedup target
		"api.example.com.:443", // trailing dot + same port, deduped
		"api.example.com:8443", // second port for same name
		"docs.example.com:443", // distinct name
	})
	if err != nil {
		t.Fatalf("ParseNameAllowList: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %v, want 2 distinct names", names)
	}
	got := names["api.example.com"]
	if len(got) != 2 || got[0] != 443 || got[1] != 8443 {
		t.Errorf("api.example.com ports = %v, want [443 8443]", got)
	}
	if docs := names["docs.example.com"]; len(docs) != 1 || docs[0] != 443 {
		t.Errorf("docs.example.com ports = %v, want [443]", docs)
	}
}

func TestParseNameAllowListOnlyIPs(t *testing.T) {
	names, err := ParseNameAllowList([]string{"10.0.0.5:443", "192.0.2.1:80"})
	if err != nil {
		t.Fatalf("ParseNameAllowList: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected no name entries for an IP-only allowlist, got %v", names)
	}
}

func TestParseNameAllowListInvalid(t *testing.T) {
	for _, s := range []string{"api.example.com", "api.example.com:0", "api.example.com:bad", ":443"} {
		if _, err := ParseNameAllowList([]string{s}); err == nil {
			t.Errorf("ParseNameAllowList(%q) expected error, got nil", s)
		}
	}
}

// TestParseNameAllowListWildcardAccepted asserts a well-formed single-leading
// wildcard is parsed and keyed verbatim (lowercased, trailing dot stripped) so
// the registry can match it with the anchored suffix rule.
func TestParseNameAllowListWildcardAccepted(t *testing.T) {
	names, err := ParseNameAllowList([]string{
		"*.example.com:443",
		"*.Example.com:8443", // same key, second port
		"*.docs.example.com:443",
	})
	if err != nil {
		t.Fatalf("ParseNameAllowList: %v", err)
	}
	got := names["*.example.com"]
	if len(got) != 2 || got[0] != 443 || got[1] != 8443 {
		t.Errorf("*.example.com ports = %v, want [443 8443]", got)
	}
	if docs := names["*.docs.example.com"]; len(docs) != 1 || docs[0] != 443 {
		t.Errorf("*.docs.example.com ports = %v, want [443]", docs)
	}
}

// TestParseNameAllowListInvalidWildcard is the boundary-validation suite: a
// malformed wildcard must be REJECTED at the boundary, never silently treated
// as a literal name. A valid wildcard is exactly a single leading "*." plus a
// valid domain.
func TestParseNameAllowListInvalidWildcard(t *testing.T) {
	for _, s := range []string{
		"*:443",              // bare star, no domain
		"*.:443",             // star dot, empty domain
		"*foo.com:443",       // star not its own label
		"a.*.com:443",        // star not leading
		"**.com:443",         // double star in the leading label
		"*.*.com:443",        // two wildcard labels
		"*.example.*:443",    // trailing wildcard label
		"*..example.com:443", // empty label after the leading *.
	} {
		if _, err := ParseNameAllowList([]string{s}); err == nil {
			t.Errorf("ParseNameAllowList(%q) expected error for malformed wildcard, got nil", s)
		}
	}
}
