package dnsproxy

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeRW is a dns.ResponseWriter that reports a settable RemoteAddr and
// captures the message the handler writes back.
type fakeRW struct {
	remote net.Addr
	mu     sync.Mutex
	msg    *dns.Msg
}

func (f *fakeRW) LocalAddr() net.Addr  { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53} }
func (f *fakeRW) RemoteAddr() net.Addr { return f.remote }
func (f *fakeRW) WriteMsg(m *dns.Msg) error {
	f.mu.Lock()
	f.msg = m
	f.mu.Unlock()
	return nil
}
func (f *fakeRW) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeRW) Close() error                { return nil }
func (f *fakeRW) TsigStatus() error           { return nil }
func (f *fakeRW) TsigTimersOnly(bool)         {}
func (f *fakeRW) Hijack()                     {}

func (f *fakeRW) written() *dns.Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msg
}

const upstreamTTL = 30

// startUpstream starts a real miekg/dns server that answers egress.test A with
// 198.51.100.2 (TTL upstreamTTL) and NXDOMAIN for everything else. It returns
// the listen address and a stop func.
func startUpstream(t *testing.T) (addr string, stop func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		q := r.Question[0]
		if q.Qtype == dns.TypeA && canonicalName(q.Name) == "egress.test" {
			rr, _ := dns.NewRR("egress.test. " + itoa(upstreamTTL) + " IN A 198.51.100.2")
			m.Answer = append(m.Answer, rr)
		} else {
			m.SetRcode(r, dns.RcodeNameError)
		}
		_ = w.WriteMsg(m)
	})
	srv := &dns.Server{PacketConn: pc, Handler: handler}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	return pc.LocalAddr().String(), func() { _ = srv.Shutdown() }
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func queryFor(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func newProxy(t *testing.T, reg *Registry, pinner Pinner, upstream string) *Server {
	t.Helper()
	return NewServer(reg, pinner, upstream, 60*time.Second, func(ip net.IP) string {
		return "sb" + ip.String()
	}, nil)
}

func TestServeDNSAllowlistedResolvesAndPins(t *testing.T) {
	upstream, stop := startUpstream(t)
	defer stop()

	reg := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	reg.Register(guest, map[string][]int{"egress.test": {8080}})
	pinner := NewFakePinner()
	s := newProxy(t, reg, pinner, upstream)

	rw := &fakeRW{remote: &net.UDPAddr{IP: guest, Port: 5300}}
	s.ServeDNS(rw, queryFor("egress.test", dns.TypeA))

	out := rw.written()
	if out == nil || out.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR with answer, got %#v", out)
	}
	if len(out.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(out.Answer))
	}
	a, ok := out.Answer[0].(*dns.A)
	if !ok || !a.A.Equal(net.ParseIP("198.51.100.2")) {
		t.Fatalf("answer = %v, want 198.51.100.2", out.Answer[0])
	}

	pins := pinner.Pins()
	if len(pins) != 1 {
		t.Fatalf("expected 1 pin, got %d: %+v", len(pins), pins)
	}
	p := pins[0]
	if !p.IP.Equal(net.ParseIP("198.51.100.2")) || p.Port != 8080 {
		t.Errorf("pin = %v:%d, want 198.51.100.2:8080", p.IP, p.Port)
	}
	if p.TTL < 60*time.Second {
		t.Errorf("pin ttl %v below floor 60s", p.TTL)
	}
	if p.Tap != "sb10.200.0.2" {
		t.Errorf("pin tap = %q, want sb10.200.0.2", p.Tap)
	}
}

func TestServeDNSNonAllowlistedRefusedNoPin(t *testing.T) {
	upstream, stop := startUpstream(t)
	defer stop()

	reg := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	reg.Register(guest, map[string][]int{"egress.test": {8080}})
	pinner := NewFakePinner()
	s := newProxy(t, reg, pinner, upstream)

	rw := &fakeRW{remote: &net.UDPAddr{IP: guest, Port: 5300}}
	s.ServeDNS(rw, queryFor("blocked.test", dns.TypeA))

	out := rw.written()
	if out == nil || out.Rcode != dns.RcodeRefused {
		t.Fatalf("expected REFUSED, got %#v", out)
	}
	if got := len(pinner.Pins()); got != 0 {
		t.Errorf("expected no pins, got %d", got)
	}
}

