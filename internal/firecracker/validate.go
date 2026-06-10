package firecracker

import (
	"fmt"
	"regexp"
)

// vmIDPattern admits only ids that cannot contain a path separator or
// traversal sequence, so an id can never escape the chroot base when it is
// joined into a filesystem path. This is the sanitizing barrier for the
// path-injection flows in StartVM and the jailer helpers; it also re-asserts
// the daemon-level validateSandboxID guard at this trust boundary.
var vmIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// validateVMID rejects any VM id that vmIDPattern does not admit. It is the
// CodeQL-recognized allowlist barrier (go/path-injection): because the
// pattern forbids separators and dots, a validated id can never introduce a
// `..` segment or an extra path element into the chroot layout. Every path
// builder that joins the id runs downstream of this check.
func validateVMID(id string) error {
	if !vmIDPattern.MatchString(id) {
		return fmt.Errorf("invalid sandbox id %q: must match %s", id, vmIDPattern.String())
	}
	return nil
}
