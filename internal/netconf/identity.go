// Package netconf holds pure, platform-independent helpers for sandbox
// network configuration: per-sandbox network identity allocation, nftables
// ruleset rendering, and command argument builders. Nothing here execs or
// touches the host; the Linux-tagged internal/network package wires the real
// exec path. This split keeps the rule rendering and allocation logic unit
// testable on any platform, mirroring the jailer UID allocator.
package netconf

import (
	"crypto/sha256"
	"fmt"
	"net"
	"sync"
)

// maxIfaceName is the Linux interface-name limit (IFNAMSIZ is 16, minus one
// for the trailing NUL). Tap names must be at most this many bytes.
const maxIfaceName = 15

// Identity is the network identity handed to one sandbox: its tap device
// name, guest MAC, and the host/guest IP pair of the per-sandbox /30
// point-to-point link. All fields are safe to log.
type Identity struct {
	TapName  string
	GuestMAC string
	HostIP   net.IP
	GuestIP  net.IP
}

// ErrSubnetExhausted is returned by Allocator.Acquire when every /30 block in
// the configured subnet is in use by a live sandbox.
type ErrSubnetExhausted struct {
	CIDR string
}

func (e *ErrSubnetExhausted) Error() string {
	return fmt.Sprintf("sandbox subnet %s exhausted; terminate sandboxes or widen the subnet", e.CIDR)
}

// Allocator hands out a unique network Identity per sandbox from a configured
// subnet, carving the subnet into /30 point-to-point blocks (one per
// sandbox). Each /30 has a network address, a host-side address, a guest-side
// address, and a broadcast address; we use the two usable addresses for the
// host and guest sides so every sandbox is isolated on its own link.
//
// The allocator is safe for concurrent use. Acquire is idempotent per
// sandboxID: acquiring the same id twice returns the same identity.
type Allocator struct {
	mu      sync.Mutex
	cidr    string
	base    uint32 // network address of the subnet, host byte order
	count   uint32 // number of /30 blocks in the subnet
	prefix  string // tap-name prefix
	next    uint32 // round-robin cursor over block indices
	byID    map[string]Identity
	inUse   map[uint32]bool // allocated block indices
	idIndex map[string]uint32
}

// NewAllocator builds an allocator over the given CIDR (e.g. 10.200.0.0/16)
// using tapPrefix for derived tap names. The CIDR must be an IPv4 network
// with a prefix length of at most 30 so it holds at least one /30 block.
func NewAllocator(cidr, tapPrefix string) (*Allocator, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse sandbox subnet %q: %w", cidr, err)
	}
	ip4 := ipNet.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("sandbox subnet %q is not IPv4", cidr)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("sandbox subnet %q is not IPv4", cidr)
	}
	if ones > 30 {
		return nil, fmt.Errorf("sandbox subnet %q is too small; need at least a /30", cidr)
	}
	// Number of /30 blocks is 2^(30-ones).
	count := uint32(1) << uint(30-ones)
	if tapPrefix == "" {
		return nil, fmt.Errorf("tap prefix must not be empty")
	}
	// The prefix plus an 8-hex-digit suffix must fit in the iface name limit.
	if len(tapPrefix)+8 > maxIfaceName {
		return nil, fmt.Errorf("tap prefix %q too long; must be at most %d chars", tapPrefix, maxIfaceName-8)
	}
	return &Allocator{
		cidr:    cidr,
		base:    ipToUint32(ip4),
		count:   count,
		prefix:  tapPrefix,
		byID:    make(map[string]Identity),
		inUse:   make(map[uint32]bool),
		idIndex: make(map[string]uint32),
	}, nil
}

// Acquire reserves and returns a network Identity for the given sandboxID.
// Calling it again with the same id returns the already-allocated identity.
// It returns ErrSubnetExhausted when no /30 block is free.
func (a *Allocator) Acquire(sandboxID string) (Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if id, ok := a.byID[sandboxID]; ok {
		return id, nil
	}

	for i := uint32(0); i < a.count; i++ {
		block := (a.next + i) % a.count
		if a.inUse[block] {
			continue
		}
		a.inUse[block] = true
		a.next = (block + 1) % a.count
		ident := a.identityForBlock(sandboxID, block)
		a.byID[sandboxID] = ident
		a.idIndex[sandboxID] = block
		return ident, nil
	}
	return Identity{}, &ErrSubnetExhausted{CIDR: a.cidr}
}