func TestServeDNSPortScopedPinning(t *testing.T) {
	upstream, stop := startUpstream(t)
	defer stop()

	reg := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	// Allowed on 443 only.
	reg.Register(guest, map[string][]int{"egress.test": {443}})
	pinner := NewFakePinner()
	s := newProxy(t, reg, pinner, upstream)

	rw := &fakeRW{remote: &net.UDPAddr{IP: guest, Port: 5300}}
	s.ServeDNS(rw, queryFor("egress.test", dns.TypeA))

	pins := pinner.Pins()
	if len(pins) != 1 || pins[0].Port != 443 {
		t.Fatalf("expected exactly one pin on 443, got %+v", pins)
	}
	for _, p := range pins {
		if p.Port == 8080 {
			t.Errorf("must not pin 8080 when only 443 is allowed")
		}
	}
}

func TestServeDNSTwoGuestsDistinctVerdicts(t *testing.T) {
	upstream, stop := startUpstream(t)
	defer stop()

	reg := NewRegistry()
	a := net.ParseIP("10.200.0.2")
	b := net.ParseIP("10.200.0.6")
	reg.Register(a, map[string][]int{"egress.test": {8080}})
	reg.Register(b, map[string][]int{"other.test": {443}})
	pinner := NewFakePinner()
	s := newProxy(t, reg, pinner, upstream)

	rwA := &fakeRW{remote: &net.UDPAddr{IP: a, Port: 5300}}
	s.ServeDNS(rwA, queryFor("egress.test", dns.TypeA))
	if rwA.written().Rcode != dns.RcodeSuccess {
		t.Errorf("guest a should resolve egress.test")
	}

	rwB := &fakeRW{remote: &net.UDPAddr{IP: b, Port: 5300}}
	s.ServeDNS(rwB, queryFor("egress.test", dns.TypeA))
	if rwB.written().Rcode != dns.RcodeRefused {
		t.Errorf("guest b should be refused for egress.test")
	}

	for _, p := range pinner.Pins() {
		if p.GuestIP.Equal(b) {
			t.Errorf("guest b must not have pins")
		}
	}
}

func TestServeDNSEmptyTapRefusedNoPin(t *testing.T) {
	upstream, stop := startUpstream(t)
	defer stop()

	reg := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	reg.Register(guest, map[string][]int{"egress.test": {8080}})
	pinner := NewFakePinner()
	// tapFor returns "" for this guest: a registry/allocator desync, or a query
	// from an IP that has already been released. The proxy must not pin into a
	// bogus set and must not hand the guest an answer it cannot use.
	s := NewServer(reg, pinner, upstream, 60*time.Second, func(net.IP) string {
		return ""
	}, nil)

	rw := &fakeRW{remote: &net.UDPAddr{IP: guest, Port: 5300}}
	s.ServeDNS(rw, queryFor("egress.test", dns.TypeA))

	out := rw.written()
	if out == nil || out.Rcode != dns.RcodeRefused {
		t.Fatalf("expected REFUSED when tap is empty, got %#v", out)
	}
	if got := len(pinner.Pins()); got != 0 {
		t.Errorf("expected no pins when tap is empty, got %d", got)
	}
}

func TestServeDNSUpstreamFailureServfailNoPin(t *testing.T) {
	reg := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	reg.Register(guest, map[string][]int{"egress.test": {8080}})
	pinner := NewFakePinner()
	// Point at a closed port so Exchange fails.
	s := newProxy(t, reg, pinner, "127.0.0.1:1")
	s.client.Timeout = 500 * time.Millisecond

	rw := &fakeRW{remote: &net.UDPAddr{IP: guest, Port: 5300}}
	s.ServeDNS(rw, queryFor("egress.test", dns.TypeA))

	out := rw.written()
	if out == nil || out.Rcode != dns.RcodeServerFailure {
		t.Fatalf("expected SERVFAIL, got %#v", out)
	}
	if got := len(pinner.Pins()); got != 0 {
		t.Errorf("expected no pins on upstream failure, got %d", got)
	}
}
