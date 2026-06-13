package netconf

import (
	"net"
	"testing"
)

func TestAcquireDistinctIdentities(t *testing.T) {
	a, err := NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	id1, err := a.Acquire("sandbox-a")
	if err != nil {
		t.Fatalf("Acquire a: %v", err)
	}
	id2, err := a.Acquire("sandbox-b")
	if err != nil {
		t.Fatalf("Acquire b: %v", err)
	}
	if id1.TapName == id2.TapName {
		t.Errorf("tap names collide: %q", id1.TapName)
	}
	if id1.GuestMAC == id2.GuestMAC {
		t.Errorf("MACs collide: %q", id1.GuestMAC)
	}
	if id1.GuestIP.Equal(id2.GuestIP) {
		t.Errorf("guest IPs collide: %v", id1.GuestIP)
	}
	if id1.HostIP.Equal(id2.HostIP) {
		t.Errorf("host IPs collide: %v", id1.HostIP)
	}
	// Host and guest sides differ within a sandbox and are consecutive.
	if id1.HostIP.Equal(id1.GuestIP) {
		t.Errorf("host and guest IP equal within sandbox: %v", id1.HostIP)
	}
}

func TestAcquireIdempotent(t *testing.T) {
	a, _ := NewAllocator("10.200.0.0/16", "sb")
	id1, _ := a.Acquire("same")
	id2, _ := a.Acquire("same")
	if id1.TapName != id2.TapName || id1.GuestMAC != id2.GuestMAC ||
		!id1.HostIP.Equal(id2.HostIP) || !id1.GuestIP.Equal(id2.GuestIP) {
		t.Errorf("Acquire not idempotent: %+v vs %+v", id1, id2)
	}
}

func TestMACIsLocallyAdministeredUnicast(t *testing.T) {
	a, _ := NewAllocator("10.200.0.0/16", "sb")
	for _, sid := range []string{"x", "y", "z", "another-sandbox", "12345"} {
		id, err := a.Acquire(sid)
		if err != nil {
			t.Fatalf("Acquire %q: %v", sid, err)
		}
		hw, err := net.ParseMAC(id.GuestMAC)
		if err != nil {
			t.Fatalf("ParseMAC %q: %v", id.GuestMAC, err)
		}
		first := hw[0]
		if first&0x02 == 0 {
			t.Errorf("MAC %q not locally administered (bit 0x02 clear)", id.GuestMAC)
		}
		if first&0x01 != 0 {
			t.Errorf("MAC %q is multicast (bit 0x01 set)", id.GuestMAC)
		}
	}
}

func TestTapNameLengthValid(t *testing.T) {
	a, _ := NewAllocator("10.200.0.0/16", "sb")
	id, _ := a.Acquire("a-very-long-sandbox-identifier-that-should-not-overflow")
	if len(id.TapName) == 0 || len(id.TapName) > maxIfaceName {
		t.Errorf("tap name %q length %d out of range (1..%d)", id.TapName, len(id.TapName), maxIfaceName)
	}
}

