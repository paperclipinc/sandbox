package fork

import (
	"net"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/dnsproxy"
	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/netconf"
	"github.com/paperclipinc/mitos/internal/network"
)

// newNetEngine builds an Engine with networking wired but WITHOUT touching
// /dev/kvm or Firecracker, so the network helpers are unit testable. The
// FakeManager records Setup/Teardown; the Allocator hands out distinct
// identities.
func newNetEngine(t *testing.T) (*Engine, *network.FakeManager, *netconf.Allocator) {
	t.Helper()
	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	e := &Engine{
		netMgr:     fm,
		netAlloc:   alloc,
		resolverIP: net.ParseIP("10.200.0.1"),
	}
	return e, fm, alloc
}

func TestPrepareForkNetworkDisabled(t *testing.T) {
	// No manager/allocator: networking disabled, helper returns nil and no
	// override is produced regardless of opts.
	e := &Engine{}
	fn, err := e.prepareForkNetwork("sb1", ForkOpts{Network: &NetworkOpts{EgressPolicy: "deny"}})
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn != nil {
		t.Errorf("expected nil forkNetwork when disabled, got %+v", fn)
	}
}

func TestPrepareForkNetworkNoOpts(t *testing.T) {
	// Networking enabled but the request carries no NetworkOpts: no-op.
	e, fm, _ := newNetEngine(t)
	fn, err := e.prepareForkNetwork("sb1", ForkOpts{})
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn != nil {
		t.Errorf("expected nil forkNetwork without NetworkOpts, got %+v", fn)
	}
	if len(fm.SetupLog) != 0 {
		t.Errorf("Setup must not be called without NetworkOpts: %+v", fm.SetupLog)
	}
}

func TestPrepareForkNetworkSetupAndOverride(t *testing.T) {
	e, fm, _ := newNetEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		AllowList:    []string{"10.0.0.5:443", "api.example.com:443"},
	}}
	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn == nil {
		t.Fatal("expected a forkNetwork")
	}

	// Setup called exactly once with the parsed policy and the enforceable
	// (IP:port) allow entry only; the DNS-name entry is dropped.
	if len(fm.SetupLog) != 1 {
		t.Fatalf("expected 1 Setup call, got %d", len(fm.SetupLog))
	}
	call := fm.SetupLog[0]
	if call.Policy != v1alpha1.EgressDeny {
		t.Errorf("policy = %q, want deny", call.Policy)
	}
	if len(call.Allow) != 1 || !call.Allow[0].IP.Equal(net.ParseIP("10.0.0.5")) || call.Allow[0].Port != 443 {
		t.Errorf("allow = %+v, want [10.0.0.5:443]", call.Allow)
	}
	if !call.ResolverIP.Equal(net.ParseIP("10.200.0.1")) {
		t.Errorf("resolver = %v, want 10.200.0.1", call.ResolverIP)
	}

	// The NIC override remaps the baked iface id to this fork's tap, with the
	// identity's MAC and tap matching what Setup received.
	if len(fn.overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(fn.overrides))
	}
	ov := fn.overrides[0]
	if ov.IfaceID != firecracker.NetIfaceID {
		t.Errorf("override iface = %q, want %q", ov.IfaceID, firecracker.NetIfaceID)
	}
	if ov.HostDevName != call.Identity.TapName {
		t.Errorf("override tap %q != identity tap %q", ov.HostDevName, call.Identity.TapName)
	}

	// The guest network config carries the distinct guest IP + host gateway.
	if fn.guestNet == nil {
		t.Fatal("expected guest network config")
	}
	if fn.guestNet.GuestIP != call.Identity.GuestIP.String() {
		t.Errorf("guest IP %q != identity %v", fn.guestNet.GuestIP, call.Identity.GuestIP)
	}
	if fn.guestNet.GatewayIP != call.Identity.HostIP.String() {
		t.Errorf("gateway %q != host IP %v", fn.guestNet.GatewayIP, call.Identity.HostIP)
	}
	if fn.guestNet.PrefixLen != 30 {
		t.Errorf("prefix = %d, want 30", fn.guestNet.PrefixLen)
	}
}

func TestPrepareForkNetworkDistinctPerFork(t *testing.T) {
	e, _, _ := newNetEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{EgressPolicy: "deny"}}

	a, err := e.prepareForkNetwork("sb-a", opts)
	if err != nil {
		t.Fatalf("prepare a: %v", err)
	}
	b, err := e.prepareForkNetwork("sb-b", opts)
	if err != nil {
		t.Fatalf("prepare b: %v", err)
	}
	if a.identity.TapName == b.identity.TapName {
		t.Errorf("tap names collide: %q", a.identity.TapName)
	}
	if a.identity.GuestMAC == b.identity.GuestMAC {
		t.Errorf("MACs collide: %q", a.identity.GuestMAC)
	}
	if a.identity.GuestIP.Equal(b.identity.GuestIP) {
		t.Errorf("guest IPs collide: %v", a.identity.GuestIP)
	}
	if a.overrides[0].HostDevName == b.overrides[0].HostDevName {
		t.Errorf("override taps collide: %q", a.overrides[0].HostDevName)
	}
}

