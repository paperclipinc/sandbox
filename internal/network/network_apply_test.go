package network

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/netconf"
)

type recordedCall struct {
	argv  []string
	stdin string
}

type recordingRunner struct {
	calls   []recordedCall
	failOn  string // substring of argv[0..] that should error
	failErr error
}

func (r *recordingRunner) run(_ context.Context, argv []string, stdin string) error {
	r.calls = append(r.calls, recordedCall{argv: argv, stdin: stdin})
	if r.failOn != "" && strings.Contains(strings.Join(argv, " "), r.failOn) {
		return r.failErr
	}
	return nil
}

func testIdentity() netconf.Identity {
	return netconf.Identity{
		TapName:  "sbtap0",
		GuestMAC: "02:11:22:33:44:55",
		HostIP:   net.ParseIP("10.200.0.1").To4(),
		GuestIP:  net.ParseIP("10.200.0.2").To4(),
	}
}

func TestSetupCommandOrder(t *testing.T) {
	rr := &recordingRunner{}
	id := testIdentity()
	allow := []netconf.HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 443}}
	resolver := net.ParseIP("10.200.0.1")

	err := setup(context.Background(), rr.run, func() error { return nil },
		id, v1alpha1.EgressDeny, allow, resolver, applyOptions{})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// tap create, addr add, link up, nft apply shared table, nft apply chain.
	if len(rr.calls) != 5 {
		t.Fatalf("expected 5 commands, got %d: %+v", len(rr.calls), rr.calls)
	}
	wantArgv := [][]string{
		netconf.TapAddArgs(id.TapName),
		netconf.AddrAddArgs(id.HostIP, id.TapName),
		netconf.LinkUpArgs(id.TapName),
		netconf.NftApplyArgs(),
		netconf.NftApplyArgs(),
	}
	for i, w := range wantArgv {
		if !reflect.DeepEqual(rr.calls[i].argv, w) {
			t.Errorf("call %d argv = %v, want %v", i, rr.calls[i].argv, w)
		}
	}
	// The first nft apply installs the idempotent shared table skeleton.
	if rr.calls[3].stdin != netconf.RenderSharedTable() {
		t.Errorf("shared-table stdin mismatch\ngot:\n%s\nwant:\n%s", rr.calls[3].stdin, netconf.RenderSharedTable())
	}
	// The second nft apply installs this sandbox's chain + dispatch element.
	wantChain := netconf.RenderSandboxChain(id.TapName, id.GuestIP, v1alpha1.EgressDeny, allow, resolver)
	if rr.calls[4].stdin != wantChain {
		t.Errorf("sandbox-chain stdin mismatch\ngot:\n%s\nwant:\n%s", rr.calls[4].stdin, wantChain)
	}
	// The tap/addr/link commands carry no stdin.
	for i := 0; i < 3; i++ {
		if rr.calls[i].stdin != "" {
			t.Errorf("call %d unexpectedly has stdin %q", i, rr.calls[i].stdin)
		}
	}
}

func TestSetupWithForwardingAndMasquerade(t *testing.T) {
	rr := &recordingRunner{}
	id := testIdentity()
	forwardCalled := false

	err := setup(context.Background(), rr.run, func() error { forwardCalled = true; return nil },
		id, v1alpha1.EgressDeny, nil, nil,
		applyOptions{subnetCIDR: "10.200.0.0/16", uplink: "eth0", enableForwarding: true})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !forwardCalled {
		t.Error("expected forwarding enabler to be called")
	}
	// tap, addr, link, nft shared table, nft chain, masquerade.
	if len(rr.calls) != 6 {
		t.Fatalf("expected 6 commands, got %d", len(rr.calls))
	}
	last := rr.calls[5].argv
	wantMasq := netconf.MasqueradeAddArgs("10.200.0.0/16", "eth0")
	if !reflect.DeepEqual(last, wantMasq) {
		t.Errorf("last call = %v, want masquerade %v", last, wantMasq)
	}
}

func TestSetupStopsOnError(t *testing.T) {
	rr := &recordingRunner{failOn: "addr add", failErr: errors.New("boom")}
	id := testIdentity()
	err := setup(context.Background(), rr.run, func() error { return nil },
		id, v1alpha1.EgressDeny, nil, nil, applyOptions{})
	if err == nil {
		t.Fatal("expected error from addr add")
	}
	// tap create then the failing addr add; nothing after.
	if len(rr.calls) != 2 {
		t.Errorf("expected 2 calls before stop, got %d", len(rr.calls))
	}
}

func TestTeardownCommandOrder(t *testing.T) {
	rr := &recordingRunner{}
	id := testIdentity()
	err := teardown(context.Background(), rr.run, id, applyOptions{})
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	// link del, dispatch element del, sandbox chain del, dynamic allow set del.
	// The shared table is NOT deleted: other sandboxes may still use it.
	if len(rr.calls) != 4 {
		t.Fatalf("expected 4 commands, got %d: %+v", len(rr.calls), rr.calls)
	}
	wantArgv := [][]string{
		netconf.LinkDelArgs(id.TapName),
		netconf.NftDeleteDispatchElementArgs(id.TapName),
		netconf.NftDeleteSandboxChainArgs(id.TapName),
		netconf.NftDeleteSandboxAllowSetArgs(id.TapName),
	}
	for i, w := range wantArgv {
		if !reflect.DeepEqual(rr.calls[i].argv, w) {
			t.Errorf("call %d argv = %v, want %v", i, rr.calls[i].argv, w)
		}
	}
	// Teardown must never delete the shared table.
	for _, c := range rr.calls {
		joined := strings.Join(c.argv, " ")
		if strings.Contains(joined, "delete table") {
			t.Errorf("teardown must not delete the shared table: %v", c.argv)
		}
	}
}

