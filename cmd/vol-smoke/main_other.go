//go:build !linux

// vol-smoke drives the real KVM-backed fork engine with per-fork volumes, which
// needs Firecracker, KVM, and loop-mounting ext4 images, all Linux only. On
// every other platform it is a no-op that exits non-zero so it is never mistaken
// for a passing run; the real proof runs in KVM CI.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "vol-smoke: only supported on Linux (needs KVM + Firecracker + ext4 loop mounts)")
	os.Exit(1)
}