func TestExhaustion(t *testing.T) {
	// A /30 subnet holds exactly one /30 block, so the second distinct
	// sandbox must fail.
	a, err := NewAllocator("10.200.0.0/30", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	if _, err := a.Acquire("first"); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	_, err = a.Acquire("second")
	if err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
	if _, ok := err.(*ErrSubnetExhausted); !ok {
		t.Errorf("expected *ErrSubnetExhausted, got %T: %v", err, err)
	}
}

func TestReuseAfterRelease(t *testing.T) {
	a, _ := NewAllocator("10.200.0.0/30", "sb")
	id1, err := a.Acquire("first")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := a.Acquire("second"); err == nil {
		t.Fatal("expected exhaustion before release")
	}
	a.Release("first")
	id2, err := a.Acquire("second")
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	// The freed block is reusable; the IP pair is the same block.
	if !id1.HostIP.Equal(id2.HostIP) {
		t.Errorf("expected reused host IP %v, got %v", id1.HostIP, id2.HostIP)
	}
}

func TestNewAllocatorRejectsBadInputs(t *testing.T) {
	cases := []struct{ cidr, prefix string }{
		{"not-a-cidr", "sb"},
		{"10.200.0.0/31", "sb"},            // smaller than /30
		{"fd00::/64", "sb"},                // IPv6
		{"10.200.0.0/16", ""},              // empty prefix
		{"10.200.0.0/16", "toolongprefix"}, // prefix + 8 hex > 15
	}
	for _, c := range cases {
		if _, err := NewAllocator(c.cidr, c.prefix); err == nil {
			t.Errorf("NewAllocator(%q,%q) expected error, got nil", c.cidr, c.prefix)
		}
	}
}

func TestIPsWithinSubnet(t *testing.T) {
	a, _ := NewAllocator("10.200.0.0/16", "sb")
	_, ipNet, _ := net.ParseCIDR("10.200.0.0/16")
	id, _ := a.Acquire("s")
	if !ipNet.Contains(id.HostIP) {
		t.Errorf("host IP %v outside subnet", id.HostIP)
	}
	if !ipNet.Contains(id.GuestIP) {
		t.Errorf("guest IP %v outside subnet", id.GuestIP)
	}
}

func TestTapForGuestIP(t *testing.T) {
	a, err := NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	id, err := a.Acquire("sb1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := a.TapForGuestIP(id.GuestIP); got != id.TapName {
		t.Errorf("TapForGuestIP(%v) = %q, want %q", id.GuestIP, got, id.TapName)
	}
	// An IP no sandbox holds maps to no tap.
	if got := a.TapForGuestIP(net.ParseIP("10.99.99.99")); got != "" {
		t.Errorf("TapForGuestIP(unknown) = %q, want empty", got)
	}
	// After release the mapping is gone.
	a.Release("sb1")
	if got := a.TapForGuestIP(id.GuestIP); got != "" {
		t.Errorf("TapForGuestIP after release = %q, want empty", got)
	}
}

// TestMarkInUseReservesExactBlock checks crash re-adoption: after a forkd
// restart the allocator is empty, but a live VM still holds a specific /30
// block derived from its recorded guest IP. MarkInUse must reserve THAT exact
// block (not the first free one), so a later Acquire never hands the same /30
// to a fresh fork, TapForGuestIP resolves the recorded VM, and Release frees
// the right block.
func TestMarkInUseReservesExactBlock(t *testing.T) {
	a, err := NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	// The recorded identity of a VM that survived the crash. Its guest IP is in
	// block index 5 (10.200.0.0 + 4*5 + 2 = 10.200.0.22), which is NOT the first
	// free block an empty allocator would hand out (that would be block 0).
	rec := Identity{
		TapName: "sbtap-recorded",
		HostIP:  net.ParseIP("10.200.0.21").To4(),
		GuestIP: net.ParseIP("10.200.0.22").To4(),
	}
	if err := a.MarkInUse("recorded-vm", rec); err != nil {
		t.Fatalf("MarkInUse: %v", err)
	}

	// The reserved identity is reported exactly as recorded.
	if got := a.TapForGuestIP(rec.GuestIP); got != rec.TapName {
		t.Fatalf("TapForGuestIP(%v) = %q, want %q", rec.GuestIP, got, rec.TapName)
	}

	// A fresh Acquire must NOT collide with the reserved block: its guest IP must
	// differ from the recorded VM's.
	fresh, err := a.Acquire("fresh-fork")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if fresh.GuestIP.Equal(rec.GuestIP) {
		t.Fatalf("fresh fork was handed the reserved block %v", fresh.GuestIP)
	}

	// Release frees exactly the reserved block.
	a.Release("recorded-vm")
	if got := a.TapForGuestIP(rec.GuestIP); got != "" {
		t.Fatalf("reserved block not freed by Release: tap %q", got)
	}
}

// TestMarkInUseIdempotentAndAcquireConsistent checks that marking an id in use
// then Acquiring the SAME id returns the reserved identity (so adoption then a
// later idempotent Acquire agree), and that a guest IP outside the subnet is
// rejected rather than silently mis-reserved.
func TestMarkInUseIdempotentAndAcquireConsistent(t *testing.T) {
	a, err := NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	rec := Identity{
		TapName: "sbtap-x",
		GuestIP: net.ParseIP("10.200.0.6").To4(), // block 1
	}
	if err := a.MarkInUse("vm-x", rec); err != nil {
		t.Fatalf("MarkInUse: %v", err)
	}
	got, err := a.Acquire("vm-x")
	if err != nil {
		t.Fatalf("Acquire same id: %v", err)
	}
	if !got.GuestIP.Equal(rec.GuestIP) {
		t.Fatalf("Acquire(vm-x) = %v, want reserved %v", got.GuestIP, rec.GuestIP)
	}

	// A guest IP outside the configured subnet must error, not corrupt state.
	if err := a.MarkInUse("vm-bad", Identity{GuestIP: net.ParseIP("10.99.0.6").To4()}); err == nil {
		t.Fatalf("MarkInUse accepted an out-of-subnet guest IP")
	}
}
