package netconf

import (
	"fmt"
	"net"
	"sort"
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

// SandboxAllowSetName returns the per-sandbox dynamic allow set name for a tap.
// The set holds (ipv4_addr . inet_service) elements with a timeout flag and is
// populated at runtime by the DNS proxy as it resolves allowlisted names. The
// per-sandbox chain accepts traffic whose (daddr . dport) is present in this
// set, so a resolved name's address is reachable only until its TTL expires.
// It is named the same way as SandboxChainName so a tap's chain and set share a
// stable, collision-free identity.
func SandboxAllowSetName(tap string) string {
	return "sb_" + tap + "_dyn"
}

// SandboxAllowSet6Name returns the per-sandbox dynamic IPv6 allow set name for a
// tap. It mirrors SandboxAllowSetName but holds (ipv6_addr . inet_service)
// elements: the DNS proxy pins resolved AAAA addresses here so a name's IPv6
// address is reachable for its TTL, just as A addresses are pinned into the v4
// set. It is named distinctly from the v4 set so the two coexist in one chain.
func SandboxAllowSet6Name(tap string) string {
	return "sb_" + tap + "_dyn6"
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

// ParseNameAllowList parses a raw allowlist into the name-based entries the DNS
// proxy enforces: a map from a lowercased DNS name to the sorted, de-duplicated
// set of TCP ports allowed for it. IP:port entries are ignored (they are
// enforced statically by the chain, not by the resolver). A malformed entry
// fails the whole call. The result is the map the dnsproxy Registry.Register
// takes; an empty map (no name entries) means the sandbox has no name egress.
//
// A wildcard name is permitted but is the egress boundary, so it is validated
// here: it must be exactly a single leading "*." followed by a valid domain.
// "*", "*.", "*foo.com", "a.*.com", "**.com", and any name with more than one
// "*" are REJECTED rather than silently treated as a literal name. A rejected
// wildcard fails the whole call with a clear error.
func ParseNameAllowList(entries []string) (map[string][]int, error) {
	names := make(map[string][]int)
	seen := make(map[string]map[int]bool)
	for _, e := range entries {
		host, portStr, splitErr := net.SplitHostPort(e)
		if splitErr != nil {
			return nil, fmt.Errorf("parse allow entry %q: %w", e, splitErr)
		}
		if host == "" {
			return nil, fmt.Errorf("parse allow entry %q: empty host", e)
		}
		port, perr := strconv.Atoi(portStr)
		if perr != nil {
			return nil, fmt.Errorf("parse allow entry %q: invalid port: %w", e, perr)
		}
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("parse allow entry %q: port %d out of range", e, port)
		}
		if net.ParseIP(host) != nil {
			// An IP:port entry: enforced statically by the chain, not the resolver.
			continue
		}
		if verr := validateNameAllowEntry(host); verr != nil {
			return nil, fmt.Errorf("parse allow entry %q: %w", e, verr)
		}
		key := strings.ToLower(strings.TrimSuffix(host, "."))
		if seen[key] == nil {
			seen[key] = make(map[int]bool)
		}
		if !seen[key][port] {
			seen[key][port] = true
			names[key] = append(names[key], port)
		}
	}
	for _, ports := range names {
		sort.Ints(ports)
	}
	return names, nil
}

// validateNameAllowEntry validates a DNS-name allow entry (host part, no port)
// at the egress boundary. A wildcard is the security-critical case: it must be
// exactly a single leading "*." plus a valid domain. The check runs on the
// trailing-dot-stripped host; a "*" anywhere except as the entire first label
// is rejected. A non-wildcard name must be a valid domain.
func validateNameAllowEntry(host string) error {
	name := strings.TrimSuffix(host, ".")
	if name == "" {
		return fmt.Errorf("empty name")
	}
	if strings.HasPrefix(name, "*.") {
		domain := strings.TrimPrefix(name, "*.")
		if strings.Contains(domain, "*") {
			return fmt.Errorf("wildcard must be a single leading %q label", "*.")
		}
		if !isValidDomain(domain) {
			return fmt.Errorf("wildcard %q must be %q followed by a valid domain", name, "*.")
		}
		return nil
	}
	if strings.Contains(name, "*") {
		return fmt.Errorf("a wildcard must be exactly a single leading %q label, got %q", "*.", name)
	}
	if !isValidDomain(name) {
		return fmt.Errorf("invalid domain %q", name)
	}
	return nil
}

// isValidDomain reports whether s is a syntactically valid DNS domain: at least
// two dot-separated labels, each label non-empty, no embedded "*", and only
// letters, digits, and hyphens with no leading or trailing hyphen per label.
// This is a deliberately conservative syntax check, not a registry lookup.
func isValidDomain(s string) bool {
	if s == "" {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !isValidLabel(label) {
			return false
		}
	}
	return true
}

// isValidLabel reports whether label is a valid DNS label: non-empty, no longer
// than 63 octets, alphanumeric or hyphen, and not starting or ending with a
// hyphen.
func isValidLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
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

	// Dynamic allow set: the DNS proxy pins (resolved ip . port) elements with a
	// timeout here as it answers allowlisted name queries. Declare the set, then
	// accept traffic whose (daddr . dport) is currently present in it. The set
	// is declared before its accept rule so the rule's reference resolves, and
	// the accept is placed after the static IP:port allows and the
	// DNS-to-resolver rules but before the final verdict, so a pinned name is
	// reachable only while its element lives and only on its allowed port. Like
	// every other accept it is saddr-pinned to stop source spoofing on this tap.
	set := SandboxAllowSetName(tap)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr . inet_service ; flags timeout ; }\n", table, set)
	fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr . tcp dport @%s accept\n", table, chain, saddr, set)

	// IPv6 dynamic allow set: the DNS proxy pins resolved AAAA addresses here
	// with a timeout, mirroring the v4 set. Accept v6 traffic whose
	// (ip6 daddr . tcp dport) is currently present. The accept is NOT saddr
	// pinned: the guest is assigned only a v4 /30 source identity today, so there
	// is no v6 source address to anti-spoof against; the v6 default-deny below is
	// the boundary. The set is declared before its accept so the reference
	// resolves.
	set6 := SandboxAllowSet6Name(tap)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr . inet_service ; flags timeout ; }\n", table, set6)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr . tcp dport @%s accept\n", table, chain, set6)

	// Final verdict for this sandbox's packets: drop under EgressDeny, accept
	// under EgressAllow. Terminal within this regular chain for this packet
	// only, so it cannot affect other taps' chains.
	final := "drop"
	if policy == v1alpha1.EgressAllow {
		final = "accept"
	}
	fmt.Fprintf(&b, "add rule inet %s %s %s %s\n", table, chain, saddr, final)
	// v6 final verdict, scoped to the v6 family so it cannot disturb the v4
	// saddr-pinned verdict above. Under EgressDeny this is the v6 default-deny:
	// any v6 destination not present in the v6 pin set is dropped, so v6 egress
	// is enforced and not silently permitted by fall-through to the base chain's
	// accept policy.
	fmt.Fprintf(&b, "add rule inet %s %s meta nfproto ipv6 %s\n", table, chain, final)

	// Dispatch element: route this tap into the chain. Delete is by key on
	// teardown, so no rule handles are tracked.
	fmt.Fprintf(&b, "add element inet %s %s { %q : jump %s }\n", table, dispatch, tap, chain)
	return b.String()
}
