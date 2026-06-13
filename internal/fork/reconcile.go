package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// reconcile reads the on-disk sandbox journal at forkd startup and, for every
// record, either RE-ADOPTS the still-running VM into the live map (so
// ListSandboxes reports it and the controller GC can reconcile it against the
// CRDs) or REAPS a dead VM's leaked artifacts (jailer chroot, rootfs CoW clone,
// fork network, jailer uid) and drops its record. This closes the forkd-crash
// leak (issue #12): without it a restarted forkd reports zero sandboxes and its
// pre-crash Firecracker processes leak until the node reboots.
//
// It is fail-OPEN: a nil journal, an unreadable journal dir, or a single bad
// record never stops forkd from starting; every orphan found and reaped is
// logged (counts + ids/paths, NEVER secrets) so an operator and the GC have
// visibility. The PID-recycle guard (verifyPID) is critical: a journaled pid is
// adopted ONLY when it is genuinely our live Firecracker, so a recycled,
// unrelated pid is reaped/dropped rather than adopted or wrongly killed.
func (e *Engine) reconcile() {
	if e.journal == nil {
		return
	}
	recs, err := e.journal.load()
	if err != nil {
		// Fail open: log and serve. A reconcile that cannot read the journal must
		// not block startup; new forks still work, only crash recovery is skipped.
		fmt.Fprintf(os.Stderr, "forkd: skip crash reconcile (journal unreadable): %v\n", err)
		return
	}
	if len(recs) == 0 {
		return
	}

	verify := e.verifyPID
	if verify == nil {
		verify = procfsVerifier
	}

	var adopted, reaped int
	for _, rec := range recs {
		if verify(rec.Pid, rec.FirecrackerBin) {
			e.adoptSandbox(rec)
			adopted++
			continue
		}
		// Not our live process: dead or a recycled, unrelated pid. Reap its
		// artifacts (it is NOT killed: a dead pid has nothing to kill, and a
		// recycled pid is not ours to kill) and drop the record.
		e.reapArtifacts(rec)
		if err := e.journal.remove(rec.ID); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: remove reaped journal record %s: %v\n", rec.ID, err)
		}
		reaped++
	}
	fmt.Fprintf(os.Stderr, "forkd: crash reconcile complete: %d sandbox(es) re-adopted, %d orphan(s) reaped\n", adopted, reaped)
}

// adoptSandbox reconstructs enough Sandbox state from a journal record to report
// it through ListSandboxes/GetCapacity and to later Terminate it directly (the
// engine has no live firecracker.Client for a VM it did not launch this
// process). The reconstructed sandbox is marked adopted so Terminate reaps it
// from the recorded pid + paths + identity.
func (e *Engine) adoptSandbox(rec sandboxRecord) {
	sb := &Sandbox{
		ID:          rec.ID,
		TemplateID:  rec.TemplateID,
		SnapshotID:  rec.SnapshotID,
		Endpoint:    rec.Endpoint,
		Pid:         rec.Pid,
		CreatedAt:   rec.CreatedAt,
		VsockPath:   rec.VsockPath,
		rootfsPath:  rec.RootfsPath,
		chrootDir:   rec.ChrootDir,
		jailerVMDir: rec.JailerVMDir,
		jailedUID:   rec.JailedUID,
		netID:       rec.Network.toIdentity(),
		hasVolumes:  rec.HasVolumes,
		adopted:     true,
		// Carry the recorded binary so reapAdopted can RE-VERIFY the pid against
		// it before killing (the adoption-then-kill TOCTOU guard).
		firecrackerBin: rec.FirecrackerBin,
	}
	sb.MemoryUnique, sb.MemoryShared = readMemoryStats(sb.Pid)

	// Re-mark the adopted VM's jailer uid as in use so a fresh fork cannot hand
	// the same uid to a new VM while this one still runs. Best effort: the
	// allocator may be nil (direct-exec) or the uid zero.
	if rec.JailedUID != 0 && e.jailer.Allocator != nil {
		e.jailer.Allocator.MarkInUse(rec.JailedUID)
	}
	// Re-mark the network identity as in use so a fresh fork cannot collide on the
	// adopted VM's tap/IP. The in-memory allocator is empty after a restart, so
	// Acquire would hand out the FIRST free /30, almost never the block this live
	// VM actually holds (the guest IP derives from the block index). MarkInUse
	// pins the EXACT recorded block from the recorded guest IP, so a later fresh
	// fork cannot be handed the same /30 and Release frees the right block.
	if sb.netID.TapName != "" && e.netAlloc != nil {
		if err := e.netAlloc.MarkInUse(rec.ID, sb.netID); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: mark adopted network in use for %s: %v\n", rec.ID, err)
		}
	}

	fmt.Fprintf(os.Stderr, "forkd: re-adopted pre-crash sandbox %s (pid %d, template %s)\n", rec.ID, rec.Pid, rec.TemplateID)

	e.mu.Lock()
	e.sandboxes[rec.ID] = sb
	e.mu.Unlock()
}

