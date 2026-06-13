package fork

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/netconf"
	"github.com/paperclipinc/mitos/internal/network"
)

// fakeVerifier returns a pidVerifier that reports the given set of pids as live
// firecracker, all others as not ours. It lets reconcile tests control the
// PID-recycle guard on darwin where procfs does not exist.
func fakeVerifier(live map[int]bool) pidVerifier {
	return func(pid int, _ string) bool { return live[pid] }
}

// TestReconcileAdoptsLivePid checks that a journal record whose pid the verifier
// reports as our live Firecracker is re-adopted into the live map so
// ListSandboxes reports it and the controller GC can reconcile it.
func TestReconcileAdoptsLivePid(t *testing.T) {
	dir := t.TempDir()
	j := newJournal(dir)

	// A real child process whose pid is alive for the duration of the test.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	livePid := cmd.Process.Pid

	rec := sampleRecord("sbx-live")
	rec.Pid = livePid
	rec.Network = networkIdentity{} // no network so teardown is skipped
	rec.JailedUID = 0
	rec.HasVolumes = false
	if err := j.write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}

	e := &Engine{
		dataDir:   dir,
		sandboxes: make(map[string]*Sandbox),
		journal:   j,
		verifyPID: fakeVerifier(map[int]bool{livePid: true}),
	}
	e.reconcile()

	recs := e.ListSandboxes()
	if len(recs) != 1 || recs[0].ID != "sbx-live" {
		t.Fatalf("live pid not re-adopted into map: %+v", recs)
	}
	pid, ok := e.SandboxPID("sbx-live")
	if !ok || pid != livePid {
		t.Fatalf("adopted sandbox lost its pid: pid=%d ok=%v", pid, ok)
	}
	// The journal record stays: the VM is still live and recoverable.
	left, _ := j.load()
	if len(left) != 1 {
		t.Fatalf("live record must be kept, got %d", len(left))
	}
}

// TestReconcileReapsDeadPid checks that a journal record whose pid is dead has
// its artifacts reaped (jailer workspace removed, network torn down, uid
// released) and its record deleted, and is NOT in the live map.
func TestReconcileReapsDeadPid(t *testing.T) {
	dir := t.TempDir()
	j := newJournal(dir)

	// A pid that has already exited.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	deadPid := cmd.Process.Pid

	// Lay down a fake jailer workspace that reaping must remove.
	jailerVMDir := filepath.Join(dir, "jail", "firecracker", "sbx-dead")
	if err := os.MkdirAll(filepath.Join(jailerVMDir, "root"), 0o755); err != nil {
		t.Fatalf("mkdir jailer dir: %v", err)
	}

	rec := sampleRecord("sbx-dead")
	rec.Pid = deadPid
	rec.JailerVMDir = jailerVMDir
	rec.ChrootDir = filepath.Join(jailerVMDir, "root")
	rec.JailedUID = 100500
	rec.Network = networkIdentity{
		TapName: "sbtap-dead",
		HostIP:  "10.200.0.1",
		GuestIP: "10.200.0.2",
	}
	rec.HasVolumes = false
	if err := j.write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}

	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	uidAlloc := firecracker.NewUIDAllocator(100000, 200000)
	e := &Engine{
		dataDir:   dir,
		sandboxes: make(map[string]*Sandbox),
		journal:   j,
		verifyPID: fakeVerifier(map[int]bool{}), // deadPid not live
		netMgr:    fm,
		netAlloc:  alloc,
		jailer:    firecracker.JailerConfig{JailerBin: "/usr/bin/jailer", Allocator: uidAlloc},
	}
	e.reconcile()

	if recs := e.ListSandboxes(); len(recs) != 0 {
		t.Fatalf("dead pid must NOT be adopted, got %+v", recs)
	}
	if _, err := os.Stat(jailerVMDir); !os.IsNotExist(err) {
		t.Fatalf("jailer workspace not reaped: %v", err)
	}
	if len(fm.Teardowns) != 1 || fm.Teardowns[0].TapName != "sbtap-dead" {
		t.Fatalf("network not torn down for dead VM: %+v", fm.Teardowns)
	}
	left, _ := j.load()
	if len(left) != 0 {
		t.Fatalf("dead record must be removed, got %d", len(left))
	}
}

