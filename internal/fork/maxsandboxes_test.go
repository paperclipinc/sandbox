package fork

import (
	"errors"
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

	// Below the cap a fork is admitted.
	delete(e.sandboxes, "b")
	if err := e.admitFork(); err != nil {
		t.Fatalf("below the cap admitFork must succeed, got %v", err)
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
