package fork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/volume"
)

// recordingVolRunner captures the argv of each volume backend invocation so the
// engine tests can assert mkfs/cp shape without running real subprocesses.
type recordingVolRunner struct {
	calls [][]string
}

func (r *recordingVolRunner) run(argv []string) error {
	r.calls = append(r.calls, argv)
	return nil
}

// newVolEngine builds an Engine with a volume backend wired to a recording
// runner, rooted at a temp dir, WITHOUT touching /dev/kvm or Firecracker, so the
// volume helpers are unit testable.
func newVolEngine(t *testing.T) (*Engine, *recordingVolRunner) {
	t.Helper()
	rr := &recordingVolRunner{}
	be := volume.NewWithRunner(t.TempDir(), rr.run)
	e := &Engine{
		dataDir:       be.Root(),
		volBackend:    be,
		enableVolumes: true,
	}
	return e, rr
}

func TestPrepareForkVolumesDisabled(t *testing.T) {
	// No backend / flag off: helper returns nil regardless of specs.
	e := &Engine{}
	rebinds, prepared, err := e.prepareForkVolumes("tmpl", "sb1", []volume.Spec{
		{Name: "data", SizeMB: 64, MountPath: "/data", Policy: volume.ForkPolicyFresh},
	})
	if err != nil {
		t.Fatalf("prepareForkVolumes: %v", err)
	}
	if rebinds != nil || prepared != nil {
		t.Errorf("expected nil result when volumes disabled, got rebinds=%v prepared=%v", rebinds, prepared)
	}
}

func TestPrepareForkVolumesNoSpecs(t *testing.T) {
	e, _ := newVolEngine(t)
	rebinds, prepared, err := e.prepareForkVolumes("tmpl", "sb1", nil)
	if err != nil {
		t.Fatalf("prepareForkVolumes: %v", err)
	}
	if rebinds != nil || prepared != nil {
		t.Errorf("expected nil result with no specs, got rebinds=%v prepared=%v", rebinds, prepared)
	}
}

func TestPrepareForkVolumesFreshRebindsRightDrive(t *testing.T) {
	e, rr := newVolEngine(t)
	specs := []volume.Spec{
		{Name: "data", SizeMB: 64, MountPath: "/data", Policy: volume.ForkPolicyFresh},
		{Name: "cache", SizeMB: 32, MountPath: "/cache", Policy: volume.ForkPolicyFresh},
	}
	rebinds, prepared, err := e.prepareForkVolumes("tmpl", "sb1", specs)
	if err != nil {
		t.Fatalf("prepareForkVolumes: %v", err)
	}
	if len(rebinds) != 2 {
		t.Fatalf("expected 2 rebinds, got %d: %+v", len(rebinds), rebinds)
	}
	if len(prepared) != 2 {
		t.Fatalf("expected 2 prepared, got %d", len(prepared))
	}
	// The drive id is the volume name; the backing is the fork's sandbox-scoped
	// ext4. The mapping must line up name -> backing path.
	for i, want := range specs {
		if rebinds[i].DriveID != want.Name {
			t.Errorf("rebind[%d] drive id = %q, want %q", i, rebinds[i].DriveID, want.Name)
		}
		wantPath := filepath.Join(e.dataDir, "sandboxes", "sb1", "volumes", want.Name+".ext4")
		if rebinds[i].PathOnHost != wantPath {
			t.Errorf("rebind[%d] path = %q, want %q", i, rebinds[i].PathOnHost, wantPath)
		}
	}
	// Two Fresh volumes mean two mkfs calls.
	mkfs := 0
	for _, c := range rr.calls {
		if len(c) > 0 && c[0] == "mkfs.ext4" {
			mkfs++
		}
	}
	if mkfs != 2 {
		t.Errorf("expected 2 mkfs calls, got %d: %v", mkfs, rr.calls)
	}
}

