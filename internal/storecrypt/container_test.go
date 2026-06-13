package storecrypt

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordedCall captures one runner invocation: the argv and the stdin bytes fed
// to it. The recorder never executes cryptsetup.
type recordedCall struct {
	argv  []string
	stdin []byte
}

// recordingRunner records every call and never runs a real command. failOn, if
// set, makes the runner return an error for the first argv whose first two
// tokens match, so rollback paths can be exercised.
type recordingRunner struct {
	calls  []recordedCall
	failOn string
}

func (r *recordingRunner) run(_ context.Context, argv []string, stdin []byte) error {
	cp := append([]byte(nil), stdin...)
	call := recordedCall{argv: append([]string(nil), argv...), stdin: cp}
	r.calls = append(r.calls, call)
	if r.failOn != "" && verb(call) == r.failOn {
		return errFail
	}
	return nil
}

type sentinelErr struct{}

func (sentinelErr) Error() string { return "forced failure" }

var errFail = sentinelErr{}

// verb returns a stable label for a recorded call: the binary plus, for
// cryptsetup, its subcommand.
func verb(c recordedCall) string {
	if len(c.argv) == 0 {
		return ""
	}
	if c.argv[0] == "cryptsetup" && len(c.argv) >= 2 {
		return "cryptsetup " + c.argv[1]
	}
	return c.argv[0]
}

func verbs(calls []recordedCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = verb(c)
	}
	return out
}

// assertKeyNeverInArgv fails if the key bytes appear in any argv token of any
// recorded call. The key is only ever allowed on stdin.
func assertKeyNeverInArgv(t *testing.T, calls []recordedCall, key []byte) {
	t.Helper()
	for _, c := range calls {
		for _, tok := range c.argv {
			if strings.Contains(tok, string(key)) {
				t.Fatalf("key leaked into argv: %v", c.argv)
			}
		}
	}
}

func TestCreateEmitsLuksSequenceWithKeyOnStdin(t *testing.T) {
	root := t.TempDir()
	mnt := filepath.Join(t.TempDir(), "mount")
	rr := &recordingRunner{}
	m := New(root, root, rr.run)

	key := Key("0123456789abcdef0123456789abcdef")
	if err := m.Create(context.Background(), "tmpl1", key, 256<<20, mnt); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := verbs(rr.calls)
	want := []string{"cryptsetup luksFormat", "cryptsetup luksOpen", "mkfs.ext4", "mount"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Create command order = %v, want %v", got, want)
	}

	// The image file was created under <root>/enc.
	if _, err := os.Stat(filepath.Join(root, "enc", "tmpl1.img")); err != nil {
		t.Fatalf("image file not created: %v", err)
	}

	// The key must be on stdin for luksFormat and luksOpen, and in NO argv.
	for _, c := range rr.calls {
		v := verb(c)
		if v == "cryptsetup luksFormat" || v == "cryptsetup luksOpen" {
			if !bytes.Equal(c.stdin, []byte(key)) {
				t.Fatalf("%s did not receive the key on stdin", v)
			}
		}
	}
	assertKeyNeverInArgv(t, rr.calls, []byte(key))

	// The mapper device and image path are wired correctly.
	formatCall := rr.calls[0]
	if formatCall.argv[len(formatCall.argv)-1] != filepath.Join(root, "enc", "tmpl1.img") {
		t.Fatalf("luksFormat target = %v", formatCall.argv)
	}
	openCall := rr.calls[1]
	if openCall.argv[len(openCall.argv)-1] != "mitos-tmpl1" {
		t.Fatalf("luksOpen mapper name = %v", openCall.argv)
	}
}

