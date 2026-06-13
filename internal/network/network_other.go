//go:build !linux

package network

import (
	"context"
	"fmt"
	"net"
	"runtime"

	"github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/netconf"
)

// notSupportedManager is returned on non-Linux platforms. Tap devices and
// nftables are Linux-only, so Setup and Teardown return a clear error.
type notSupportedManager struct{}

// Options mirrors the Linux Options so callers compile on any platform.
type Options struct {
	SubnetCIDR       string
	Uplink           string
	EnableForwarding bool
}

// NewManager returns a Manager that reports networking is unsupported on this
// platform.
func NewManager(_ Options) Manager {
	return notSupportedManager{}
}

func (notSupportedManager) Setup(_ context.Context, _ netconf.Identity, _ v1alpha1.EgressPolicy, _ []netconf.HostPort, _ net.IP) error {
	return fmt.Errorf("sandbox networking is not supported on %s; requires Linux", runtime.GOOS)
}

func (notSupportedManager) Teardown(_ context.Context, _ netconf.Identity) error {
	return fmt.Errorf("sandbox networking is not supported on %s; requires Linux", runtime.GOOS)
}

var _ Manager = notSupportedManager{}
