package dnsproxy

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/paperclipinc/sandbox/internal/netconf"
)

func TestNftPinnerElementSyntax(t *testing.T) {
	var got []string
	p := NewNftPinner(func(argv []string) error {
		got = argv
		return nil
	})

	err := p.Pin(net.ParseIP("10.200.0.2"), "sbtap0", net.ParseIP("198.51.100.2"), 8080, 30*time.Second)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	want := []string{
		"nft", "add", "element", "inet",
		netconf.SharedTableName(), netconf.SandboxAllowSetName("sbtap0"),
		"{ 198.51.100.2 . 8080 timeout 30s }",
	}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
	if !strings.Contains(strings.Join(got, " "), "198.51.100.2 . 8080 timeout 30s") {
		t.Errorf("element does not match set type ip . port with timeout: %v", got)
	}
}

// TestNftPinnerV6IntoV6Set asserts an IPv6 address is pinned into the tap's v6
// set (SandboxAllowSet6Name), not the v4 set, so the element type matches.
func TestNftPinnerV6IntoV6Set(t *testing.T) {
	var got []string
	p := NewNftPinner(func(argv []string) error {
		got = argv
		return nil
	})

	err := p.Pin(net.ParseIP("10.200.0.2"), "sbtap0", net.ParseIP("2001:db8::1"), 443, 30*time.Second)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	want := []string{
		"nft", "add", "element", "inet",
		netconf.SharedTableName(), netconf.SandboxAllowSet6Name("sbtap0"),
		"{ 2001:db8::1 . 443 timeout 30s }",
	}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestNftPinnerRoundsSubSecondTTLUp(t *testing.T) {
	var got []string
	p := NewNftPinner(func(argv []string) error { got = argv; return nil })
	if err := p.Pin(net.ParseIP("10.200.0.2"), "t", net.ParseIP("1.2.3.4"), 80, 1500*time.Millisecond); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if got[len(got)-1] != "{ 1.2.3.4 . 80 timeout 2s }" {
		t.Errorf("element = %q, want timeout 2s", got[len(got)-1])
	}
}
