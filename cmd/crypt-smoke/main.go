// Command crypt-smoke is a small CI helper that drives the REAL
// internal/storecrypt Manager (with the production DefaultRunner, so the
// actual cryptsetup code path is exercised, not just raw cryptsetup) to prove
// encryption at rest, decrypt/restore through the mount, and crypto-shred.
//
// It deliberately reuses storecrypt.Manager.Create/Open/Close/Shred so the CI
// gate runs the same package code that forkd's encryption integration uses.
// The key is generated once and persisted to a key file ONLY for the duration
// of a single smoke run (it is the test key, never a production secret); it is
// fed to cryptsetup via storecrypt's stdin discipline and is never placed in
// argv. The key value is never logged: only key lengths and operation names
// are printed.
//
// Modes (each takes -scope, -root, -mount; -key-file holds the generated key
// between invocations of one smoke run):
//
//	gen-key     generate a fresh 256-bit key and write it to -key-file.
//	create      Manager.Create a container for -scope sized -size-mib, mounted
//	            at -mount, then write -marker into -marker-file inside the mount.
//	probe-open  assert -marker is present in -marker-file while the container is
//	            still mounted (the control: the marker string IS findable in the
//	            decrypted view), then Close.
//	grep-raw    grep the raw <root>/enc/<scope>.img for -marker; exit 0 only if
//	            the marker is ABSENT on the raw device (ciphertext at rest).
//	reopen-read Manager.Open the container with the key, mount it, and assert
//	            -marker reads back intact from -marker-file (decrypt/restore).
//	close       Manager.Close the container.
//	shred       Manager.Shred the container (luksErase + remove img).
//	reopen-fail attempt Manager.Open with the key; exit 0 only if it FAILS and
//	            the img is gone (crypto-shred made the data unrecoverable).
//
// Every mode prints a clear stdout line and exits nonzero on any failure so the
// CI step can gate on it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/paperclipinc/mitos/internal/storecrypt"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: crypt-smoke <mode> [flags]")
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	var (
		root       = fs.String("root", "", "storecrypt root (holds enc/<scope>.img)")
		scope      = fs.String("scope", "", "scope id for the container")
		mount      = fs.String("mount", "", "mount point for the container")
		keyFile    = fs.String("key-file", "", "path holding the generated test key between invocations")
		markerFile = fs.String("marker-file", "marker.txt", "file (relative to mount) holding the marker")
		marker     = fs.String("marker", "", "recognizable plaintext marker")
		sizeMiB    = fs.Int64("size-mib", 64, "container size in MiB for create")
	)
	_ = fs.Parse(os.Args[2:])

	ctx := context.Background()
	mgr := storecrypt.New(*root, filepath.Join(*root, "mounts"), storecrypt.DefaultRunner)

	switch mode {
	case "gen-key":
		genKey(*keyFile)
	case "create":
		create(ctx, mgr, *scope, *mount, *keyFile, *mount, *markerFile, *marker, *sizeMiB)
	case "probe-open":
		probeOpen(ctx, mgr, *scope, *mount, *markerFile, *marker)
	case "grep-raw":
		grepRaw(*root, *scope, *marker)
	case "reopen-read":
		reopenRead(ctx, mgr, *scope, *mount, *keyFile, *markerFile, *marker)
	case "close":
		closeContainer(ctx, mgr, *scope, *mount)
	case "shred":
		shred(ctx, mgr, *scope, *mount)
	case "reopen-fail":
		reopenFail(ctx, mgr, *scope, *mount, *keyFile, *root)
	default:
		fail("unknown mode %q", mode)
	}
}

// genKey generates a fresh 256-bit key and writes it to keyFile. The key VALUE
// is never logged; only its length is printed.
func genKey(keyFile string) {
	if keyFile == "" {
		fail("gen-key requires -key-file")
	}
	k, err := storecrypt.NewKey()
	if err != nil {
		fail("generate key: %v", err)
	}
	if err := os.WriteFile(keyFile, k, 0o600); err != nil {
		fail("write key file: %v", err)
	}
	fmt.Printf("OK gen-key: wrote %d-byte key to %s\n", len(k), keyFile)
}

// loadKey reads the test key from keyFile. The returned bytes are a Key so a
// stray format would redact rather than leak them.
func loadKey(keyFile string) storecrypt.Key {
	if keyFile == "" {
		fail("this mode requires -key-file")
	}
	b, err := os.ReadFile(keyFile) //nolint:gosec // CI-only test key path
	if err != nil {
		fail("read key file: %v", err)
	}
	return storecrypt.Key(b)
}

