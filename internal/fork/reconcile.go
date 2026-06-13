package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pidVerifier reports whether the process at pid is GENUINELY our Firecracker
// VM (the one launched with firecrackerBin), as opposed to dead or a recycled,
// unrelated pid. It is the PID-recycle guard: startup reconcile adopts a journal
// record only when the verifier returns true, and reaps (kills nothing, cleans
// artifacts) when it returns false. It is a seam so engine reconcile tests can
// inject a fake on darwin, where procfs does not exist.
type pidVerifier func(pid int, firecrackerBin string) bool

// procfsAvailable reports whether this host exposes the /proc filesystem the
// production verifier reads. False on darwin, where reconcile tests inject a
// fake verifier instead.
func procfsAvailable() bool {
	_, err := os.Stat("/proc/self/exe")
	return err == nil
}

// procfsVerifier is the production PID-recycle guard. It returns true only when:
//   - /proc/<pid> exists (the pid is live), and
//   - the process's executable resolves to the SAME file as firecrackerBin
//     (so a recycled pid running some other program is rejected).
//
// It resolves /proc/<pid>/exe (the canonical, symlink-followed binary path) and
// compares it to the resolved firecrackerBin. When firecrackerBin is empty or
// unresolvable it falls back to a basename match against /proc/<pid>/comm so a
// jailer-launched firecracker (whose argv0 differs) is still recognized, but a
// live unrelated process is never adopted. Any read error is treated as "not
// ours": fail closed on adoption, so a dead or opaque pid is reaped, never
// killed-as-ours when uncertain.
func procfsVerifier(pid int, firecrackerBin string) bool {
	if pid <= 0 {
		return false
	}
	procDir := filepath.Join("/proc", fmt.Sprintf("%d", pid))
	if _, err := os.Stat(procDir); err != nil {
		// No live process at this pid: it is dead (or never ours).
		return false
	}

	exe, err := os.Readlink(filepath.Join(procDir, "exe"))
	if err == nil && exe != "" {
		if resolved, rerr := filepath.EvalSymlinks(firecrackerBin); rerr == nil && resolved != "" {
			if exe == resolved || exe == firecrackerBin {
				return true
			}
		}
		// Even without a resolvable recorded binary, an exe basename of
		// "firecracker" identifies our VMM (the jailer requires the exec-file to
		// be named firecracker; see firecracker.jailerExecFileName).
		if filepath.Base(strings.TrimSuffix(exe, " (deleted)")) == "firecracker" {
			return true
		}
		// Live, but a different executable: a recycled, unrelated pid. Reject.
		return false
	}

	// /proc/<pid>/exe unreadable (e.g. the jailed VM runs under a different uid
	// and forkd is not root, or a kernel that hides it). Fall back to comm, whose
	// basename the kernel sets to the executable name.
	comm, cerr := os.ReadFile(filepath.Join(procDir, "comm"))
	if cerr != nil {
		return false
	}
	return strings.TrimSpace(string(comm)) == "firecracker"
}