func TestPrepareForkVolumesTwoForksDistinctFreshBackings(t *testing.T) {
	e, _ := newVolEngine(t)
	specs := []volume.Spec{{Name: "data", SizeMB: 64, MountPath: "/data", Policy: volume.ForkPolicyFresh}}

	a, _, err := e.prepareForkVolumes("tmpl", "fork-a", specs)
	if err != nil {
		t.Fatalf("prepare a: %v", err)
	}
	b, _, err := e.prepareForkVolumes("tmpl", "fork-b", specs)
	if err != nil {
		t.Fatalf("prepare b: %v", err)
	}
	if a[0].PathOnHost == b[0].PathOnHost {
		t.Errorf("two forks share a Fresh backing: %q", a[0].PathOnHost)
	}
	if !strings.Contains(a[0].PathOnHost, "fork-a") || !strings.Contains(b[0].PathOnHost, "fork-b") {
		t.Errorf("backings not sandbox-scoped: a=%q b=%q", a[0].PathOnHost, b[0].PathOnHost)
	}
}

func TestPrepareForkVolumesSnapshotReflinksTemplateSeedPerFork(t *testing.T) {
	e, rr := newVolEngine(t)
	specs := []volume.Spec{{Name: "data", SizeMB: 64, MountPath: "/data", Policy: volume.ForkPolicySnapshot}}

	a, _, err := e.prepareForkVolumes("tmpl", "fork-a", specs)
	if err != nil {
		t.Fatalf("prepare a: %v", err)
	}
	b, _, err := e.prepareForkVolumes("tmpl", "fork-b", specs)
	if err != nil {
		t.Fatalf("prepare b: %v", err)
	}
	if a[0].PathOnHost == b[0].PathOnHost {
		t.Errorf("two Snapshot forks share a reflink copy: %q", a[0].PathOnHost)
	}
	// The reflink source is the TEMPLATE seed backing for both forks.
	seed := e.volBackend.TemplateVolumePath("tmpl", "data")
	cpCalls := 0
	for _, c := range rr.calls {
		if len(c) >= 3 && c[0] == "cp" {
			cpCalls++
			src := c[len(c)-2]
			if src != seed {
				t.Errorf("cp source = %q, want template seed %q", src, seed)
			}
		}
	}
	if cpCalls != 2 {
		t.Errorf("expected 2 cp (reflink) calls, got %d: %v", cpCalls, rr.calls)
	}
}

func TestPrepareForkVolumesShareIsReadOnlyTemplateSeed(t *testing.T) {
	e, _ := newVolEngine(t)
	specs := []volume.Spec{{Name: "ro", SizeMB: 64, MountPath: "/ro", Policy: volume.ForkPolicyShare}}

	rebinds, prepared, err := e.prepareForkVolumes("tmpl", "fork-a", specs)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	seed := e.volBackend.TemplateVolumePath("tmpl", "ro")
	if rebinds[0].PathOnHost != seed {
		t.Errorf("Share backing = %q, want template seed %q", rebinds[0].PathOnHost, seed)
	}
	if !prepared[0].ReadOnly {
		t.Errorf("Share volume must be read-only")
	}
}

