// Command cas-verify is a small operational and CI helper for the
// content-addressed snapshot store (internal/cas). It is dependency-free
// (only internal/cas and the standard library) and scriptable: every mode
// prints clear stdout and exits nonzero on any failure.
//
// Modes:
//
//	put          PutSnapshot the files in a snapshot dir, print the manifest digest.
//	materialize  Materialize a given manifest digest to an output dir.
//	check        put then materialize then compare sha256 of each reconstructed
//	             file to the original; exit 0 only if every file is byte-identical.
//	tamper-check put, corrupt one byte of one chunk in the store, then assert
//	             Materialize fails (the integrity gate). Exit 0 only if it fails.
//
// A snapshot dir is expected to contain the files named by -files (default
// "mem,vmstate"). The store root is given by -store.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/paperclipinc/sandbox/internal/cas"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode := os.Args[1]
	args := os.Args[2:]

	var err error
	switch mode {
	case "put":
		err = runPut(args)
	case "materialize":
		err = runMaterialize(args)
	case "check":
		err = runCheck(args)
	case "tamper-check":
		err = runTamperCheck(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cas-verify %s: %v\n", mode, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: cas-verify <mode> [flags]

modes:
  put          -dir <snapshot dir> -store <root> [-files mem,vmstate] [-vmm-version V]
  materialize  -digest <manifest> -store <root> -out <dir>
  check        -dir <snapshot dir> -store <root> -out <dir> [-files mem,vmstate] [-vmm-version V]
  tamper-check -dir <snapshot dir> -store <root> -out <dir> [-files mem,vmstate] [-vmm-version V]
`)
}

// flagsFor builds the common flag set and returns the parsed values.
type commonFlags struct {
	dir, store, out, files, vmmVersion, digest string
}

func parseCommon(args []string, wantDir, wantOut, wantDigest bool) (commonFlags, error) {
	fs := flag.NewFlagSet("cas-verify", flag.ContinueOnError)
	var c commonFlags
	if wantDir {
		fs.StringVar(&c.dir, "dir", "", "snapshot directory holding the input files")
	}
	fs.StringVar(&c.store, "store", "", "CAS store root")
	if wantOut {
		fs.StringVar(&c.out, "out", "", "output directory for materialized files")
	}
	if wantDigest {
		fs.StringVar(&c.digest, "digest", "", "manifest digest to materialize")
	}
	fs.StringVar(&c.files, "files", "mem,vmstate", "comma-separated logical file names to look for in -dir")
	fs.StringVar(&c.vmmVersion, "vmm-version", "", "VMM version recorded in the manifest")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.store == "" {
		return c, fmt.Errorf("-store is required")
	}
	if wantDir && c.dir == "" {
		return c, fmt.Errorf("-dir is required")
	}
	if wantOut && c.out == "" {
		return c, fmt.Errorf("-out is required")
	}
	if wantDigest && c.digest == "" {
		return c, fmt.Errorf("-digest is required")
	}
	return c, nil
}

// snapshotFiles maps each requested logical name to its path under dir,
// requiring every named file to exist.
func snapshotFiles(dir, fileList string) (map[string]string, error) {
	files := make(map[string]string)
	for _, name := range strings.Split(fileList, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("input file %q: %w", name, err)
		}
		files[name] = path
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no input files named")
	}
	return files, nil
}

func runPut(args []string) error {
	c, err := parseCommon(args, true, false, false)
	if err != nil {
		return err
	}
	files, err := snapshotFiles(c.dir, c.files)
	if err != nil {
		return err
	}
	store, err := cas.New(c.store)
	if err != nil {
		return err
	}
	m, err := store.PutSnapshot(files, cas.Metadata{VMMVersion: c.vmmVersion})
	if err != nil {
		return err
	}
	fmt.Println(m.Digest())
	return nil
}

func runMaterialize(args []string) error {
	c, err := parseCommon(args, false, true, true)
	if err != nil {
		return err
	}
	store, err := cas.New(c.store)
	if err != nil {
		return err
	}
	if err := store.Materialize(cas.Digest(c.digest), c.out); err != nil {
		return err
	}
	fmt.Printf("materialized %s to %s\n", c.digest, c.out)
	return nil
}

func runCheck(args []string) error {
	c, err := parseCommon(args, true, true, false)
	if err != nil {
		return err
	}
	files, err := snapshotFiles(c.dir, c.files)
	if err != nil {
		return err
	}
	store, err := cas.New(c.store)
	if err != nil {
		return err
	}
	m, err := store.PutSnapshot(files, cas.Metadata{VMMVersion: c.vmmVersion})
	if err != nil {
		return err
	}
	digest := m.Digest()
	fmt.Printf("manifest digest: %s\n", digest)

	if err := store.Materialize(digest, c.out); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}

	for name, orig := range files {
		want, err := fileSHA(orig)
		if err != nil {
			return err
		}
		got, err := fileSHA(filepath.Join(c.out, name))
		if err != nil {
			return err
		}
		if want != got {
			return fmt.Errorf("file %q NOT byte-identical: original %s reconstructed %s", name, want, got)
		}
		fmt.Printf("file %q byte-identical: %s\n", name, got)
	}
	fmt.Println("CHECK PASSED: all files reconstructed byte-identically")
	return nil
}

func runTamperCheck(args []string) error {
	c, err := parseCommon(args, true, true, false)
	if err != nil {
		return err
	}
	files, err := snapshotFiles(c.dir, c.files)
	if err != nil {
		return err
	}
	store, err := cas.New(c.store)
	if err != nil {
		return err
	}
	m, err := store.PutSnapshot(files, cas.Metadata{VMMVersion: c.vmmVersion})
	if err != nil {
		return err
	}
	digest := m.Digest()

	// Pick the first chunk of the first file and flip one byte of its on-disk
	// data. The store layout is documented: <root>/chunks/<digest[:2]>/<digest>.
	if len(m.Files) == 0 || len(m.Files[0].Chunks) == 0 {
		return fmt.Errorf("snapshot has no chunks to tamper")
	}
	victim := string(m.Files[0].Chunks[0].Digest)
	chunkPath := filepath.Join(c.store, "chunks", victim[:2], victim)
	if err := flipOneByte(chunkPath); err != nil {
		return fmt.Errorf("tamper chunk %s: %w", victim, err)
	}
	fmt.Printf("tampered chunk %s\n", victim)

	// Materialize MUST now fail: the corrupted chunk no longer hashes to its
	// digest, so verification rejects it.
	if err := store.Materialize(digest, c.out); err == nil {
		return fmt.Errorf("TAMPER CHECK FAILED: Materialize succeeded on a corrupted chunk")
	} else {
		fmt.Printf("Materialize correctly rejected the tampered chunk: %v\n", err)
	}

	// And the partial/corrupt output must not be left behind.
	if _, err := os.Stat(filepath.Join(c.out, m.Files[0].Name)); !os.IsNotExist(err) {
		return fmt.Errorf("TAMPER CHECK FAILED: destination file left behind after failed Materialize")
	}
	fmt.Println("TAMPER CHECK PASSED: corruption detected and no partial output left")
	return nil
}

// flipOneByte flips the low bit of the first byte of the file in place.
func flipOneByte(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // path derived from a digest
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // best-effort, error surfaced by read/write below

	b := make([]byte, 1)
	if _, err := f.ReadAt(b, 0); err != nil {
		return err
	}
	b[0] ^= 0x01
	if _, err := f.WriteAt(b, 0); err != nil {
		return err
	}
	return f.Close()
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // operational helper over caller-supplied path
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only file
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
