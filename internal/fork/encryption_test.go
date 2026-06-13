package fork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/storecrypt"
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

func (f *fakeContainerManager) Shred(_ context.Context, scopeID, mountPoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shreds = append(f.shreds, scopeID)
	f.open[scopeID] = false
	// Mirror the real Shred: unmount + remove image makes the container (and the
	// .encrypted marker that lives inside the mount) disappear, so a retry sees
	// no half-built container and templateEncrypted reports false again.
	if mountPoint != "" {
		_ = os.RemoveAll(mountPoint)
	}
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

// TestCreateTemplateFailedBuildRollsBackContainer proves C1: when a build step
// after createTemplateContainer fails, CreateTemplate shreds/closes the
// container (no open device, no half-built container left behind) and a
// subsequent CreateTemplate for the same id recreates cleanly.
func TestCreateTemplateFailedBuildRollsBackContainer(t *testing.T) {
	fake := newFakeContainerManager()
	e := newEncryptedTestEngine(t, fake)

	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}

	// First build fails AFTER the container was created+mounted.
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		return fmt.Errorf("boom: build failed")
	}
	if err := e.CreateTemplate("tmpl1", rootfs, nil, nil); err == nil {
		t.Fatal("expected CreateTemplate to fail when the build step fails")
	}

	// The container was created then rolled back (shredded), so no open device
	// and no half-built container remain.
	if len(fake.creates) != 1 {
		t.Fatalf("expected exactly one Create, got %v", fake.creates)
	}
	if len(fake.shreds) != 1 || fake.shreds[0] != "tmpl1" {
		t.Fatalf("expected the failed build to shred the container, got shreds=%v", fake.shreds)
	}
	if fake.isOpen("tmpl1") {
		t.Fatal("container left open after a failed build")
	}
	e.mu.RLock()
	_, stillOpen := e.encOpen["tmpl1"]
	e.mu.RUnlock()
	if stillOpen {
		t.Fatal("keep-open record left behind after a failed build")
	}
	if e.templateEncrypted("tmpl1") {
		t.Fatal("encrypted marker left behind after rollback")
	}

	// A retry for the SAME id now succeeds: the container recreates cleanly.
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		writeFakeSnapshot(t, e.dataDir, id)
		return nil
	}
	if err := e.CreateTemplate("tmpl1", rootfs, nil, nil); err != nil {
		t.Fatalf("retry CreateTemplate after rollback: %v", err)
	}
	if len(fake.creates) != 2 {
		t.Fatalf("expected a second Create on retry, got %v", fake.creates)
	}
	if !e.templateEncrypted("tmpl1") {
		t.Fatal("retry did not mark the template encrypted")
	}
}

// TestDeleteTemplateForgetsAndZeroizesKey proves C2: after DeleteTemplate (which
// shreds) the key provider no longer holds the key and the original key bytes
// were zeroized.
func TestDeleteTemplateForgetsAndZeroizesKey(t *testing.T) {
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

	// Hold a reference to the cached key bytes so we can prove they were zeroized
	// in place (the provider shares the underlying array with any caller copy).
	prov := e.keyProvider.(*InMemoryKeyProvider)
	keyCopy, err := prov.KeyFor("tmpl1")
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	if !prov.hasKey("tmpl1") {
		t.Fatal("precondition: provider should hold the key before delete")
	}
	allZero := true
	for _, b := range keyCopy {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("precondition: key should be non-zero before shred")
	}

	if err := e.DeleteTemplate("tmpl1"); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}

	if prov.hasKey("tmpl1") {
		t.Fatal("provider still holds the key after crypto-shred")
	}
	for i, b := range keyCopy {
		if b != 0 {
			t.Fatalf("key byte %d not zeroized after shred: %d", i, b)
		}
	}
}

