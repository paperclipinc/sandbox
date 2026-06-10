//go:build !linux

// net-smoke drives the real Linux network Manager (taps + nftables), which only
// exists on Linux. On every other platform it is a no-op that exits non-zero so
// it is never mistaken for a passing run; the real proof runs in KVM CI.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "net-smoke: only supported on Linux (needs real nft + iproute2)")
	os.Exit(1)
}