func TestTerminateCleansVolumeBackings(t *testing.T) {
	e, _ := newVolEngine(t)
	e.sandboxes = map[string]*Sandbox{"sb1": {ID: "sb1", hasVolumes: true}}

	if err := e.Terminate("sb1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	// Cleanup is best effort; the assertion is that the sandbox volume dir is
	// gone after Terminate (RemoveAll of a missing path is a no-op success).
	dir := filepath.Join(e.dataDir, "sandboxes", "sb1", "volumes")
	if _, err := filepathGlobExists(dir); err {
		t.Errorf("volume dir %q still present after Terminate", dir)
	}
}

// filepathGlobExists reports whether dir exists. It is a tiny helper kept local
// to the test to avoid importing os just for a stat.
func filepathGlobExists(dir string) (string, bool) {
	matches, _ := filepath.Glob(dir)
	return dir, len(matches) > 0
}

// TestVolumeMountTableDeviceOrdering proves the guest mount table the engine
// builds from the prepared volumes: the i-th volume drive is /dev/vd{b+i}
// (rootfs is /dev/vda), the mount path comes from the spec, and ReadOnly comes
// from the resolved drive policy (Share or explicit readOnly).
func TestVolumeMountTableDeviceOrdering(t *testing.T) {
	prepared := []volume.Prepared{
		{Name: "data", MountPath: "/data", ReadOnly: false},
		{Name: "shared", MountPath: "/shared", ReadOnly: true},
		{Name: "cache", MountPath: "/cache", ReadOnly: false},
	}
	got := volumeMountTable(prepared)
	if len(got) != 3 {
		t.Fatalf("expected 3 mount entries, got %d", len(got))
	}
	wantDev := []string{"/dev/vdb", "/dev/vdc", "/dev/vdd"}
	for i, p := range prepared {
		if got[i].Device != wantDev[i] {
			t.Errorf("entry[%d] device = %q, want %q", i, got[i].Device, wantDev[i])
		}
		if got[i].MountPath != p.MountPath {
			t.Errorf("entry[%d] mountPath = %q, want %q", i, got[i].MountPath, p.MountPath)
		}
		if got[i].ReadOnly != p.ReadOnly {
			t.Errorf("entry[%d] readOnly = %v, want %v", i, got[i].ReadOnly, p.ReadOnly)
		}
	}
}

// TestVolumeMountTableEmpty proves no volumes means a nil table (the guest
// mounts nothing).
func TestVolumeMountTableEmpty(t *testing.T) {
	if got := volumeMountTable(nil); got != nil {
		t.Errorf("expected nil table for no volumes, got %v", got)
	}
}

// TestDriveReadOnlyFromPolicy proves the resolved read-only flag the template
// build bakes into each placeholder drive: a volume is read-only at the drive
// level iff spec.ReadOnly is true OR the resolved policy is Share. Firecracker
// cannot flip is_read_only on a PATCH /drives rebind, so a Share (or any
// readOnly) volume MUST bake is_read_only=true at snapshot time, else a fork
// could write the shared seed.
func TestDriveReadOnlyFromPolicy(t *testing.T) {
	cases := []struct {
		name   string
		spec   volume.Spec
		wantRO bool
	}{
		{"share bakes read-only even when spec.ReadOnly is false",
			volume.Spec{Name: "shared", Policy: volume.ForkPolicyShare}, true},
		{"explicit readOnly bakes read-only",
			volume.Spec{Name: "ro", ReadOnly: true, Policy: volume.ForkPolicyFresh}, true},
		{"fresh writable bakes writable",
			volume.Spec{Name: "data", Policy: volume.ForkPolicyFresh}, false},
		{"snapshot writable bakes writable",
			volume.Spec{Name: "work", Policy: volume.ForkPolicySnapshot}, false},
		{"readOnly share bakes read-only",
			volume.Spec{Name: "rs", ReadOnly: true, Policy: volume.ForkPolicyShare}, true},
	}
	for _, c := range cases {
		if got := driveReadOnly(c.spec); got != c.wantRO {
			t.Errorf("%s: driveReadOnly = %v, want %v", c.name, got, c.wantRO)
		}
	}
}

// TestCreateTemplateBakesReadOnlyForShare proves the build path wires the
// resolved read-only flag into the placeholder drive: a Share volume declared
// WITHOUT spec.ReadOnly still bakes a read-only drive, while a Fresh writable
// volume bakes a writable drive.
func TestCreateTemplateBakesReadOnlyForShare(t *testing.T) {
	dataDir := t.TempDir()
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	rr := &recordingVolRunner{}
	e := &Engine{
		dataDir:          dataDir,
		casStore:         store,
		sandboxes:        make(map[string]*Sandbox),
		unverifiedWarned: make(map[string]struct{}),
		templateDigests:  make(map[string]cas.Digest),
		enableVolumes:    true,
		volBackend:       volume.NewWithRunner(dataDir, rr.run),
	}
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}
	var gotDrives []firecracker.VolumeDrive
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		gotDrives = cfg.VolumeDrives
		return nil
	}

	vols := []volume.Spec{
		// Share declared without spec.ReadOnly: must still bake read-only.
		{Name: "shared", SizeMB: 64, MountPath: "/shared", Policy: volume.ForkPolicyShare},
		// Fresh writable: must bake writable.
		{Name: "scratch", SizeMB: 32, MountPath: "/scratch", Policy: volume.ForkPolicyFresh},
	}
	_ = e.CreateTemplate("tmpl", rootfs, nil, vols)

	if len(gotDrives) != 2 {
		t.Fatalf("expected 2 placeholder drives, got %d", len(gotDrives))
	}
	if !gotDrives[0].ReadOnly {
		t.Errorf("Share volume must bake is_read_only=true even without spec.ReadOnly")
	}
	if gotDrives[1].ReadOnly {
		t.Errorf("Fresh writable volume must bake a writable drive")
	}
}

