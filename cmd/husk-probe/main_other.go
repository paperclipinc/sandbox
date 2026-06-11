//go:build !linux

// husk-probe drives the real KVM-backed fork engine and reads Linux cgroup v2
// and /proc accounting, so it only builds and runs on Linux. This stub keeps
// the darwin build and lint green.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "husk-probe is linux-only")
	os.Exit(1)
}
