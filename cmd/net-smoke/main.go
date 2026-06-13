//go:build linux

// Command net-smoke drives the REAL internal/network Linux Manager against a
// live nft + iproute2 on the KVM CI runner. It is the host-side counterpart to
// the guest egress proof and closes the darwin gap: the nftables dispatch model
// and the rendered ruleset are never exercised against a real nft on developer
// machines (darwin has no nft), so this binary runs the real Manager.Setup the
// same way forkd does in production.
//
// Subcommands:
//
//	validate                     install TWO sandbox identities (two taps, two
//	                             /30s, two allowlists) and assert with
//	                             `nft list ruleset` that both per-sandbox chains
//	                             exist and the dispatch verdict map keys both
//	                             taps. Proves real nft accepts the rendered
//	                             syntax AND the two-sandbox dispatch model
//	                             installs cleanly (cross-tap isolation).
//
//	setup-one --guest-ip ... --host-ip ... --tap ... --allow ip:port
//	                             install ONE sandbox identity (tap + host IP +
//	                             egress allowlist) for the explicit addresses the
//	                             guest-VM phase boots a Firecracker NIC behind.
//	                             Goes through the same Manager.Setup; no teardown
//	                             so the VM can run behind it.
//
//	teardown-one --tap ... --host-ip ...
//	                             remove the single identity created by setup-one.
//
// All values it prints (tap names, IPs, ports) are safe to log. Requires root
// (creates taps, edits nftables). Exits non-zero on any failure so the CI phase
// gates.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/dnsproxy"
	"github.com/paperclipinc/mitos/internal/netconf"
	"github.com/paperclipinc/mitos/internal/network"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: net-smoke <validate|setup-one|teardown-one> [flags]")
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = runValidate()
	case "setup-one":
		err = runSetupOne(os.Args[2:])
	case "teardown-one":
		err = runTeardownOne(os.Args[2:])
	case "setup-name-egress":
		err = runSetupNameEgress(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q (want validate|setup-one|teardown-one|setup-name-egress)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "net-smoke: %v\n", err)
		os.Exit(1)
	}
}

// runValidate installs two concurrent sandbox identities through the real
// Manager and asserts the live ruleset proves cross-tap isolation.
func runValidate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A small subnet is enough for two /30 blocks; the tap prefix keeps the
	// generated interface names well under the kernel's 15-char limit.
	alloc, err := netconf.NewAllocator("10.201.0.0/24", "nsmoke")
	if err != nil {
		return fmt.Errorf("build allocator: %w", err)
	}
	mgr := network.NewManager(network.Options{})

	idA, err := alloc.Acquire("smoke-a")
	if err != nil {
		return fmt.Errorf("acquire identity A: %w", err)
	}
	idB, err := alloc.Acquire("smoke-b")
	if err != nil {
		return fmt.Errorf("acquire identity B: %w", err)
	}
	if idA.TapName == idB.TapName {
		return fmt.Errorf("two sandboxes got the SAME tap %s: identity allocator is not distinct", idA.TapName)
	}
	fmt.Printf("net-smoke: A tap=%s host=%s guest=%s | B tap=%s host=%s guest=%s\n",
		idA.TapName, idA.HostIP, idA.GuestIP, idB.TapName, idB.HostIP, idB.GuestIP)

	// Two sandboxes with DISTINCT allowlists. Both deny everything else. The two
	// setups must coexist: B's setup must not flush or override A's chain, and
	// the dispatch map must end up keying both taps.
	allowA := []netconf.HostPort{{IP: net.ParseIP("10.201.250.10"), Port: 443}}
	allowB := []netconf.HostPort{{IP: net.ParseIP("10.201.250.20"), Port: 80}}

	defer func() {
		_ = mgr.Teardown(context.Background(), idA)
		_ = mgr.Teardown(context.Background(), idB)
	}()

	if err := mgr.Setup(ctx, idA, v1alpha1.EgressDeny, allowA, nil); err != nil {
		return fmt.Errorf("setup sandbox A (real nft rejected the ruleset?): %w", err)
	}
	if err := mgr.Setup(ctx, idB, v1alpha1.EgressDeny, allowB, nil); err != nil {
		return fmt.Errorf("setup sandbox B (second-sandbox install disturbed the first?): %w", err)
	}

	ruleset, err := nftListRuleset(ctx)
	if err != nil {
		return err
	}
	fmt.Println("=== nft list ruleset ===")
	fmt.Println(ruleset)
	fmt.Println("=== end ruleset ===")

	if err := validate(ruleset, idA, idB); err != nil {
		return err
	}
	fmt.Println("net-smoke: two-sandbox nftables install validated against real nft")
	return nil
}

