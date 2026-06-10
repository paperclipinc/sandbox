package daemon

import (
	"fmt"
	"regexp"
)

// sandboxIDPattern constrains every caller-supplied id (sandbox, snapshot,
// template) that forkd later embeds in host filesystem paths: workspace
// dirs, snapshot files, and the jailer chroot layout. No dots and no
// separators, so a validated id can never introduce a `..` segment or an
// extra path element; this is the gRPC-boundary half of the C1 traversal
// defense (the firecracker package independently refuses escaping paths).
var sandboxIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// validateSandboxID rejects ids that fail sandboxIDPattern before any
// engine or filesystem operation sees them.
func validateSandboxID(s string) error {
	if !sandboxIDPattern.MatchString(s) {
		return fmt.Errorf("invalid id %q: ids must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes); use a plain identifier such as sb-1234", s)
	}
	return nil
}
