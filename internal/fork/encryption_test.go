package fork

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/paperclipinc/sandbox/internal/cas"
	"github.com/paperclipinc/sandbox/internal/firecracker"
	"github.com/paperclipinc/sandbox/internal/storecrypt"
)

// fakeContainerManager records Create/Open/Close/Shred calls and simulates a
// mounted container by ensuring the mount point directory exists (so the
// engine's snapshot write/read logic runs against a real directory without
// dm-crypt). It records the scopes it considers "open" so the keep-open logic
// can be asserted.
type fakeContainerManager struct {
	mu       sync.Mutex
	creates  []string
	opens    []string
	closes   []string
	shreds   []string
	open     map[string]bool
	createdN map[string]int
}

func newFakeContainerManager() *fakeContainerManager {
	return &fakeContainerManager{open: map[string]bool{}, createdN: map[string]int{}}
}

func (f *fakeContainerManager) Create(_ context.Context, scopeID string, _ storecrypt.Key, _ int64, mountPoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, scopeID)
	f.createdN[scopeID]++
	f.open[scopeID] = true
	return os.MkdirAll(mountPoint, 0o755)
}

func (f *fakeContainerManager) Open(_ context.Context, scopeID string, _ storecrypt.Key, mountPoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, scopeID)
	f.open[scopeID] = true
	return os.MkdirAll(mountPoint, 0o755)
}

func (f *fakeContainerManager) Close(_ context.Context, scopeID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, scopeID)
	f.open[scopeID] = false
	return nil
}

func (f *fakeContainerManager) Shred(_ context.Context, scopeID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shreds = append(f.shreds, scopeID)
	f.open[scopeID] = false
	return nil
}

func (f *fakeContainerManager) isOpen(scopeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[scopeID]
}

// newEncryptedTestEngine builds an Engine with encryption enabled and the fake
// container manager + in-memory key provider, seaming out the rootfs and
// Firecracker build so CreateTemplate runs without KVM. runTemplateBuild writes
// a fake snapshot into the (fake) mounted template dir so the snapshot
// write/read logic is exercised.
func newEncryptedTestEngine(t *testing.T, fake *fakeContainerManager) *Engine {
	t.Helper()
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	dataDir := t.TempDir()
	e := &Engine{
		dataDir:          dataDir,
		casStore:         store,
		sandboxes:        make(map[string]*Sandbox),
		unverifiedWarned: make(map[string]struct{}),
		templateDigests:  make(map[string]cas.Digest),
		templateMgr:      firecracker.NewTemplateManager("", "", dataDir, firecracker.JailerConfig{}),
		enableEncryption: true,
		crypt:            fake,
		keyProvider:      NewInMemoryKeyProvider(),
		encOpen:          make(map[string]struct{}),
		buildRootfsFromImage: func(ctx context.Context, ref, outPath, agentBin, busyboxBin string) error {
			t.Fatalf("buildRootfsFromImage should not run for a file-path template")
			return nil
		},
	}
	return e
}

// writeFakeSnapshot writes a minimal snapshot into the template dir so
// recordTemplateDigest and Fork have files to operate on.
func writeFakeSnapshot(t *testing.T, dataDir, id string) {
	t.Helper()
	dir := filepath.Join(dataDir, "templates", id, "snapshot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mem"), []byte("mem-bytes"), 0o644); err != nil {
		t.Fatalf("write mem: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vmstate"), []byte("vmstate-bytes"), 0o644); err != nil {
		t.Fatalf("write vmstate: %v", err)
	}
}