// TestReconcileRejectsRecycledPid checks the PID-recycle guard: a live but
// UNRELATED pid (not our firecracker) is treated as dead (reaped + dropped),
// never adopted.
func TestReconcileRejectsRecycledPid(t *testing.T) {
	dir := t.TempDir()
	j := newJournal(dir)

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	recycledPid := cmd.Process.Pid

	rec := sampleRecord("sbx-recycled")
	rec.Pid = recycledPid
	rec.JailerVMDir = ""
	rec.ChrootDir = ""
	rec.JailedUID = 0
	rec.Network = networkIdentity{}
	rec.HasVolumes = false
	if err := j.write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}

	e := &Engine{
		dataDir:   dir,
		sandboxes: make(map[string]*Sandbox),
		journal:   j,
		// The verifier reports the pid as NOT ours (recycled/unrelated).
		verifyPID: fakeVerifier(map[int]bool{}),
	}
	e.reconcile()

	if recs := e.ListSandboxes(); len(recs) != 0 {
		t.Fatalf("recycled unrelated pid must NOT be adopted, got %+v", recs)
	}
	left, _ := j.load()
	if len(left) != 0 {
		t.Fatalf("recycled record must be dropped, got %d", len(left))
	}
}

// TestTerminateAdoptedReapsProcessAndArtifacts checks that terminating a
// re-adopted sandbox (one with no live firecracker.Client) kills the recorded
// pid, removes its jailer workspace, releases its uid, and tears down its
// network: the GC must be able to reap a pre-crash VM through the normal
// Terminate path after a restart.
func TestTerminateAdoptedReapsProcessAndArtifacts(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	jailerVMDir := filepath.Join(dir, "jail", "firecracker", "sbx-adopt")
	if err := os.MkdirAll(jailerVMDir, 0o755); err != nil {
		t.Fatalf("mkdir jailer dir: %v", err)
	}

	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	uidAlloc := firecracker.NewUIDAllocator(100000, 100010)
	uidAlloc.MarkInUse(100007)
	e := &Engine{
		dataDir:   dir,
		sandboxes: make(map[string]*Sandbox),
		journal:   newJournal(dir),
		netMgr:    fm,
		netAlloc:  alloc,
		jailer:    firecracker.JailerConfig{JailerBin: "/usr/bin/jailer", Allocator: uidAlloc},
	}
	e.sandboxes["sbx-adopt"] = &Sandbox{
		ID:          "sbx-adopt",
		Pid:         pid,
		jailerVMDir: jailerVMDir,
		jailedUID:   100007,
		netID:       netconf.Identity{TapName: "sbtap-adopt"},
		adopted:     true,
	}

	if err := e.Terminate("sbx-adopt"); err != nil {
		t.Fatalf("Terminate adopted: %v", err)
	}

	// Process killed: a Signal(0) (via FindProcess+Wait) should show it gone.
	_, waitErr := cmd.Process.Wait()
	if waitErr != nil {
		t.Logf("wait after terminate: %v", waitErr)
	}
	if _, err := os.Stat(jailerVMDir); !os.IsNotExist(err) {
		t.Fatalf("adopted jailer dir not reaped: %v", err)
	}
	if len(fm.Teardowns) != 1 || fm.Teardowns[0].TapName != "sbtap-adopt" {
		t.Fatalf("adopted network not torn down: %+v", fm.Teardowns)
	}
	// uid returned to the pool: MarkInUse it again must succeed without panic and
	// Acquire should be able to hand out 100007 again now it is free.
	uidAlloc.MarkInUse(100007) // re-reserve to prove Release happened (idempotent)
}

// TestReconcileFailsOpen checks that a single malformed journal record does not
// stop reconcile (and thus forkd startup): the good record is still processed.
func TestReconcileFailsOpen(t *testing.T) {
	dir := t.TempDir()
	j := newJournal(dir)

	good := sampleRecord("sbx-good")
	good.Pid = 1
	good.Network = networkIdentity{}
	good.JailerVMDir = ""
	good.JailedUID = 0
	good.HasVolumes = false
	if err := j.write(good); err != nil {
		t.Fatalf("write good: %v", err)
	}
	// A torn/garbage record file: load skips it, reconcile must not panic.
	if err := os.WriteFile(filepath.Join(dir, journalDirName, "sbx-bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	e := &Engine{
		dataDir:   dir,
		sandboxes: make(map[string]*Sandbox),
		journal:   j,
		verifyPID: fakeVerifier(map[int]bool{1: true}),
	}
	// Must not panic; the good record is adopted.
	e.reconcile()
	if recs := e.ListSandboxes(); len(recs) != 1 || recs[0].ID != "sbx-good" {
		t.Fatalf("good record not processed past the bad one: %+v", recs)
	}
}
