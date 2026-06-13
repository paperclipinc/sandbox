//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/vsock"
)

func TestPathAllowed(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/workspace", true},
		{"/workspace/", true},
		{"/workspace/sub/dir", true},
		{"/workspace/../workspace", true},
		{"/", false},
		{"/etc", false},
		{"/etc/passwd", false},
		{"/workspace/../etc", false},
		{"/root/.ssh", false},
		{"", false},
		{"workspace", false},
	}
	for _, tc := range cases {
		if got := pathAllowed(tc.path); got != tc.want {
			t.Errorf("pathAllowed(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	dst := "/workspace"
	bad := []string{
		"../escape",
		"../../etc/passwd",
		"/etc/passwd",
		"sub/../../escape",
		"..",
	}
	for _, name := range bad {
		if _, err := safeJoin(dst, name); err == nil {
			t.Errorf("safeJoin(%q, %q) accepted a traversal member; want rejection", dst, name)
		}
	}
	good := map[string]string{
		"a.txt":        "/workspace/a.txt",
		"sub/b.txt":    "/workspace/sub/b.txt",
		"./c.txt":      "/workspace/c.txt",
		"sub/../d.txt": "/workspace/d.txt",
	}
	for name, want := range good {
		got, err := safeJoin(dst, name)
		if err != nil {
			t.Errorf("safeJoin(%q, %q) unexpected error: %v", dst, name, err)
			continue
		}
		if got != want {
			t.Errorf("safeJoin(%q, %q) = %q, want %q", dst, name, got, want)
		}
	}
}

func TestTarDirUntarDirRoundTrip(t *testing.T) {
	src := t.TempDir()
	files := map[string]string{
		"top.txt":        "hello",
		"sub/nested.txt": "world",
		"sub/deep/x.bin": "\x00\x01\x02binary",
		"empty.txt":      "",
	}
	for rel, content := range files {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	data, err := tarDir(src)
	if err != nil {
		t.Fatalf("tarDir: %v", err)
	}

	dst := t.TempDir()
	if err := untarDir(dst, data); err != nil {
		t.Fatalf("untarDir: %v", err)
	}
	for rel, content := range files {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("read %s after round trip: %v", rel, err)
			continue
		}
		if string(got) != content {
			t.Errorf("round trip %s = %q, want %q", rel, got, content)
		}
	}
}

func TestUntarDirRejectsMaliciousMember(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "../escape", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := untarDir(dst, buf.Bytes()); err == nil {
		t.Fatal("untarDir accepted a ../escape member; want rejection")
	}
	// The escape file must not exist anywhere outside dst.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escape")); err == nil {
		t.Fatal("untarDir wrote a file outside the target directory")
	}
}

func TestHandleTarDirAllowlist(t *testing.T) {
	resp := handleTarDir(&vsock.TarDirRequest{Path: "/etc"})
	if resp.OK {
		t.Fatal("handleTarDir allowed /etc; want allowlist rejection")
	}
	resp = handleUntarDir(&vsock.UntarDirRequest{Path: "/etc", Tar: nil})
	if resp.OK {
		t.Fatal("handleUntarDir allowed /etc; want allowlist rejection")
	}
}