// TestEnsureTemplateOpenSerializesConcurrentForks proves I1: two concurrent
// ensureTemplateOpen calls for the same template (after a restart, encOpen
// empty) result in exactly one crypt.Open. Run with -race.
func TestEnsureTemplateOpenSerializesConcurrentForks(t *testing.T) {
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

	// Simulate a restart: container closed, keep-open record gone, but the
	// template is marked encrypted on disk.
	_ = fake.Close(context.Background(), "tmpl1", "")
	e.forgetTemplateOpen("tmpl1")
	// Re-create the (fake) mount dir Close removed so reopen-as-mount works; the
	// marker still lives on disk to keep templateEncrypted true.
	if err := os.MkdirAll(templateDir(e.dataDir, "tmpl1"), 0o755); err != nil {
		t.Fatalf("recreate template dir: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = e.ensureTemplateOpen("tmpl1")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensureTemplateOpen goroutine %d: %v", i, err)
		}
	}
	if len(fake.opens) != 1 {
		t.Fatalf("expected exactly one Open under concurrent forks, got %d: %v", len(fake.opens), fake.opens)
	}
}

// TestTerminateDoesNotShredSharedTemplateContainer proves I2: terminating one of
// two sibling forks of an encrypted template never shreds the shared container.
// Only DeleteTemplate shreds.
func TestTerminateDoesNotShredSharedTemplateContainer(t *testing.T) {
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

	// Register two sibling forks of the template. The fork boot path needs
	// Firecracker, so the sandbox records are constructed directly; the property
	// under test (Terminate must not touch the shared container) lives entirely
	// in Terminate, which has no encryption code path.
	e.mu.Lock()
	e.sandboxes["fork-a"] = &Sandbox{ID: "fork-a", TemplateID: "tmpl1", SnapshotID: "tmpl1"}
	e.sandboxes["fork-b"] = &Sandbox{ID: "fork-b", TemplateID: "tmpl1", SnapshotID: "tmpl1"}
	e.mu.Unlock()

	if err := e.Terminate("fork-a"); err != nil {
		t.Fatalf("Terminate fork-a: %v", err)
	}

	if len(fake.shreds) != 0 {
		t.Fatalf("Terminate shredded the shared template container, shreds=%v", fake.shreds)
	}
	if !fake.isOpen("tmpl1") {
		t.Fatal("Terminate closed the shared template container that the sibling fork still needs")
	}

	// The sibling fork is still tracked and the template container survives.
	e.mu.RLock()
	_, siblingAlive := e.sandboxes["fork-b"]
	e.mu.RUnlock()
	if !siblingAlive {
		t.Fatal("sibling fork was removed by terminating its sibling")
	}
}

// TestCreateTemplateFailsClosedWithoutKey proves that with encryption enabled
// and a RequestKeyProvider that holds NO key for the scope, CreateTemplate
// fails rather than silently building a plaintext snapshot, and creates no
// container.
func TestCreateTemplateFailsClosedWithoutKey(t *testing.T) {
	fake := newFakeContainerManager()
	e := newEncryptedTestEngine(t, fake)
	// Swap the in-memory provider (which would generate a key) for a request
	// provider with nothing stashed: the scope has no key.
	e.keyProvider = NewRequestKeyProvider()
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		t.Fatal("build must not run when no key is available (fail closed)")
		return nil
	}
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}
	if err := e.CreateTemplate("tmpl1", rootfs, nil, nil); err == nil {
		t.Fatal("CreateTemplate must fail closed when encryption is on but no key is available")
	}
	if len(fake.creates) != 0 {
		t.Fatalf("no container should be created without a key, got %v", fake.creates)
	}
}

// TestCreateTemplateUsesRequestKey proves the engine uses the request-delivered
// key (stashed via SetKey) on the build path.
func TestCreateTemplateUsesRequestKey(t *testing.T) {
	fake := newFakeContainerManager()
	e := newEncryptedTestEngine(t, fake)
	prov := NewRequestKeyProvider()
	e.keyProvider = prov
	key := storecrypt.Key("0123456789abcdef0123456789abcdef")
	prov.SetKey("tmpl1", key)
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		writeFakeSnapshot(t, e.dataDir, id)
		return nil
	}
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}
	if err := e.CreateTemplate("tmpl1", rootfs, nil, nil); err != nil {
		t.Fatalf("CreateTemplate with request key: %v", err)
	}
	if len(fake.creates) != 1 || fake.creates[0] != "tmpl1" {
		t.Fatalf("expected one Create for tmpl1, got %v", fake.creates)
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
