package dnsproxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/miekg/dns"
)

// Server is the controlled resolver. It answers DNS queries from sandboxes,
// resolving only names on the querying sandbox's allowlist and pinning each
// resolved address into that sandbox's dynamic nftables set before returning
// the answer. Names that are not allowlisted are refused; upstream failures are
// reported as SERVFAIL and pin nothing.
//
// Both A (IPv4) and AAAA (IPv6) are enforced: an allowed name's A records are
// pinned into the sandbox's v4 set and its AAAA records into the v6 set, each
// for the record's TTL. Any other query type is refused.
type Server struct {
	registry *Registry
	pinner   Pinner
	// upstream is the host:port of the real resolver to forward allowed queries
	// to (for example 1.1.1.1:53).
	upstream string
	// ttlFloor is the minimum pin lifetime. A record's TTL is raised to this
	// floor so a very short TTL does not expire the pin before the guest
	// connects.
	ttlFloor time.Duration
	// tapFor maps a guest source IP to its tap device name, used as the set key
	// when pinning.
	tapFor func(net.IP) string

	client *dns.Client
	logger *slog.Logger

	udp *dns.Server
	tcp *dns.Server
}

// NewServer builds a Server. logger may be nil, in which case a discarding
// logger is used.
func NewServer(registry *Registry, pinner Pinner, upstream string, ttlFloor time.Duration, tapFor func(net.IP) string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Server{
		registry: registry,
		pinner:   pinner,
		upstream: upstream,
		ttlFloor: ttlFloor,
		tapFor:   tapFor,
		client:   &dns.Client{},
		logger:   logger,
	}
}

// ServeDNS implements dns.Handler. It is the whole enforcement path: attribute
// the query to a sandbox by source IP, check the allowlist, forward allowed
// queries upstream, pin the resolved addresses, and return the answer.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		s.refuse(w, r)
		return
	}
	q := r.Question[0]
	clientIP := clientIPOf(w.RemoteAddr())
	if clientIP == nil {
		s.refuse(w, r)
		return
	}

	// Only A and AAAA are forwarded and pinned. Every other qtype is refused so
	// the resolver cannot be used as a covert tunnel.
	if q.Qtype != dns.TypeA && q.Qtype != dns.TypeAAAA {
		s.refuse(w, r)
		return
	}

	ports, ok := s.registry.Lookup(clientIP, q.Name)
	if !ok {
		// The name is not a secret; logging it aids debugging of egress denials.
		s.logger.Debug("dns refused: name not allowlisted",
			"guest", clientIP.String(), "name", q.Name)
		s.refuse(w, r)
		return
	}

	// Resolve the source's tap before forwarding. An empty tap means a
	// registry/allocator desync, or a query from an IP that has already been
	// released: there is no set to pin into, so any answer would be unreachable
	// for the guest. Refuse rather than forward upstream and pin a bogus set.
	tap := s.tapFor(clientIP)
	if tap == "" {
		s.logger.Debug("dns refused: source guest has no tap mapping",
			"guest", clientIP.String(), "name", q.Name)
		s.refuse(w, r)
		return
	}

	resp, _, err := s.client.Exchange(r, s.upstream)
	if err != nil || resp == nil {
		s.logger.Debug("dns upstream failure",
			"guest", clientIP.String(), "name", q.Name, "err", err)
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}

	for _, ans := range resp.Answer {
		// Pin each A and AAAA answer. The pinner routes a v4 address into the v4
		// set and a v6 address into the v6 set so the element type matches.
		var addr net.IP
		switch rr := ans.(type) {
		case *dns.A:
			addr = rr.A
		case *dns.AAAA:
			addr = rr.AAAA
		default:
			continue
		}
		ttl := time.Duration(ans.Header().Ttl) * time.Second
		if ttl < s.ttlFloor {
			ttl = s.ttlFloor
		}
		for _, port := range ports {
			if perr := s.pinner.Pin(clientIP, tap, addr, port, ttl); perr != nil {
				s.logger.Warn("dns pin failed",
					"guest", clientIP.String(), "name", q.Name, "port", port, "err", perr)
			}
		}
	}

	if werr := w.WriteMsg(resp); werr != nil {
		s.logger.Debug("dns write failed", "guest", clientIP.String(), "err", werr)
	}
}

func (s *Server) refuse(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeRefused)
	_ = w.WriteMsg(m)
}

// ListenAndServe starts the resolver on addr for both UDP and TCP. It blocks
// until one of the listeners fails or Shutdown is called, returning the first
// error.
func (s *Server) ListenAndServe(addr string) error {
	s.udp = &dns.Server{Addr: addr, Net: "udp", Handler: s}
	s.tcp = &dns.Server{Addr: addr, Net: "tcp", Handler: s}

	errCh := make(chan error, 2)
	go func() { errCh <- s.udp.ListenAndServe() }()
	go func() { errCh <- s.tcp.ListenAndServe() }()
	return <-errCh
}

// Shutdown stops both listeners. It is safe to call before ListenAndServe has
// fully started; nil listeners are skipped.
func (s *Server) Shutdown(ctx context.Context) error {
	var firstErr error
	if s.udp != nil {
		if err := s.udp.ShutdownContext(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shutdown udp resolver: %w", err)
		}
	}
	if s.tcp != nil {
		if err := s.tcp.ShutdownContext(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shutdown tcp resolver: %w", err)
		}
	}
	return firstErr
}

// clientIPOf extracts the IP from a DNS client's remote address (UDP or TCP).
func clientIPOf(addr net.Addr) net.IP {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.IP
	case *net.TCPAddr:
		return a.IP
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return nil
		}
		return net.ParseIP(host)
	}
}

// discard is an io.Writer that drops everything, used for the default logger.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
