package dnsproxy

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/paperclipinc/sandbox/internal/netconf"
)

// Pinner installs a resolved (address . port) into a sandbox's dynamic egress
// allow set so the guest can reach exactly the address the proxy resolved, for
// the record's TTL. It is the bridge between the resolver and nftables.
type Pinner interface {
	// Pin allows the guest behind tap to reach ip:port for ttl. guestIP is the
	// querying sandbox's source address, supplied for attribution; the set is
	// keyed by tap.
	Pin(guestIP net.IP, tap string, ip net.IP, port int, ttl time.Duration) error
}

// NftPinner adds elements to a tap's dynamic allow set via nft. The nft
// invocation is injected as runner so the pinner is unit-testable without root.
type NftPinner struct {
	// runner executes one nft invocation given its argv. Production wires this
	// to an exec-based runner; tests capture the argv.
	runner func(argv []string) error
}

// NewNftPinner returns an NftPinner that runs nft via the given runner.
func NewNftPinner(runner func(argv []string) error) *NftPinner {
	return &NftPinner{runner: runner}
}

// Pin adds (ip . port) to the tap's dynamic set with a timeout equal to ttl,
// rounded up to whole seconds (a sub-second timeout would expire immediately).
// An IPv4 address is pinned into the v4 set, an IPv6 address into the v6 set, so
// the element type matches the set type rendered by RenderSandboxChain:
//
//	nft add element inet <table> sb_<tap>_dyn  { <v4> . <port> timeout <N>s }
//	nft add element inet <table> sb_<tap>_dyn6 { <v6> . <port> timeout <N>s }
func (p *NftPinner) Pin(_ net.IP, tap string, ip net.IP, port int, ttl time.Duration) error {
	secs := int(ttl / time.Second)
	if ttl%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	set := netconf.SandboxAllowSetName(tap)
	if ip.To4() == nil {
		set = netconf.SandboxAllowSet6Name(tap)
	}
	element := fmt.Sprintf("{ %s . %d timeout %ds }", ip.String(), port, secs)
	argv := []string{
		"nft", "add", "element", "inet",
		netconf.SharedTableName(), set,
		element,
	}
	if err := p.runner(argv); err != nil {
		return fmt.Errorf("pin %s:%d for tap %s: %w", ip.String(), port, tap, err)
	}
	return nil
}

// PinnedEntry is one recorded pin, used by FakePinner in tests.
type PinnedEntry struct {
	GuestIP net.IP
	Tap     string
	IP      net.IP
	Port    int
	TTL     time.Duration
}

// FakePinner records pins instead of touching nftables. It is safe for
// concurrent use so tests can drive the proxy with -race.
type FakePinner struct {
	mu      sync.Mutex
	pinned  []PinnedEntry
	failErr error
}

// NewFakePinner returns an empty FakePinner.
func NewFakePinner() *FakePinner {
	return &FakePinner{}
}

// SetError makes subsequent Pin calls return err (and not record the pin).
func (f *FakePinner) SetError(err error) {
	f.mu.Lock()
	f.failErr = err
	f.mu.Unlock()
}

// Pin records the call unless a failure error is configured.
func (f *FakePinner) Pin(guestIP net.IP, tap string, ip net.IP, port int, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.pinned = append(f.pinned, PinnedEntry{GuestIP: guestIP, Tap: tap, IP: ip, Port: port, TTL: ttl})
	return nil
}

// Pins returns a copy of the recorded pins.
func (f *FakePinner) Pins() []PinnedEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PinnedEntry, len(f.pinned))
	copy(out, f.pinned)
	return out
}
