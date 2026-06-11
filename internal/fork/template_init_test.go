package fork

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/paperclipinc/sandbox/internal/cas"
	"github.com/paperclipinc/sandbox/internal/firecracker"
)

// newTestEngine builds an Engine without KVM by populating it directly and
// seaming out the rootfs build and the Firecracker-backed template build, so
// CreateTemplate can be exercised without /dev/kvm or a registry.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	return &Engine{
		dataDir:          t.TempDir(),
		casStore:         store,
		sandboxes:        make(map[string]*Sandbox),
		unverifiedWarned: make(map[string]struct{}),
		templateDigests:  make(map[string]cas.Digest),
		buildRootfsFromImage: func(ctx context.Context, ref, outPath, agentBin, busyboxBin string) error {
			t.Fatalf("buildRootfsFromImage should not run for a file-path template")
			return nil
		},
	}
}

// TestCreateTemplate_InitFailureAbortsBuild is the key safety property: when an
// init command fails, CreateTemplate must return an error and must NOT record a
// template digest (the snapshot is never served). It runs without Firecracker
// by seaming runTemplateBuild to mimic the template manager surfacing an init
// failure.
func TestCreateTemplate_InitFailureAbortsBuild(t *testing.T) {
	e := newTestEngine(t)

	var gotInit []string
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		gotInit = initCommands
		// Mirror the real abort: an init command exited nonzero.
		return runInitCommandsForTest(initCommands)
	}

	err := e.CreateTemplate("py", "/exists/rootfs.ext4", []string{"echo ok", "pip install nope"}, nil)
	if err == nil {
		t.Fatal("expected CreateTemplate to fail when an init command fails")
	}
	if !strings.Contains(err.Error(), "pip install nope") {
		t.Errorf("error should name the failing command: %v", err)
	}
	if len(gotInit) != 2 {
		t.Errorf("init commands not plumbed to the build: %v", gotInit)
	}

	// A failed build must not record a digest (nothing to serve).
	e.mu.Lock()
	_, recorded := e.templateDigests["py"]
	e.mu.Unlock()
	if recorded {
		t.Error("a failed template build must not record a digest")
	}
}

// runInitCommandsForTest stands in for the firecracker template manager's init
// step: it fails on the canned bad command so the engine test does not depend
// on firecracker internals.
func runInitCommandsForTest(cmds []string) error {
	for _, c := range cmds {
		if strings.Contains(c, "nope") {
			return &initErr{cmd: c}
		}
	}
	return nil
}

type initErr struct{ cmd string }

func (e *initErr) Error() string {
	return "template py init failed: init command " + e.cmd + " failed with exit code 1: No matching distribution found"
}

// TestCreateTemplate_FilePathSkipsImageBuild proves the back-compat path: an
// existing file path is treated as a rootfs and never triggers an image pull.
func TestCreateTemplate_FilePathSkipsImageBuild(t *testing.T) {
	e := newTestEngine(t)

	rootfs := t.TempDir() + "/rootfs.ext4"
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}

	var gotCfg firecracker.VMConfig
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		gotCfg = cfg
		return nil
	}

	// recordTemplateDigest will fail (no real snapshot on disk), but the build
	// seam must have been reached with the file path as the rootfs and the
	// image build seam must NOT have run (it t.Fatals if it does).
	_ = e.CreateTemplate("py", rootfs, nil, nil)
	if gotCfg.RootfsPath != rootfs {
		t.Errorf("file-path rootfs not passed through: got %q want %q", gotCfg.RootfsPath, rootfs)
	}
}