// reapArtifacts removes a dead VM's leaked host artifacts: the jailer workspace
// (chroot + CoW rootfs clone live under it), the sandbox working directory, the
// fork network (tap + ruleset + identity), and the jailer uid. It is best-effort
// and idempotent: an already-gone artifact is not an error. Paths are logged,
// never secrets. The recorded pid is NOT signaled here: a dead pid has nothing
// to kill, and an unrelated recycled pid is not ours to kill.
func (e *Engine) reapArtifacts(rec sandboxRecord) {
	// Jailer workspace (parent of the chroot, holds the CoW rootfs hard link).
	if rec.JailerVMDir != "" {
		if err := os.RemoveAll(rec.JailerVMDir); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: reap jailer dir %s for %s: %v\n", rec.JailerVMDir, rec.ID, err)
		}
	}
	// Sandbox working dir (direct-exec rootfs clone, vsock socket, checkpoints).
	sandboxDir := filepath.Join(e.dataDir, "sandboxes", rec.ID)
	if err := os.RemoveAll(sandboxDir); err != nil {
		fmt.Fprintf(os.Stderr, "forkd: reap sandbox dir %s for %s: %v\n", sandboxDir, rec.ID, err)
	}
	// Fork network: tear down the tap + egress ruleset and release the identity.
	if rec.Network.TapName != "" && e.networkEnabled() {
		e.teardownForkNetwork(rec.ID, rec.Network.toIdentity())
	}
	// Per-fork volume backings.
	if rec.HasVolumes && e.volBackend != nil {
		if err := e.volBackend.Cleanup(rec.ID); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: reap volumes for %s: %v\n", rec.ID, err)
		}
	}
	// Jailer uid back to the pool.
	if rec.JailedUID != 0 && e.jailer.Allocator != nil {
		e.jailer.Allocator.Release(rec.JailedUID)
	}
	fmt.Fprintf(os.Stderr, "forkd: reaped orphan sandbox %s (pid %d, jailerDir %q, tap %q)\n", rec.ID, rec.Pid, rec.JailerVMDir, rec.Network.TapName)
}

// reapAdopted reaps a sandbox that was re-adopted from the journal (no live
// firecracker.Client) when the controller GC later terminates it. It reaps the
// leaked filesystem artifacts and the jailer uid, then leaves network/volume
// teardown to Terminate, which already runs it for every sandbox.
//
// The kill is the single most dangerous operation in the codebase: an adopted
// firecracker is re-parented to init across the forkd crash (it is NOT a child
// of this restarted forkd), so killing its bare recorded pid is unconditional.
// Adoption happens at startup but Terminate runs a full GC interval later;
// between the two the adopted VM can exit on its own (guest poweroff, OOM, FC
// crash), init reaps it, the kernel frees the pid, and on a busy node that pid
// can be RECYCLED to an unrelated process. To avoid SIGKILLing that unrelated
// process we RE-RUN the SAME PID-recycle guard used at adoption, against the
// recorded firecracker binary, and skip the kill when the pid no longer
// resolves to OUR firecracker. Artifacts are still reaped and state still
// dropped (by Terminate) in that case: a recycled pid means our VM is long
// gone, so its leaked host artifacts must go regardless.
func (e *Engine) reapAdopted(sb *Sandbox) {
	if sb.Pid > 0 {
		verify := e.verifyPID
		if verify == nil {
			verify = procfsVerifier
		}
		if !verify(sb.Pid, sb.firecrackerBin) {
			// The recorded pid is dead or recycled to an unrelated process. Do NOT
			// signal it: a dead pid has nothing to kill, and a recycled pid is not
			// ours to kill. Fall through to artifact reaping below.
			fmt.Fprintf(os.Stderr, "forkd: skip kill of adopted sandbox %s (pid %d no longer our firecracker)\n", sb.ID, sb.Pid)
		} else if proc, err := os.FindProcess(sb.Pid); err == nil {
			if kerr := proc.Kill(); kerr != nil && !isProcessGone(kerr) {
				fmt.Fprintf(os.Stderr, "forkd: kill adopted sandbox %s (pid %d): %v\n", sb.ID, sb.Pid, kerr)
			}
		}
	}
	if sb.jailerVMDir != "" {
		if err := os.RemoveAll(sb.jailerVMDir); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: remove jailer dir %s for adopted %s: %v\n", sb.jailerVMDir, sb.ID, err)
		}
	}
	if sb.jailedUID != 0 && e.jailer.Allocator != nil {
		e.jailer.Allocator.Release(sb.jailedUID)
	}
}

// isProcessGone reports whether a kill error means the process was already gone
// (ESRCH or the os "process already finished" sentinel), which is not a failure
// for reaping.
func isProcessGone(err error) bool {
	if err == nil {
		return true
	}
	if err == os.ErrProcessDone {
		return true
	}
	return strings.Contains(err.Error(), "process already finished") ||
		strings.Contains(err.Error(), syscall.ESRCH.Error())
}

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
	//
	// RESIDUAL RISK (accepted): comm is weaker than exe. An attacker (or sheer
	// bad luck) could in principle win a recycle race onto our exact recorded pid
	// AND have that unrelated process named "firecracker" (renamed comm), which
	// this fallback would then accept as ours. That requires hitting a specific
	// pid in the narrow adoption/Terminate window AND a matching comm, so it is a
	// far weaker hazard than the exe path it backstops. We keep the fallback
	// because the common deployment (jailed VM under a non-root uid, forkd not
	// root) makes /proc/<pid>/exe genuinely unreadable, and refusing to adopt
	// there would leak every jailed VM across a restart. reapAdopted runs this
	// SAME verifier before killing, so the kill path is no weaker than adoption.
	comm, cerr := os.ReadFile(filepath.Join(procDir, "comm"))
	if cerr != nil {
		return false
	}
	return strings.TrimSpace(string(comm)) == "firecracker"
}
