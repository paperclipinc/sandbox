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

// SharedTableName returns the single shared nftables table that holds every
// sandbox's egress rules. All sandboxes share ONE table and ONE base chain so
// that adding or removing one sandbox never disturbs another's traffic.
func SharedTableName() string {
	return "agentrun_egress"
}

// BaseChainName returns the single base chain hooked on the forward path. It
// has policy accept and only dispatches by interface into per-sandbox regular
// chains; it never drops, so non-sandbox host forwarding is unaffected.
func BaseChainName() string {
	return "forward"
}

// DispatchMapName returns the verdict map keyed by interface name. The base
// chain looks up the inbound interface (the tap) in this map and jumps to that
// sandbox's regular chain. Adding/removing a sandbox is a single map element
// add/delete by key, with no rule handles to track.
func DispatchMapName() string {
	return "tapdispatch"
}

// SandboxChainName returns the per-sandbox regular chain name for a tap. The
// chain holds that sandbox's accepts and a final drop; because it is reached
// only via the dispatch jump for this tap, its drop is a verdict for this
// sandbox's packets only and cannot affect other sandboxes.
func SandboxChainName(tap string) string {
	return "sb_" + tap
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

// RenderSharedTable renders the idempotent skeleton every sandbox shares: one
// `inet` table, one base chain hooked on the forward path with policy ACCEPT,
// and an empty interface-keyed verdict map the base chain dispatches through.
//
// All statements are `add` of named objects, which nftables treats as
// idempotent: re-applying this against an existing table is a no-op and does
// NOT flush the table or its chains, so a second sandbox's Setup never
// disturbs the first sandbox's chain or dispatch element.
//
// The base chain never drops. Its policy is accept so unrelated host
// forwarding passes; sandbox isolation is enforced entirely by each sandbox's
// own regular chain (rendered by RenderSandboxChain), which ends in drop and
// is reached only via the per-tap dispatch jump. This is the fix for the
// cross-fork drop: there is no shared policy-drop base chain whose drop would
// be terminal for every tap on the forward hook.
func RenderSharedTable() string {
	table := SharedTableName()
	base := BaseChainName()
	dispatch := DispatchMapName()

	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %s\n", table)
	fmt.Fprintf(&b, "add chain inet %s %s { type filter hook forward priority 0 ; policy accept ; }\n", table, base)
	fmt.Fprintf(&b, "add map inet %s %s { type ifname : verdict ; }\n", table, dispatch)
	// The single dispatch rule sends traffic whose inbound interface (the tap)
	// is a known sandbox tap into that sandbox's regular chain. Interfaces not
	// in the map fall through to the accept policy untouched. Adding the same
	// rule twice would duplicate it, but a duplicate dispatch lookup is inert
	// (the map is the single source of truth), and Setup only applies this
	// skeleton before each sandbox add; the rule body is fixed so nft collapses
	// it. To stay strictly idempotent we flush only this base chain's rules
	// before re-adding the single dispatch rule, which is safe because the base
	// chain holds no per-sandbox state (all of that lives in the map and the
	// per-sandbox chains).
	fmt.Fprintf(&b, "flush chain inet %s %s\n", table, base)
	fmt.Fprintf(&b, "add rule inet %s %s iifname vmap @%s\n", table, base, dispatch)
	return b.String()
}

// RenderSandboxChain renders the add block for ONE sandbox: its regular chain
// `sb_<tap>` (no hook, no policy) plus the dispatch map element routing this
// tap into it. The block is applied with `nft -f -` after RenderSharedTable.
//
// The chain accepts established/related connections, each allowlisted
// destination IP:port, and DNS (udp/tcp 53) to resolverIP only, then ends in a
// terminal drop (EgressDeny) or accept (EgressAllow). Every accept keys on
// `ip saddr <guestIP>` as defense in depth: even though only this tap reaches
// the chain via the dispatch jump, the saddr check stops a guest from
// spoofing another sandbox's source address on its own tap.
//
// Because the drop/accept verdict is reached only through the per-tap jump, it
// applies to this sandbox's packets alone and cannot terminate another
// sandbox's allowed traffic. The output is deterministic for the same inputs.
func RenderSandboxChain(tap string, guestIP net.IP, policy v1alpha1.EgressPolicy, allow []HostPort, resolverIP net.IP) string {
	table := SharedTableName()
	chain := SandboxChainName(tap)
	dispatch := DispatchMapName()

	var b strings.Builder
	// Regular chain: no hook, no policy. A regular chain's final verdict is a
	// verdict for the matched packet, not a hook-wide default.
	fmt.Fprintf(&b, "add chain inet %s %s\n", table, chain)

	// Anti-spoof: pin the accepts to this sandbox's guest source IP.
	saddr := fmt.Sprintf("ip saddr %s", guestIP.String())

	fmt.Fprintf(&b, "add rule inet %s %s %s ct state established,related accept\n", table, chain, saddr)

	for _, hp := range allow {
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s tcp dport %d accept\n",
			table, chain, saddr, hp.IP.String(), hp.Port)
	}

	if resolverIP != nil {
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s udp dport 53 accept\n",
			table, chain, saddr, resolverIP.String())
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s tcp dport 53 accept\n",
			table, chain, saddr, resolverIP.String())
	}

	// Final verdict for this sandbox's packets: drop under EgressDeny, accept
	// under EgressAllow. Terminal within this regular chain for this packet
	// only, so it cannot affect other taps' chains.
	final := "drop"
	if policy == v1alpha1.EgressAllow {
		final = "accept"
	}
	fmt.Fprintf(&b, "add rule inet %s %s %s %s\n", table, chain, saddr, final)

	// Dispatch element: route this tap into the chain. Delete is by key on
	// teardown, so no rule handles are tracked.
	fmt.Fprintf(&b, "add element inet %s %s { %q : jump %s }\n", table, dispatch, tap, chain)
	return b.String()
}
