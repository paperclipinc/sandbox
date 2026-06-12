// Command dns-stub is a deterministic stub DNS resolver for the KVM CI
// name-egress proof. It answers A queries from a fixed name->IP map and returns
// NXDOMAIN for anything else; AAAA queries get an empty NOERROR. It exists so
// the name-egress phase has an upstream resolver entirely under our control: the
// controlled dnsproxy forwards allowed queries here, and because every mapped
// name (egress.test, denied.test) resolves to the SAME test server IP, the
// phase proves allowlisting is by NAME, not by IP.
//
// Usage:
//
//	dns-stub --listen 127.0.0.1:5300 --map egress.test=198.51.100.2,denied.test=198.51.100.2 [--ttl 30]
//
// All values it handles (names, IPs, ports) are safe to log.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/miekg/dns"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:5300", "address (host:port) to serve DNS on, udp and tcp")
	mapFlag := flag.String("map", "", "comma-separated name=ip entries answered for A queries, e.g. egress.test=198.51.100.2,denied.test=198.51.100.2")
	ttl := flag.Int("ttl", 30, "TTL in seconds for answered A records")
	flag.Parse()

	answers, err := parseMap(*mapFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dns-stub: %v\n", err)
		os.Exit(1)
	}
	if len(answers) == 0 {
		fmt.Fprintln(os.Stderr, "dns-stub: --map must contain at least one name=ip entry")
		os.Exit(1)
	}

	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) == 0 {
			_ = w.WriteMsg(m)
			return
		}
		q := r.Question[0]
		name := canonical(q.Name)
		switch q.Qtype {
		case dns.TypeA:
			ip, ok := answers[name]
			if !ok {
				m.SetRcode(r, dns.RcodeNameError)
				break
			}
			rr := &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: uint32(*ttl)},
				A:   ip,
			}
			m.Answer = append(m.Answer, rr)
		case dns.TypeAAAA:
			// This stub is A-only: an empty NOERROR models an A-only upstream name
			// (the proxy now also forwards and pins AAAA when the upstream answers).
		default:
			m.SetRcode(r, dns.RcodeNameError)
		}
		_ = w.WriteMsg(m)
	})

	errCh := make(chan error, 2)
	udp := &dns.Server{Addr: *listen, Net: "udp", Handler: handler}
	tcp := &dns.Server{Addr: *listen, Net: "tcp", Handler: handler}
	go func() { errCh <- udp.ListenAndServe() }()
	go func() { errCh <- tcp.ListenAndServe() }()
	fmt.Printf("dns-stub: serving %d name(s) on %s (udp+tcp), ttl=%ds\n", len(answers), *listen, *ttl)
	for name, ip := range answers {
		fmt.Printf("dns-stub: %s A %s\n", name, ip)
	}
	if err := <-errCh; err != nil {
		fmt.Fprintf(os.Stderr, "dns-stub: serve error: %v\n", err)
		os.Exit(1)
	}
}

// parseMap parses "name=ip,name=ip" into a canonicalized name->IPv4 map.
func parseMap(s string) (map[string]net.IP, error) {
	out := make(map[string]net.IP)
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("map entry %q is not name=ip", entry)
		}
		ip := net.ParseIP(strings.TrimSpace(v)).To4()
		if ip == nil {
			return nil, fmt.Errorf("map entry %q has an invalid IPv4 address", entry)
		}
		out[canonical(k)] = ip
	}
	return out, nil
}

// canonical lowercases a DNS name and strips a single trailing dot so map keys
// and query names compare equal regardless of FQDN spelling.
func canonical(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}
