package fork

import (
	"os/exec"
	"testing"
)

// TestProcfsVerifierLiveUnrelated checks the PID-recycle guard: a live process
// whose executable is NOT the recorded firecracker binary must be reported as
// "not ours" so it is never adopted or killed. We spawn a real child (sleep)
// and journal it as if it were a firecracker VM whose binary was /usr/bin/
// firecracker; the verifier must reject it.
func TestProcfsVerifierLiveUnrelated(t *testing.T) {
	if !procfsAvailable() {
		t.Skip("procfs not available on this platform")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	v := procfsVerifier
	// sleep is alive, but its exe is not /usr/bin/firecracker, so it is NOT ours.
	if v(cmd.Process.Pid, "/usr/bin/firecracker") {
		t.Fatalf("verifier adopted an unrelated live pid as firecracker")
	}
}

// TestProcfsVerifierDeadPid checks that a pid with no live process is reported
// as not ours (it is dead, its artifacts must be reaped).
func TestProcfsVerifierDeadPid(t *testing.T) {
	if !procfsAvailable() {
		t.Skip("procfs not available on this platform")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	dead := cmd.Process.Pid
	if procfsVerifier(dead, "/usr/bin/firecracker") {
		t.Fatalf("verifier treated a dead pid as a live firecracker")
	}
}