func TestCreateTemplateEncryptedCreatesContainerAndWritesInside(t *testing.T) {
	fake := newFakeContainerManager()
	e := newEncryptedTestEngine(t, fake)

	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		// The container is mounted at the template dir; write the snapshot there.
		writeFakeSnapshot(t, e.dataDir, id)
		return nil
	}

	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}

	if err := e.CreateTemplate("tmpl1", rootfs, nil, nil); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	if len(fake.creates) != 1 || fake.creates[0] != "tmpl1" {
		t.Fatalf("expected one Create for scope tmpl1, got %v", fake.creates)
	}
	// The snapshot was written inside the (fake) mount, i.e. the template dir.
	if _, err := os.Stat(filepath.Join(e.dataDir, "templates", "tmpl1", "snapshot", "mem")); err != nil {
		t.Fatalf("snapshot not written inside the mounted template dir: %v", err)
	}
	// The encrypted marker was recorded so Fork knows to open the container.
	if !e.templateEncrypted("tmpl1") {
		t.Fatal("template not marked encrypted")
	}
	// The digest was recorded (build succeeded end to end).
	e.mu.Lock()
	_, ok := e.templateDigests["tmpl1"]
	e.mu.Unlock()
	if !ok {
		t.Fatal("template digest not recorded")
	}
}

func TestForkEncryptedOpensContainerWhenNotOpen(t *testing.T) {
	fake := newFakeContainerManager()
	e := newEncryptedTestEngine(t, fake)
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		writeFakeSnapshot(t, e.dataDir, id)
		return nil
	}
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}
	if err := e.CreateTemplate("tmpl1", rootfs, nil, nil); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	// Simulate a restart: the container is no longer open and the engine's
	// in-memory keep-open record is gone, but the template is marked encrypted
	// on disk. ensureTemplateOpen must Open it.
	_ = fake.Close(context.Background(), "tmpl1", "")
	e.forgetTemplateOpen("tmpl1")
	if fake.isOpen("tmpl1") {
		t.Fatal("precondition: container should be closed")
	}

	if err := e.ensureTemplateOpen("tmpl1"); err != nil {
		t.Fatalf("ensureTemplateOpen: %v", err)
	}
	if !fake.isOpen("tmpl1") {
		t.Fatal("Fork path did not open the encrypted container")
	}
	if len(fake.opens) != 1 {
		t.Fatalf("expected exactly one Open, got %v", fake.opens)
	}

	// A second ensure is a no-op while the container stays open (keep-open).
	if err := e.ensureTemplateOpen("tmpl1"); err != nil {
		t.Fatalf("ensureTemplateOpen second call: %v", err)
	}
	if len(fake.opens) != 1 {
		t.Fatalf("keep-open violated: Open called again, opens=%v", fake.opens)
	}
}

func TestDeleteTemplateShredsContainer(t *testing.T) {
	fake := newFakeContainerManager()
	e := newEncryptedTestEngine(t, fake)
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		writeFakeSnapshot(t, e.dataDir, id)
		return nil
	}
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}
	if err := e.CreateTemplate("tmpl1", rootfs, nil, nil); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	if err := e.DeleteTemplate("tmpl1"); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if len(fake.shreds) != 1 || fake.shreds[0] != "tmpl1" {
		t.Fatalf("expected Shred of tmpl1, got %v", fake.shreds)
	}
}

// TestEncryptionDisabledCreatesNoContainer proves the off path is unchanged: no
// container is created and no encrypted marker is written.
func TestEncryptionDisabledCreatesNoContainer(t *testing.T) {
	e := newTestEngine(t)
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		writeFakeSnapshot(t, e.dataDir, id)
		return nil
	}
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}
	if err := e.CreateTemplate("tmpl1", rootfs, nil, nil); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if e.crypt != nil {
		t.Fatal("encryption disabled but a container manager is set")
	}
	if e.templateEncrypted("tmpl1") {
		t.Fatal("template marked encrypted with encryption disabled")
	}
	// ensureTemplateOpen is a no-op when encryption is off.
	if err := e.ensureTemplateOpen("tmpl1"); err != nil {
		t.Fatalf("ensureTemplateOpen with encryption off: %v", err)
	}
}

func TestInMemoryKeyProviderCachesPerScope(t *testing.T) {
	p := NewInMemoryKeyProvider()
	k1, err := p.KeyFor("a")
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	k1again, err := p.KeyFor("a")
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	if string(k1) != string(k1again) {
		t.Fatal("KeyFor returned a different key for the same scope")
	}
	k2, err := p.KeyFor("b")
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	if string(k1) == string(k2) {
		t.Fatal("KeyFor returned the same key for different scopes")
	}
}