// MarkInUse reserves the EXACT /30 block a live, already-running sandbox holds,
// keyed by sandboxID, deriving the block index from the recorded identity's
// guest IP. It exists for crash re-adoption: after a forkd restart the
// in-memory allocator is empty, so calling Acquire would hand out the FIRST
// free block, almost never the one the surviving VM actually uses (the guest IP
// derives from the block index). That divergence lets a later fresh fork get
// the same /30 (ambiguous host routes) and makes Release free the wrong block.
// MarkInUse pins the recorded block instead so Acquire, TapForGuestIP, and
// Release all agree with the live VM. It returns an error when the guest IP is
// missing or falls outside the configured subnet, and is idempotent per
// sandboxID. The stored identity is the recorded one verbatim (its TapName and
// MAC, which derive from the original sandboxID, not from id alone).
func (a *Allocator) MarkInUse(sandboxID string, id Identity) error {
	guest := id.GuestIP.To4()
	if guest == nil {
		return fmt.Errorf("mark sandbox %s in use: identity has no IPv4 guest IP", sandboxID)
	}
	guestU := ipToUint32(guest)
	// Within block N the guest side is base + 4*N + 2, so recover N and validate
	// that the IP lands exactly on a guest-side address inside the subnet.
	if guestU < a.base+2 {
		return fmt.Errorf("mark sandbox %s in use: guest IP %v is below subnet %s", sandboxID, guest, a.cidr)
	}
	offset := guestU - a.base
	block := (offset - 2) / 4
	if (offset-2)%4 != 0 || block >= a.count {
		return fmt.Errorf("mark sandbox %s in use: guest IP %v is not a valid guest address in subnet %s", sandboxID, guest, a.cidr)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.byID[sandboxID]; ok {
		return nil // idempotent: already reserved for this id
	}
	a.inUse[block] = true
	a.byID[sandboxID] = id
	a.idIndex[sandboxID] = block
	return nil
}

// TapForGuestIP returns the tap device name of the live sandbox whose guest IP
// matches ip, or "" when no live sandbox holds that guest IP. The DNS proxy
// uses it to find the nftables set to pin into, given the source IP of a query.
// It is safe for concurrent use.
func (a *Allocator) TapForGuestIP(ip net.IP) string {
	want := ip.To4()
	if want == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range a.byID {
		if id.GuestIP.Equal(want) {
			return id.TapName
		}
	}
	return ""
}

// Release frees the identity held by sandboxID. Releasing an unknown id is a
// no-op.
func (a *Allocator) Release(sandboxID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	block, ok := a.idIndex[sandboxID]
	if !ok {
		return
	}
	delete(a.inUse, block)
	delete(a.byID, sandboxID)
	delete(a.idIndex, sandboxID)
}

// identityForBlock derives the deterministic identity for a /30 block index.
// Within block N the four addresses are base + 4*N + {0,1,2,3}: .0 is the
// network address, .1 the host side, .2 the guest side, .3 the broadcast.
func (a *Allocator) identityForBlock(sandboxID string, block uint32) Identity {
	blockBase := a.base + 4*block
	hostIP := uint32ToIP(blockBase + 1)
	guestIP := uint32ToIP(blockBase + 2)
	return Identity{
		TapName:  a.tapName(sandboxID),
		GuestMAC: deriveMAC(sandboxID),
		HostIP:   hostIP,
		GuestIP:  guestIP,
	}
}

// tapName derives a deterministic, valid interface name from the sandboxID:
// the prefix plus the first 8 hex digits of the sha256 of the id. The length
// is bounded by NewAllocator so it always fits IFNAMSIZ-1.
func (a *Allocator) tapName(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	return fmt.Sprintf("%s%08x", a.prefix, sum[:4])
}

// deriveMAC builds a locally-administered unicast MAC deterministically from
// the sandboxID. The first octet has bit 0x02 set (locally administered) and
// bit 0x01 cleared (unicast); the remaining five octets come from the hash.
func deriveMAC(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	first := (sum[0] | 0x02) &^ 0x01
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", first, sum[1], sum[2], sum[3], sum[4], sum[5])
}

func ipToUint32(ip net.IP) uint32 {
	ip4 := ip.To4()
	return uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
}

func uint32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