// TestForkRunningFailsClosedWithNetworking asserts that a live fork
// (ForkRunning) of a sandbox is rejected with an explicit, actionable error
// while per-VM netns is not yet available (#18). Restoring the source's baked
// NIC into a second live VM would collide on tap/MAC/IP, so we fail closed
// rather than silently break networking.
func TestForkRunningFailsClosedWithNetworking(t *testing.T) {
	e, _, _ := newNetEngine(t)
	e.sandboxes = map[string]*Sandbox{
		"src": {ID: "src"},
	}

	_, err := e.ForkRunning("src", "child", false)
	if err == nil {
		t.Fatal("expected ForkRunning to fail closed when networking is enabled")
	}
	if !strings.Contains(err.Error(), "not supported yet") || !strings.Contains(err.Error(), "#18") {
		t.Errorf("error = %q, want an explicit unsupported message referencing #18", err)
	}
}

// TestForkRunningUnknownSandboxWithNetworking keeps the not-found path intact:
// a missing source still reports not found, not the networking error.
func TestForkRunningUnknownSandboxWithNetworking(t *testing.T) {
	e, _, _ := newNetEngine(t)
	e.sandboxes = map[string]*Sandbox{}
	_, err := e.ForkRunning("nope", "child", false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestTeardownForkNetwork(t *testing.T) {
	// Use a /30 allocator (exactly one block) so Release is observable: the
	// slot is exhausted while held and free again after teardown.
	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/30", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	e := &Engine{netMgr: fm, netAlloc: alloc, resolverIP: net.ParseIP("10.200.0.1")}
	opts := ForkOpts{Network: &NetworkOpts{EgressPolicy: "deny"}}

	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The single /30 block is now held: a different sandbox cannot acquire.
	if _, err := alloc.Acquire("other"); err == nil {
		t.Fatal("expected exhaustion while sb1 holds the only block")
	}

	e.teardownForkNetwork("sb1", fn.identity)

	if len(fm.Teardowns) != 1 || fm.Teardowns[0].TapName != fn.identity.TapName {
		t.Errorf("Teardown not called for identity: %+v", fm.Teardowns)
	}
	// Release freed the block: a new sandbox can now acquire it.
	if _, err := alloc.Acquire("other"); err != nil {
		t.Fatalf("expected block free after teardown, got %v", err)
	}
}

// newDNSEngine builds an Engine with networking AND DNS-based name egress wired
// to a real in-memory dnsproxy.Registry, without touching KVM or Firecracker.
func newDNSEngine(t *testing.T) (*Engine, *network.FakeManager, *dnsproxy.Registry) {
	t.Helper()
	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	reg := dnsproxy.NewRegistry()
	e := &Engine{
		netMgr:         fm,
		netAlloc:       alloc,
		resolverIP:     net.ParseIP("169.254.1.1"),
		dnsRegistry:    reg,
		enableDNSEgres: true,
	}
	return e, fm, reg
}

func TestPrepareForkNetworkRegistersNames(t *testing.T) {
	e, _, reg := newDNSEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		AllowList:    []string{"10.0.0.5:443", "api.example.com:443", "api.example.com:8443"},
	}}
	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn == nil {
		t.Fatal("expected a forkNetwork")
	}

	// The sandbox's guest IP is registered with its name allowlist.
	ports, ok := reg.Lookup(fn.identity.GuestIP, "api.example.com")
	if !ok {
		t.Fatal("expected api.example.com registered for the guest IP")
	}
	if len(ports) != 2 {
		t.Errorf("api.example.com ports = %v, want 2 ports", ports)
	}
	// An unlisted name is not registered.
	if _, ok := reg.Lookup(fn.identity.GuestIP, "evil.example.com"); ok {
		t.Error("evil.example.com must not be registered")
	}

	// The resolver IP is delivered to the guest for resolv.conf.
	if fn.guestNet.ResolverIP != "169.254.1.1" {
		t.Errorf("guest resolver = %q, want 169.254.1.1", fn.guestNet.ResolverIP)
	}

	// Teardown deregisters the guest IP.
	e.teardownForkNetwork("sb1", fn.identity)
	if _, ok := reg.Lookup(fn.identity.GuestIP, "api.example.com"); ok {
		t.Error("expected guest deregistered after teardown")
	}
}

func TestPrepareForkNetworkIPOnlyDoesNotRegister(t *testing.T) {
	e, _, reg := newDNSEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		AllowList:    []string{"10.0.0.5:443"},
	}}
	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	// No name entries: nothing is registered for this guest.
	if _, ok := reg.Lookup(fn.identity.GuestIP, "api.example.com"); ok {
		t.Error("an IP-only allowlist must not register any name")
	}
	// The resolver IP is still delivered so the guest resolves through us.
	if fn.guestNet.ResolverIP != "169.254.1.1" {
		t.Errorf("guest resolver = %q, want 169.254.1.1", fn.guestNet.ResolverIP)
	}
}

func TestPrepareForkNetworkDNSDisabledNoRegister(t *testing.T) {
	// Networking on, DNS egress OFF: behavior is unchanged. No registry call,
	// resolverIP nil, and the guest gets no resolver to point at.
	e, _, _ := newNetEngine(t)
	e.resolverIP = nil
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		AllowList:    []string{"api.example.com:443"},
	}}
	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn.guestNet.ResolverIP != "" {
		t.Errorf("guest resolver = %q, want empty when DNS egress disabled", fn.guestNet.ResolverIP)
	}
}
