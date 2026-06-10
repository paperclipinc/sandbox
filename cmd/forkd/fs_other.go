//go:build !linux

package main

// sameDevice on non-linux platforms always reports true. forkd's real
// engine (and therefore the jailer) only runs on linux; on darwin this
// code path exists solely so the binary builds for development, and the
// linux build enforces the actual same-filesystem requirement.
func sameDevice(a, b string) (bool, error) {
	return true, nil
}
