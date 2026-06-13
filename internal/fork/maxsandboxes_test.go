package fork

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/firecracker"
)

// TestAdmitForkAtCapacity verifies the O(1) admission ceiling: once the live
// sandbox count reaches MaxSandboxes the engine refuses a new fork with the
// typed ErrAtCapacity BEFORE allocating or booting anything. A runaway tenant
// can therefore never exhaust the node by opening forks; the check is under the
// engine lock and off the boot path (it runs before any Firecracker work).
func TestAdmitForkAtCapacity(t *testing.T) {
	e := &Engine{
		sandboxes:    make(map[string]*Sandbox),
		maxSandboxes: 2,
	}
	e.sandboxes["a"] = &Sandbox{ID: "a"}
	e.sandboxes["b"] = &Sandbox{ID: "b"}

	if err := e.admitFork(); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("at the cap admitFork must return ErrAtCapacity, got %v", err)
	}

	// Below the cap a fork is admitted (and the slot it reserves is released so
	// the count is not left dangling).
	delete(e.sandboxes, "b")
	if err := e.admitFork(); err != nil {
		t.Fatalf("below the cap admitFork must succeed, got %v", err)
	}
	e.releaseReservation()
	if e.reserved != 0 {
		t.Fatalf("after releaseReservation reserved must be 0, got %d", e.reserved)
	}
}

// TestAdmitForkReservationBlocksOvershoot verifies the admission check and the
// slot reservation are a single atomic step: a reserved-but-not-yet-inserted
// fork (in flight, mid-boot) counts against the ceiling, so a second admit at
// len==max-1 with one slot already reserved is refused. This is the property
// that stops N concurrent forks from overshooting MaxSandboxes.
func TestAdmitForkReservationBlocksOvershoot(t *testing.T) {
	e := &Engine{
		sandboxes:    make(map[string]*Sandbox),
		maxSandboxes: 2,
	}
	e.sandboxes["a"] = &Sandbox{ID: "a"}

	// One slot free: first admit reserves it.
	if err := e.admitFork(); err != nil {
		t.Fatalf("first admit below cap must succeed, got %v", err)
	}
	// len(sandboxes)=1 + reserved=1 == max=2: the next admit must be refused
	// even though nothing has been inserted yet.
	if err := e.admitFork(); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("second admit must be refused while a slot is reserved, got %v", err)
	}
}

// TestCommitReservationAtomic verifies the successful insert path swaps a
// reservation for a live sandbox under one lock: reserved drops by one as the
// sandbox enters the map, so len(sandboxes)+reserved is unchanged across the
// commit (no transient window where the cap is under- or over-counted).
func TestCommitReservationAtomic(t *testing.T) {
	e := &Engine{
		sandboxes:    make(map[string]*Sandbox),
		maxSandboxes: 4,
	}
	if err := e.admitFork(); err != nil {
		t.Fatalf("admit must succeed, got %v", err)
	}
	if e.reserved != 1 {
		t.Fatalf("after admit reserved must be 1, got %d", e.reserved)
	}
	e.commitReservation("x", &Sandbox{ID: "x"})
	if e.reserved != 0 {
		t.Fatalf("after commit reserved must be 0, got %d", e.reserved)
	}
	if _, ok := e.sandboxes["x"]; !ok {
		t.Fatalf("commit must insert the sandbox into the live map")
	}
}

// TestReleaseReservationIsLeakProof verifies a fork that fails after admission
// (boot error, volume error, etc.) releases its reserved slot, so the cap does
// not permanently shrink. After a reserve+release the engine admits again right
// up to MaxSandboxes.
func TestReleaseReservationIsLeakProof(t *testing.T) {
	e := &Engine{
		sandboxes:    make(map[string]*Sandbox),
		maxSandboxes: 1,
	}
	// Reserve the only slot, then simulate a boot failure that rolls back.
	if err := e.admitFork(); err != nil {
		t.Fatalf("admit must succeed, got %v", err)
	}
	e.releaseReservation()
	if e.reserved != 0 {
		t.Fatalf("release must return reserved to 0, got %d", e.reserved)
	}
	// The slot is free again: a subsequent fork below the cap is admitted.
	if err := e.admitFork(); err != nil {
		t.Fatalf("after a rolled-back fork the freed slot must be admittable, got %v", err)
	}
}

// TestConcurrentForkDoesNotOvershoot is the TOCTOU regression test (run with
// -race): N goroutines race admitFork against an engine sitting at
// MaxSandboxes-1. Exactly one must win the last slot; the rest must get
// ErrAtCapacity. The winner then commits a sandbox (the success path) and the
// losers release nothing (they never reserved). Afterwards reserved is 0 (no
// leak) and len(sandboxes) never exceeded MaxSandboxes.
func TestConcurrentForkDoesNotOvershoot(t *testing.T) {
	const max = 8
	const racers = 64

	e := &Engine{
		sandboxes:    make(map[string]*Sandbox),
		maxSandboxes: max,
	}
	for i := 0; i < max-1; i++ {
		id := "seed-" + string(rune('A'+i))
		e.sandboxes[id] = &Sandbox{ID: id}
	}

	var wins int64
	var caps int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := e.admitFork()
			if err == nil {
				atomic.AddInt64(&wins, 1)
				// Simulate the successful boot+insert: commit the reservation.
				e.commitReservation("won-"+string(rune(i)), &Sandbox{ID: "won"})
				return
			}
			if errors.Is(err, ErrAtCapacity) {
				atomic.AddInt64(&caps, 1)
				return
			}
			t.Errorf("unexpected admit error: %v", err)
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one fork must win the last slot, got %d wins", wins)
	}
	if caps != racers-1 {
		t.Fatalf("the remaining %d forks must get ErrAtCapacity, got %d", racers-1, caps)
	}
	if e.reserved != 0 {
		t.Fatalf("reserved must return to 0 after the race (no leak), got %d", e.reserved)
	}
	if len(e.sandboxes) != max {
		t.Fatalf("live sandboxes must equal MaxSandboxes=%d, got %d", max, len(e.sandboxes))
	}
}

// TestAdmitForkUnlimited verifies MaxSandboxes<=0 disables the ceiling (the
// prior behavior: GetCapacity reported the number but Fork never enforced it).
func TestAdmitForkUnlimited(t *testing.T) {
	e := &Engine{
		sandboxes:    make(map[string]*Sandbox),
		maxSandboxes: 0,
	}
	for i := 0; i < 1000; i++ {
		e.sandboxes[string(rune(i))] = &Sandbox{}
	}
	if err := e.admitFork(); err != nil {
		t.Fatalf("with MaxSandboxes=0 admitFork must never reject, got %v", err)
	}
}

// TestGetCapacityReportsMaxSandboxes verifies the configured ceiling is
// surfaced to the controller via GetCapacity (it was wired into the struct but
// the real engine never populated it).
func TestGetCapacityReportsMaxSandboxes(t *testing.T) {
	dataDir := t.TempDir()
	e := &Engine{
		sandboxes:       make(map[string]*Sandbox),
		templateMgr:     firecracker.NewTemplateManager("fc", "vmlinux", dataDir, firecracker.JailerConfig{}),
		templateDigests: make(map[string]cas.Digest),
		maxSandboxes:    42,
		meminfoReader: func() (string, error) {
			return "MemTotal: 100 kB\n", nil
		},
	}
	if got := e.GetCapacity().MaxSandboxes; got != 42 {
		t.Fatalf("GetCapacity().MaxSandboxes: got %d want 42", got)
	}
}
