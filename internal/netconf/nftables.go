package netconf

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/paperclipinc/sandbox/api/v1alpha1"
)

// HostPort is a destination IP and TCP port from an egress allowlist.
type HostPort struct {
	IP   net.IP
	Port int
}

// TableName returns the deterministic nftables table name scoping a tap's
// egress ruleset. One table per tap keeps teardown a single table delete.
func TableName(tap string) string {
	return "sbx_egress_" + tap
}

// ChainName returns the egress chain name within a tap's table. There is one
// egress chain per table, so the name is fixed.
func ChainName() string {
	return "egress"
}

// ParseAllowEntry parses a single allowlist entry of the form host:port. When
// host is a literal IPv4 address it returns the HostPort and isName=false.
// When host is a DNS name it returns isName=true (these cannot be enforced in
// PR1 without a controlled resolver, so the renderer omits them). A malformed
// entry (missing port, bad port, empty host) returns an error.
func ParseAllowEntry(s string) (hp HostPort, isName bool, err error) {
	host, portStr, splitErr := net.SplitHostPort(s)
	if splitErr != nil {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: %w", s, splitErr)
	}
	if host == "" {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: empty host", s)
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: invalid port: %w", s, perr)
	}
	if port < 1 || port > 65535 {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: port %d out of range", s, port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A DNS name: parsed but not enforceable in PR1.
		return HostPort{}, true, nil
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: only IPv4 destinations are supported", s)
	}
	return HostPort{IP: ip4, Port: port}, false, nil
}

// SplitAllowList parses a raw allowlist (e.g. NetworkPolicy.Allow) into the
// enforceable IP:port HostPorts and the list of skipped name-based entries
// (returned verbatim so forkd can log a clear warning). A malformed entry
// fails the whole call.
func SplitAllowList(entries []string) (enforceable []HostPort, skipped []string, err error) {
	for _, e := range entries {
		hp, isName, perr := ParseAllowEntry(e)
		if perr != nil {
			return nil, nil, perr
		}
		if isName {
			skipped = append(skipped, e)
			continue
		}
		enforceable = append(enforceable, hp)
	}
	return enforceable, skipped, nil
}

// RenderEgressRuleset renders an nft ruleset string for one tap's egress
// chain. The ruleset is a self-contained `add table` / `add chain` / `add
// rule` block scoped to the given tap so it can be applied with `nft -f -`
// and removed with a single table delete.
//
// The chain default policy is drop. Traffic originating from guestIP on the
// tap is matched; established/related connections are accepted, each
// allowlisted destination IP:port is accepted, DNS (udp/tcp 53) to resolverIP
// only is accepted, and everything else falls through to the drop policy.
//
// The output is deterministic for the same inputs (allowlist order is
// preserved as given by the caller). When policy is EgressAllow the chain
// still renders but with an accept fallthrough documented inline; PR1 callers
// pass EgressDeny for enforcement.
func RenderEgressRuleset(tap string, guestIP net.IP, policy v1alpha1.EgressPolicy, allow []HostPort, resolverIP net.IP) string {
	table := TableName(tap)
	chain := ChainName()

	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %s\n", table)

	// The chain hooks egress on the forward path; oifname is the tap so the
	// rules only govern traffic toward the guest's link. saddr pins the rules
	// to the guest IP so a spoofed source on the tap is not matched as allowed.
	policyKeyword := "drop"
	if policy == v1alpha1.EgressAllow {
		policyKeyword = "accept"
	}
	fmt.Fprintf(&b, "add chain ip %s %s { type filter hook forward priority 0 ; policy %s ; }\n", table, chain, policyKeyword)

	// Only consider traffic from this sandbox's guest IP on this tap.
	saddr := fmt.Sprintf("iifname %q ip saddr %s", tap, guestIP.String())

	fmt.Fprintf(&b, "add rule ip %s %s %s ct state established,related accept\n", table, chain, saddr)

	for _, hp := range allow {
		fmt.Fprintf(&b, "add rule ip %s %s %s ip daddr %s tcp dport %d accept\n",
			table, chain, saddr, hp.IP.String(), hp.Port)
	}

	if resolverIP != nil {
		fmt.Fprintf(&b, "add rule ip %s %s %s ip daddr %s udp dport 53 accept\n",
			table, chain, saddr, resolverIP.String())
		fmt.Fprintf(&b, "add rule ip %s %s %s ip daddr %s tcp dport 53 accept\n",
			table, chain, saddr, resolverIP.String())
	}

	// Everything else from the guest IP is dropped by the chain policy.
	return b.String()
}
