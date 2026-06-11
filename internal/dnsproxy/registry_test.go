package dnsproxy

import (
	"net"
	"sort"
	"testing"
)

func TestRegistryLookupExactMatch(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"egress.test": {8080, 443}})

	ports, ok := r.Lookup(guest, "egress.test")
	if !ok {
		t.Fatal("expected egress.test to be allowed")
	}
	sort.Ints(ports)
	if len(ports) != 2 || ports[0] != 443 || ports[1] != 8080 {
		t.Errorf("ports = %v, want [443 8080]", ports)
	}
}

func TestRegistryLookupCaseAndTrailingDot(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"Egress.Test": {8080}})

	if _, ok := r.Lookup(guest, "EGRESS.TEST."); !ok {
		t.Error("expected case-insensitive match with trailing dot")
	}
	if _, ok := r.Lookup(guest, "egress.test"); !ok {
		t.Error("expected lowercase match")
	}
}

func TestRegistryLookupUnknownNameAndGuest(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"egress.test": {8080}})

	if _, ok := r.Lookup(guest, "other.test"); ok {
		t.Error("unknown name must not be allowed")
	}
	if _, ok := r.Lookup(net.ParseIP("10.200.0.6"), "egress.test"); ok {
		t.Error("unregistered guest must not be allowed")
	}
}

func TestRegistryDeregister(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"egress.test": {8080}})
	r.Deregister(guest)
	if _, ok := r.Lookup(guest, "egress.test"); ok {
		t.Error("deregistered guest must not be allowed")
	}
}

func TestRegistryTwoGuestsDistinct(t *testing.T) {
	r := NewRegistry()
	a := net.ParseIP("10.200.0.2")
	b := net.ParseIP("10.200.0.6")
	r.Register(a, map[string][]int{"egress.test": {8080}})
	r.Register(b, map[string][]int{"other.test": {443}})

	if _, ok := r.Lookup(a, "egress.test"); !ok {
		t.Error("guest a should allow egress.test")
	}
	if _, ok := r.Lookup(a, "other.test"); ok {
		t.Error("guest a should not allow other.test")
	}
	if _, ok := r.Lookup(b, "egress.test"); ok {
		t.Error("guest b should not allow egress.test")
	}
}
