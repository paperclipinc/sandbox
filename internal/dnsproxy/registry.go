// Package dnsproxy implements a controlled DNS resolver for sandbox egress.
//
// A sandbox is allowed to reach a set of DNS names on specific ports. Names
// cannot be enforced by nftables directly (it matches on IP, not on the name a
// guest looked up), so the proxy is the single resolver the guest is allowed to
// query: it resolves only allowlisted names, and for each resolved address it
// pins (address . port) into that sandbox's dynamic nftables set for the
// record's TTL. The guest can then connect to exactly the address the proxy
// resolved, for exactly as long as the answer is valid.
package dnsproxy

import (
	"net"
	"strings"
	"sync"
)

// Allowlist maps a lowercased fully qualified domain name to the set of TCP
// ports the sandbox may reach for that name. The inner map is used as a set:
// the bool value is always true for an allowed port.
type Allowlist map[string]map[int]bool

// Registry maps a sandbox guest IP (the source address of its DNS queries) to
// the names and ports that sandbox may resolve. It is safe for concurrent use:
// the DNS proxy reads it on every query while the daemon registers and
// deregisters sandboxes.
type Registry struct {
	mu      sync.RWMutex
	byGuest map[string]Allowlist
}

// NewRegistry returns an empty Registry ready for use.
func NewRegistry() *Registry {
	return &Registry{byGuest: make(map[string]Allowlist)}
}

// Register records the allowlist for a guest IP. names maps a DNS name to the
// ports allowed for it; names are lowercased and any trailing dot is dropped so
// lookups match regardless of how the guest spells the query. Registering the
// same guest again replaces its allowlist.
func (r *Registry) Register(guestIP net.IP, names map[string][]int) {
	al := make(Allowlist, len(names))
	for name, ports := range names {
		key := canonicalName(name)
		set := make(map[int]bool, len(ports))
		for _, p := range ports {
			set[p] = true
		}
		al[key] = set
	}
	r.mu.Lock()
	r.byGuest[guestIP.String()] = al
	r.mu.Unlock()
}

// Deregister removes a guest's allowlist. Subsequent lookups for that guest
// return ok=false so its queries are refused.
func (r *Registry) Deregister(guestIP net.IP) {
	r.mu.Lock()
	delete(r.byGuest, guestIP.String())
	r.mu.Unlock()
}

// Lookup returns the allowed ports for a name queried by a guest. Matching is
// exact on the fully qualified name, case-insensitive, and tolerant of a
// trailing dot on the queried name. ok is false when the guest is not
// registered or the name is not on its allowlist.
func (r *Registry) Lookup(guestIP net.IP, name string) (ports []int, ok bool) {
	key := canonicalName(name)
	r.mu.RLock()
	defer r.mu.RUnlock()
	al, found := r.byGuest[guestIP.String()]
	if !found {
		return nil, false
	}
	set, found := al[key]
	if !found {
		return nil, false
	}
	ports = make([]int, 0, len(set))
	for p := range set {
		ports = append(ports, p)
	}
	return ports, true
}

// canonicalName lowercases a DNS name and strips a single trailing dot so the
// registry keys and query names compare equal regardless of FQDN spelling.
func canonicalName(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".")
}
