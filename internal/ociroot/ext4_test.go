package ociroot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildExt4Argv(t *testing.T) {
	srcDir := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "rootfs.ext4")

	var got []string
	b := &Ext4Builder{Runner: func(argv []string) error {
		got = argv
		return nil
	}}

	if err := b.BuildExt4(srcDir, outPath, 128); err != nil {
		t.Fatalf("BuildExt4: %v", err)
	}

	want := []string{"mkfs.ext4", "-F", "-q", "-d", srcDir, outPath, "128M"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestBuildExt4PropagatesRunnerError(t *testing.T) {
	b := &Ext4Builder{Runner: func(argv []string) error {
		return fmt.Errorf("boom")
	}}
	err := b.BuildExt4(t.TempDir(), filepath.Join(t.TempDir(), "out.ext4"), 64)
	if err == nil {
		t.Fatal("BuildExt4 should propagate runner error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to wrap runner error", err)
	}
}

func TestBuildExt4FuncDefault(t *testing.T) {
	// The package-level BuildExt4 helper must work with an injected runner.
	var got []string
	err := BuildExt4WithRunner(t.TempDir(), filepath.Join(t.TempDir(), "o.ext4"), 64, func(argv []string) error {
		got = argv
		return nil
	})
	if err != nil {
		t.Fatalf("BuildExt4WithRunner: %v", err)
	}
	if len(got) == 0 || got[0] != "mkfs.ext4" {
		t.Errorf("argv = %v, want it to start with mkfs.ext4", got)
	}
}

func TestDirSizeMBHasHeadroomAndFloor(t *testing.T) {
	dir := t.TempDir()

	// Empty dir should hit the floor.
	size, err := DirSizeMB(dir)
	if err != nil {
		t.Fatalf("DirSizeMB empty: %v", err)
	}
	if size < minRootfsMB {
		t.Errorf("empty dir size = %d, want >= floor %d", size, minRootfsMB)
	}

	// Write ~4 MiB of content; expect at least content*1.4 worth of headroom,
	// which is comfortably above the floor.
	const contentBytes = 200 * 1024 * 1024
	big := make([]byte, contentBytes)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	size, err = DirSizeMB(dir)
	if err != nil {
		t.Fatalf("DirSizeMB big: %v", err)
	}

	contentMB := contentBytes / (1024 * 1024)
	if size <= contentMB {
		t.Errorf("size = %d, want headroom above content %d MB", size, contentMB)
	}
	// 1.4x headroom: 200 MiB -> at least 280 MB.
	if size < 280 {
		t.Errorf("size = %d, want at least 280 (content*1.4)", size)
	}
}