func TestCreateRollsBackOnMkfsFailure(t *testing.T) {
	root := t.TempDir()
	mnt := filepath.Join(t.TempDir(), "mount")
	rr := &recordingRunner{failOn: "mkfs.ext4"}
	m := New(root, root, rr.run)

	key := Key("0123456789abcdef0123456789abcdef")
	err := m.Create(context.Background(), "tmpl1", key, 256<<20, mnt)
	if err == nil {
		t.Fatal("expected Create to fail when mkfs fails")
	}
	// Rollback: luksClose was issued and the image removed.
	if !contains(verbs(rr.calls), "cryptsetup luksClose") {
		t.Fatalf("expected luksClose rollback, got %v", verbs(rr.calls))
	}
	if _, statErr := os.Stat(filepath.Join(root, "enc", "tmpl1.img")); !os.IsNotExist(statErr) {
		t.Fatal("image file should have been removed on rollback")
	}
}

func TestCreateRollsBackOnMountFailure(t *testing.T) {
	root := t.TempDir()
	mnt := filepath.Join(t.TempDir(), "mount")
	rr := &recordingRunner{failOn: "mount"}
	m := New(root, root, rr.run)

	key := Key("0123456789abcdef0123456789abcdef")
	err := m.Create(context.Background(), "tmpl1", key, 256<<20, mnt)
	if err == nil {
		t.Fatal("expected Create to fail when mount fails")
	}
	// Rollback: luksClose was issued and the image removed.
	if !contains(verbs(rr.calls), "cryptsetup luksClose") {
		t.Fatalf("expected luksClose rollback, got %v", verbs(rr.calls))
	}
	if _, statErr := os.Stat(filepath.Join(root, "enc", "tmpl1.img")); !os.IsNotExist(statErr) {
		t.Fatal("image file should have been removed on rollback")
	}
}

func TestCloseEmitsUmountAndLuksClose(t *testing.T) {
	root := t.TempDir()
	rr := &recordingRunner{}
	m := New(root, root, rr.run)

	if err := m.Close(context.Background(), "tmpl1", "/mnt/tmpl1"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := verbs(rr.calls)
	want := []string{"umount", "cryptsetup luksClose"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Close commands = %v, want %v", got, want)
	}
}

func TestShredErasesAndRemovesImage(t *testing.T) {
	root := t.TempDir()
	rr := &recordingRunner{}
	m := New(root, root, rr.run)

	// Seed an image so Shred has something to erase.
	encDir := filepath.Join(root, "enc")
	if err := os.MkdirAll(encDir, 0o700); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(encDir, "tmpl1.img")
	if err := os.WriteFile(img, []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.Shred(context.Background(), "tmpl1", "/mnt/tmpl1"); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	got := verbs(rr.calls)
	if !contains(got, "cryptsetup luksErase") {
		t.Fatalf("expected luksErase, got %v", got)
	}
	if _, err := os.Stat(img); !os.IsNotExist(err) {
		t.Fatal("Shred did not remove the image")
	}
}

// TestShredIsIdempotentForMissingContainer proves a missing image is not an
// error and luksErase is not attempted.
func TestShredIsIdempotentForMissingContainer(t *testing.T) {
	root := t.TempDir()
	rr := &recordingRunner{}
	m := New(root, root, rr.run)

	if err := m.Shred(context.Background(), "ghost", "/mnt/ghost"); err != nil {
		t.Fatalf("Shred of missing container should be nil, got %v", err)
	}
	if contains(verbs(rr.calls), "cryptsetup luksErase") {
		t.Fatal("luksErase should not run when there is no image")
	}
}

func TestScopeIDRejectedBeforeAnyCommand(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../x", "a/b", "", ".", "..", "a.b"} {
		rr := &recordingRunner{}
		m := New(root, root, rr.run)
		key := Key("0123456789abcdef0123456789abcdef")
		if err := m.Create(context.Background(), bad, key, 1<<20, "/mnt/x"); err == nil {
			t.Fatalf("Create accepted invalid scope id %q", bad)
		}
		if len(rr.calls) != 0 {
			t.Fatalf("invalid scope id %q reached the runner: %v", bad, rr.calls)
		}
		// No image file should have been created either.
		if _, err := os.Stat(filepath.Join(root, "enc")); err == nil {
			entries, _ := os.ReadDir(filepath.Join(root, "enc"))
			if len(entries) != 0 {
				t.Fatalf("invalid scope id %q created files", bad)
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