func create(ctx context.Context, mgr *storecrypt.Manager, scope, mount, keyFile, mountPoint, markerFile, marker string, sizeMiB int64) {
	if scope == "" || mount == "" || marker == "" {
		fail("create requires -scope, -mount, -marker")
	}
	key := loadKey(keyFile)
	defer key.Zeroize()
	size := sizeMiB << 20
	if err := mgr.Create(ctx, scope, key, size, mount); err != nil {
		fail("Manager.Create: %v", err)
	}
	target := filepath.Join(mountPoint, markerFile)
	if err := os.WriteFile(target, []byte(marker+"\n"), 0o600); err != nil {
		fail("write marker into mounted container: %v", err)
	}
	fmt.Printf("OK create: container for scope %s mounted at %s, %d-byte key, wrote marker to %s\n",
		scope, mount, len(key), target)
}

func probeOpen(ctx context.Context, mgr *storecrypt.Manager, scope, mount, markerFile, marker string) {
	_ = ctx
	if marker == "" {
		fail("probe-open requires -marker")
	}
	target := filepath.Join(mount, markerFile)
	b, err := os.ReadFile(target) //nolint:gosec // CI-only marker path inside the mount
	if err != nil {
		fail("read marker from mounted container: %v", err)
	}
	if !strings.Contains(string(b), marker) {
		fail("marker NOT found in the decrypted mount; the control read failed")
	}
	fmt.Printf("OK probe-open: marker present in the decrypted mount (control: the marker string IS findable)\n")
}

// grepRaw scans the raw backing image for the marker. It must be ABSENT: the
// bytes on disk are ciphertext.
func grepRaw(root, scope, marker string) {
	if root == "" || scope == "" || marker == "" {
		fail("grep-raw requires -root, -scope, -marker")
	}
	img := filepath.Join(root, "enc", scope+".img")
	data, err := os.ReadFile(img) //nolint:gosec // CI-only raw image path
	if err != nil {
		fail("read raw image %s: %v", img, err)
	}
	count := strings.Count(string(data), marker)
	if count != 0 {
		fail("CIPHERTEXT-AT-REST VIOLATION: marker found %d time(s) as plaintext in raw image %s", count, img)
	}
	fmt.Printf("OK grep-raw: marker ABSENT in raw image %s (%d bytes scanned); data at rest is ciphertext\n",
		img, len(data))
}

func reopenRead(ctx context.Context, mgr *storecrypt.Manager, scope, mount, keyFile, markerFile, marker string) {
	if scope == "" || mount == "" || marker == "" {
		fail("reopen-read requires -scope, -mount, -marker")
	}
	key := loadKey(keyFile)
	defer key.Zeroize()
	if err := mgr.Open(ctx, scope, key, mount); err != nil {
		fail("Manager.Open (reopen with key): %v", err)
	}
	target := filepath.Join(mount, markerFile)
	b, err := os.ReadFile(target) //nolint:gosec // CI-only marker path inside the mount
	if err != nil {
		fail("read marker after reopen: %v", err)
	}
	if !strings.Contains(string(b), marker) {
		fail("DECRYPT FAILURE: marker not intact after reopen+mount")
	}
	fmt.Printf("OK reopen-read: reopened with key, mounted, marker reads back intact (decrypt/restore works)\n")
}

func closeContainer(ctx context.Context, mgr *storecrypt.Manager, scope, mount string) {
	if scope == "" || mount == "" {
		fail("close requires -scope, -mount")
	}
	if err := mgr.Close(ctx, scope, mount); err != nil {
		fail("Manager.Close: %v", err)
	}
	fmt.Printf("OK close: container for scope %s unmounted and closed\n", scope)
}

func shred(ctx context.Context, mgr *storecrypt.Manager, scope, mount string) {
	if scope == "" || mount == "" {
		fail("shred requires -scope, -mount")
	}
	if err := mgr.Shred(ctx, scope, mount); err != nil {
		fail("Manager.Shred: %v", err)
	}
	fmt.Printf("OK shred: container for scope %s crypto-shredded (luksErase + img removed)\n", scope)
}

// reopenFail proves crypto-shred made the data unrecoverable: Open MUST fail
// after Shred, and the img must be gone.
func reopenFail(ctx context.Context, mgr *storecrypt.Manager, scope, mount, keyFile, root string) {
	if scope == "" || mount == "" || root == "" {
		fail("reopen-fail requires -scope, -mount, -root")
	}
	img := filepath.Join(root, "enc", scope+".img")
	if _, err := os.Stat(img); err == nil {
		fail("CRYPTO-SHRED VIOLATION: image %s still exists after shred", img)
	}
	key := loadKey(keyFile)
	defer key.Zeroize()
	if err := mgr.Open(ctx, scope, key, mount); err == nil {
		fail("CRYPTO-SHRED VIOLATION: container reopened with the original key after shred")
	}
	fmt.Printf("OK reopen-fail: reopen with the original key FAILS and img is gone; data is unrecoverable\n")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "crypt-smoke: "+format+"\n", args...)
	os.Exit(1)
}