// validate asserts the live ruleset contains both per-sandbox chains, both
// dispatch map elements (one per tap), the shared table + base chain, and each
// sandbox's distinct allowlisted destination. These are exactly the invariants
// that make cross-tap isolation hold: each tap dispatches into its OWN chain,
// whose terminal drop is a verdict for that tap's packets only.
func validate(ruleset string, idA, idB netconf.Identity) error {
	table := netconf.SharedTableName()
	must := []string{
		fmt.Sprintf("table inet %s", table),
		fmt.Sprintf("chain %s", netconf.BaseChainName()),
		fmt.Sprintf("chain %s", netconf.SandboxChainName(idA.TapName)),
		fmt.Sprintf("chain %s", netconf.SandboxChainName(idB.TapName)),
		idA.TapName,
		idB.TapName,
		"10.201.250.10",
		"10.201.250.20",
	}
	var missing []string
	for _, want := range must {
		if !strings.Contains(ruleset, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("live ruleset is missing expected fragments %v", missing)
	}

	// The dispatch map must map BOTH taps to their OWN chains. Require each tap's
	// jump-to-its-own-chain so a single shared chain (the cross-tap bug) cannot
	// pass.
	for _, id := range []netconf.Identity{idA, idB} {
		jump := fmt.Sprintf("jump %s", netconf.SandboxChainName(id.TapName))
		if !strings.Contains(ruleset, jump) {
			return fmt.Errorf("dispatch for tap %s does not jump to its own chain (%s missing); cross-tap isolation not proven", id.TapName, jump)
		}
	}
	fmt.Printf("net-smoke: both sandbox chains present, dispatch map keys both taps (%s, %s)\n", idA.TapName, idB.TapName)
	return nil
}

// runSetupOne installs a single sandbox identity for the explicit addresses the
// guest-VM egress phase boots a Firecracker NIC behind. The tap, host IP, guest
// IP, and one allow entry are passed as flags so the workflow controls exactly
// which destination is permitted; everything else is denied by the default-deny
// chain. It goes through the same Manager.Setup forkd uses, with host
// forwarding enabled so guest traffic actually traverses the FORWARD hook the
// allowlist filters.
func runSetupOne(args []string) error {
	fs := newFlagSet()
	tap := fs.String("tap")
	hostIP := fs.String("host-ip")
	guestIP := fs.String("guest-ip")
	guestMAC := fs.String("guest-mac")
	allow := fs.String("allow")
	if err := fs.parse(args); err != nil {
		return err
	}
	if *tap == "" || *hostIP == "" || *guestIP == "" || *allow == "" {
		return fmt.Errorf("setup-one requires --tap, --host-ip, --guest-ip, --allow")
	}
	hp, isName, err := netconf.ParseAllowEntry(*allow)
	if err != nil {
		return fmt.Errorf("parse --allow: %w", err)
	}
	if isName {
		return fmt.Errorf("--allow must be IP:port, not a name (%s)", *allow)
	}

	id := netconf.Identity{
		TapName:  *tap,
		GuestMAC: *guestMAC,
		HostIP:   net.ParseIP(*hostIP),
		GuestIP:  net.ParseIP(*guestIP),
	}
	if id.HostIP == nil || id.GuestIP == nil {
		return fmt.Errorf("host-ip/guest-ip must be valid IPv4 addresses")
	}

	// EnableForwarding so guest egress traverses the FORWARD hook the allowlist
	// filters; no masquerade (the destination is a host-reachable netns).
	mgr := network.NewManager(network.Options{EnableForwarding: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mgr.Setup(ctx, id, v1alpha1.EgressDeny, []netconf.HostPort{hp}, nil); err != nil {
		return fmt.Errorf("setup single identity: %w", err)
	}
	fmt.Printf("net-smoke: setup-one tap=%s host=%s guest=%s allow=%s:%d\n",
		id.TapName, id.HostIP, id.GuestIP, hp.IP, hp.Port)
	return nil
}

// runTeardownOne removes the single identity created by setup-one.
func runTeardownOne(args []string) error {
	fs := newFlagSet()
	tap := fs.String("tap")
	hostIP := fs.String("host-ip")
	if err := fs.parse(args); err != nil {
		return err
	}
	if *tap == "" {
		return fmt.Errorf("teardown-one requires --tap")
	}
	id := netconf.Identity{TapName: *tap, HostIP: net.ParseIP(*hostIP)}
	mgr := network.NewManager(network.Options{EnableForwarding: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mgr.Teardown(ctx, id); err != nil {
		return fmt.Errorf("teardown single identity: %w", err)
	}
	fmt.Printf("net-smoke: teardown-one tap=%s\n", id.TapName)
	return nil
}

// runSetupNameEgress installs a single sandbox identity whose egress is governed
// by NAME, then runs the controlled DNS resolver in the foreground so the
// name-egress KVM phase can boot a VM behind it and probe from inside the guest.
//
// It mirrors exactly what forkd does with --enable-dns-egress, but for one
// explicitly addressed sandbox:
//   - Manager.Setup is called WITH a resolver IP, so the rendered chain allows
//     udp/tcp 53 to that resolver and declares the per-sandbox dynamic
//     (ip . port) allow set. No static IP allow is installed: the allowlist is a
//     NAME (for example egress.test:8080), enforced only by the resolver pinning
//     the resolved address as it answers.
//   - A dnsproxy.Registry is populated with the guest's name allowlist and a
//     dnsproxy.Server is bound to the resolver IP. The proxy forwards allowed
//     queries to --upstream (the CI stub) and pins each resolved (ip . port)
//     into this guest's set via real nft, so the guest can reach exactly the
//     address it resolved for an allowlisted name and nothing else.
//
// The resolver IP must already be bound and reachable from the guest tap; the
// workflow binds it on the host before invoking this. This call BLOCKS in
// ListenAndServe, so the workflow runs it in the background.
func runSetupNameEgress(args []string) error {
	fs := newFlagSet()
	tap := fs.String("tap")
	hostIP := fs.String("host-ip")
	guestIP := fs.String("guest-ip")
	guestMAC := fs.String("guest-mac")
	resolverIPStr := fs.String("resolver-ip")
	upstream := fs.String("upstream")
	nameAllow := fs.String("name-allow")
	if err := fs.parse(args); err != nil {
		return err
	}
	if *tap == "" || *hostIP == "" || *guestIP == "" || *resolverIPStr == "" || *upstream == "" || *nameAllow == "" {
		return fmt.Errorf("setup-name-egress requires --tap, --host-ip, --guest-ip, --resolver-ip, --upstream, --name-allow")
	}

	resolverIP := net.ParseIP(*resolverIPStr)
	if resolverIP == nil {
		return fmt.Errorf("--resolver-ip must be a valid IP address")
	}

	// The allow entry must be a NAME, not an IP: the whole point of this phase is
	// that allowlisting is by name. A literal IP would be enforced statically and
	// would not prove the resolver path.
	if _, isName, err := netconf.ParseAllowEntry(*nameAllow); err != nil {
		return fmt.Errorf("parse --name-allow: %w", err)
	} else if !isName {
		return fmt.Errorf("--name-allow must be a NAME:port, not an IP:port (%s)", *nameAllow)
	}
	names, err := netconf.ParseNameAllowList([]string{*nameAllow})
	if err != nil {
		return fmt.Errorf("parse --name-allow: %w", err)
	}

	id := netconf.Identity{
		TapName:  *tap,
		GuestMAC: *guestMAC,
		HostIP:   net.ParseIP(*hostIP),
		GuestIP:  net.ParseIP(*guestIP),
	}
	if id.HostIP == nil || id.GuestIP == nil {
		return fmt.Errorf("host-ip/guest-ip must be valid IPv4 addresses")
	}

	// Setup with the resolver IP and NO static IP allows. The chain now allows
	// DNS to the resolver and consults the dynamic set; egress to any concrete
	// IP:port is allowed only once the proxy pins it for a resolved name.
	mgr := network.NewManager(network.Options{EnableForwarding: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mgr.Setup(ctx, id, v1alpha1.EgressDeny, nil, resolverIP); err != nil {
		return fmt.Errorf("setup name-egress identity: %w", err)
	}
	fmt.Printf("net-smoke: setup-name-egress tap=%s host=%s guest=%s resolver=%s name-allow=%s upstream=%s\n",
		id.TapName, id.HostIP, id.GuestIP, resolverIP, *nameAllow, *upstream)

	// Register the guest's name allowlist and build the controlled resolver. The
	// pinner runs real nft (same exec path forkd uses); tapFor maps this single
	// guest IP to its tap so the proxy pins into the right set.
	registry := dnsproxy.NewRegistry()
	registry.Register(id.GuestIP, names)
	pinner := dnsproxy.NewNftPinner(func(argv []string) error {
		if len(argv) == 0 {
			return fmt.Errorf("empty nft command")
		}
		out, runErr := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed nft argv from validated addresses/ports
		if runErr != nil {
			return fmt.Errorf("%s: %w: %s", argv[0], runErr, string(out))
		}
		return nil
	})
	tapFor := func(ip net.IP) string {
		if ip.Equal(id.GuestIP) {
			return id.TapName
		}
		return ""
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := dnsproxy.NewServer(registry, pinner, *upstream, 30*time.Second, tapFor, logger)

	addr := net.JoinHostPort(resolverIP.String(), "53")
	fmt.Printf("net-smoke: controlled resolver listening on %s (upstream %s)\n", addr, *upstream)
	if err := server.ListenAndServe(addr); err != nil {
		return fmt.Errorf("controlled resolver on %s: %w", addr, err)
	}
	return nil
}

func nftListRuleset(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "nft", "list", "ruleset").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nft list ruleset: %w: %s", err, string(out))
	}
	return string(out), nil
}

// flagSet is a tiny stdlib-flag wrapper so each subcommand can declare its own
// string flags without colliding on the global flag set.
type flagSet struct {
	vals map[string]*string
}

func newFlagSet() *flagSet { return &flagSet{vals: map[string]*string{}} }

func (f *flagSet) String(name string) *string {
	v := new(string)
	f.vals[name] = v
	return v
}

func (f *flagSet) parse(args []string) error {
	for i := 0; i < len(args); i++ {
		a := strings.TrimPrefix(args[i], "--")
		v, ok := f.vals[a]
		if !ok {
			return fmt.Errorf("unknown flag --%s", a)
		}
		if i+1 >= len(args) {
			return fmt.Errorf("flag --%s needs a value", a)
		}
		*v = args[i+1]
		i++
	}
	return nil
}