func TestTeardownBestEffort(t *testing.T) {
	// link del fails but the nft deletes still run; the first error is returned.
	rr := &recordingRunner{failOn: "link del", failErr: errors.New("no such device")}
	id := testIdentity()
	err := teardown(context.Background(), rr.run, id, applyOptions{})
	if err == nil {
		t.Fatal("expected error from link del")
	}
	if len(rr.calls) != 4 {
		t.Errorf("expected all teardown commands to run, got %d", len(rr.calls))
	}
}

func TestTeardownWithMasquerade(t *testing.T) {
	rr := &recordingRunner{}
	id := testIdentity()
	err := teardown(context.Background(), rr.run, id,
		applyOptions{subnetCIDR: "10.200.0.0/16", uplink: "eth0"})
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	// masquerade del, link del, dispatch element del, sandbox chain del,
	// dynamic allow set del.
	if len(rr.calls) != 5 {
		t.Fatalf("expected 5 commands, got %d", len(rr.calls))
	}
	if !reflect.DeepEqual(rr.calls[0].argv, netconf.MasqueradeDelArgs("10.200.0.0/16", "eth0")) {
		t.Errorf("first teardown call = %v, want masquerade del", rr.calls[0].argv)
	}
}

// TestSecondSandboxSetupIsIdempotent asserts that setting up a second sandbox
// reapplies the SAME shared-table skeleton (idempotent add, no flush of other
// sandboxes' chains) and adds its own distinct chain. The first sandbox's
// teardown then removes only its own chain + dispatch element, leaving the
// shared table and the second sandbox's chain intact.
func TestSecondSandboxSetupIsIdempotent(t *testing.T) {
	idA := testIdentity()
	idB := netconf.Identity{
		TapName: "sbtap1",
		HostIP:  net.ParseIP("10.200.0.5").To4(),
		GuestIP: net.ParseIP("10.200.0.6").To4(),
	}

	rrA := &recordingRunner{}
	if err := setup(context.Background(), rrA.run, func() error { return nil },
		idA, v1alpha1.EgressDeny, nil, nil, applyOptions{}); err != nil {
		t.Fatalf("setup A: %v", err)
	}
	rrB := &recordingRunner{}
	if err := setup(context.Background(), rrB.run, func() error { return nil },
		idB, v1alpha1.EgressDeny, nil, nil, applyOptions{}); err != nil {
		t.Fatalf("setup B: %v", err)
	}

	// Both Setups apply the identical shared-table skeleton (idempotent).
	if rrA.calls[3].stdin != rrB.calls[3].stdin {
		t.Errorf("shared-table skeleton differs between sandboxes:\nA:\n%s\nB:\n%s",
			rrA.calls[3].stdin, rrB.calls[3].stdin)
	}
	if !strings.Contains(rrA.calls[3].stdin, "add table inet "+netconf.SharedTableName()) {
		t.Errorf("shared table not idempotently added\n%s", rrA.calls[3].stdin)
	}
	// Each sandbox installs its OWN chain, never the other's.
	if !strings.Contains(rrA.calls[4].stdin, netconf.SandboxChainName(idA.TapName)) ||
		strings.Contains(rrA.calls[4].stdin, netconf.SandboxChainName(idB.TapName)) {
		t.Errorf("sandbox A chain leaks into/omits its own chain\n%s", rrA.calls[4].stdin)
	}
	if !strings.Contains(rrB.calls[4].stdin, netconf.SandboxChainName(idB.TapName)) ||
		strings.Contains(rrB.calls[4].stdin, netconf.SandboxChainName(idA.TapName)) {
		t.Errorf("sandbox B chain leaks into/omits its own chain\n%s", rrB.calls[4].stdin)
	}

	// Tearing down A touches only A's chain + dispatch element.
	rrT := &recordingRunner{}
	if err := teardown(context.Background(), rrT.run, idA, applyOptions{}); err != nil {
		t.Fatalf("teardown A: %v", err)
	}
	for _, c := range rrT.calls {
		joined := strings.Join(c.argv, " ")
		if strings.Contains(joined, netconf.SandboxChainName(idB.TapName)) || strings.Contains(joined, idB.TapName) {
			t.Errorf("teardown of A touched B's resources: %v", c.argv)
		}
	}
}

func TestFakeManagerRecords(t *testing.T) {
	fm := &FakeManager{}
	id := testIdentity()
	allow := []netconf.HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 443}}
	if err := fm.Setup(context.Background(), id, v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1")); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := fm.Teardown(context.Background(), id); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(fm.SetupLog) != 1 || fm.SetupLog[0].Policy != v1alpha1.EgressDeny {
		t.Errorf("SetupLog not recorded: %+v", fm.SetupLog)
	}
	if len(fm.Teardowns) != 1 || fm.Teardowns[0].TapName != "sbtap0" {
		t.Errorf("Teardowns not recorded: %+v", fm.Teardowns)
	}
}