// TestCreateTemplateBakesPlaceholderDrives proves the build path: a
// volumes-enabled CreateTemplate seeds one template backing per declared volume
// and passes a placeholder VolumeDrive (drive id = volume name, in order) to the
// build so the snapshot bakes the block devices.
func TestCreateTemplateBakesPlaceholderDrives(t *testing.T) {
	dataDir := t.TempDir()
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	rr := &recordingVolRunner{}
	e := &Engine{
		dataDir:          dataDir,
		casStore:         store,
		sandboxes:        make(map[string]*Sandbox),
		unverifiedWarned: make(map[string]struct{}),
		templateDigests:  make(map[string]cas.Digest),
		enableVolumes:    true,
		volBackend:       volume.NewWithRunner(dataDir, rr.run),
		buildRootfsFromImage: func(ctx context.Context, ref, outPath, agentBin, busyboxBin string) error {
			t.Fatalf("buildRootfsFromImage should not run for a file-path template")
			return nil
		},
	}

	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}

	var gotDrives []firecracker.VolumeDrive
	e.runTemplateBuild = func(id string, cfg firecracker.VMConfig, initCommands []string) error {
		gotDrives = cfg.VolumeDrives
		return nil
	}

	vols := []volume.Spec{
		{Name: "data", SizeMB: 64, MountPath: "/data", Policy: volume.ForkPolicyFresh},
		{Name: "cache", SizeMB: 32, MountPath: "/cache", ReadOnly: true, Policy: volume.ForkPolicyShare},
	}
	// recordTemplateDigest fails (no real snapshot), but the build seam must have
	// been reached with the placeholder drives in order.
	_ = e.CreateTemplate("tmpl", rootfs, nil, vols)

	if len(gotDrives) != 2 {
		t.Fatalf("expected 2 placeholder drives, got %d: %+v", len(gotDrives), gotDrives)
	}
	for i, want := range vols {
		if gotDrives[i].DriveID != want.Name {
			t.Errorf("drive[%d] id = %q, want %q (order must match volume order)", i, gotDrives[i].DriveID, want.Name)
		}
		seed := e.volBackend.TemplateVolumePath("tmpl", want.Name)
		if gotDrives[i].PathOnHost != seed {
			t.Errorf("drive[%d] path = %q, want seed %q", i, gotDrives[i].PathOnHost, seed)
		}
		if gotDrives[i].ReadOnly != want.ReadOnly {
			t.Errorf("drive[%d] readOnly = %v, want %v", i, gotDrives[i].ReadOnly, want.ReadOnly)
		}
	}
	// One mkfs per template seed.
	mkfs := 0
	for _, c := range rr.calls {
		if len(c) > 0 && c[0] == "mkfs.ext4" {
			mkfs++
		}
	}
	if mkfs != 2 {
		t.Errorf("expected 2 mkfs (seed) calls, got %d: %v", mkfs, rr.calls)
	}
}
