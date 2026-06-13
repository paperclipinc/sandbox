//go:build !linux

package main

// prepareChrootMount on non-linux platforms is a no-op. The husk stub's jailer
// path only runs on linux (it needs mount(2) and the Firecracker jailer); on
// darwin this exists solely so the binary builds for development, mirroring
// cmd/forkd/fs_other.go.
func prepareChrootMount(chrootBase string) error {
	return nil
}
