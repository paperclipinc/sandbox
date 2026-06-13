// Command ws-smoke drives the bulk workspace hydrate/dehydrate data path against
// real guest VMs over vsock, for the KVM integration phase. It connects to two
// already-booted guest agents (each on its own Firecracker vsock UDS), writes a
// set of known files into the SOURCE guest's /workspace, Dehydrates that
// workspace into a content-addressed manifest in a node CAS store, Hydrates the
// manifest into the DESTINATION guest's /workspace, and asserts every file came
// back byte-identical. A mismatch, a transfer error, or a missing file is a real
// FAILURE (exit 1); a usage error is a SETUP error (exit 2), so the workflow can
// tell a broken harness from a broken transfer.
//
// Usage:
//
//	ws-smoke --cas <dir> --src <src-vsock-uds> --dst <dst-vsock-uds>
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/vsock"
	"github.com/paperclipinc/mitos/internal/workspace"
)

func main() {
	casDir := flag.String("cas", "", "directory for the node CAS store")
	srcUDS := flag.String("src", "", "source guest vsock UDS path")
	dstUDS := flag.String("dst", "", "destination guest vsock UDS path")
	flag.Parse()

	if *casDir == "" || *srcUDS == "" || *dstUDS == "" {
		fmt.Fprintln(os.Stderr, "SETUP: ws-smoke requires --cas, --src, and --dst")
		os.Exit(2)
	}

	store, err := cas.New(*casDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SETUP: open CAS store: %v\n", err)
		os.Exit(2)
	}

	src := connect(*srcUDS, "source")
	defer src.Close() //nolint:errcheck // best-effort close
	dst := connect(*dstUDS, "destination")
	defer dst.Close() //nolint:errcheck // best-effort close

	// Known workspace content, including a nested path and binary bytes, so the
	// proof covers directory structure and non-text content.
	want := map[string][]byte{
		"/workspace/main.go":           []byte("package main\n\nfunc main() {}\n"),
		"/workspace/sub/nested.txt":    []byte("nested workspace content\n"),
		"/workspace/sub/deep/data.bin": {0x00, 0x01, 0x02, 0xff, 0xfe},
		"/workspace/notes.md":          []byte("# notes\nhydrate/dehydrate round trip\n"),
	}
	// A file that should NOT survive a revision: it sits at a secret path the
	// dehydrate exclude list strips. The destination must never see it.
	secretPath := "/workspace/.netrc"
	if err := src.WriteFile(secretPath, []byte("machine secret password hunter2\n"), 0o600); err != nil {
		fail("write secret file into source workspace: %v", err)
	}
	for path, content := range want {
		if err := src.WriteFile(path, content, 0o644); err != nil {
			fail("write %s into source workspace: %v", path, err)
		}
	}
	fmt.Printf("WS_SMOKE wrote %d files into source /workspace\n", len(want))

	ctx := context.Background()
	excludes := []string{"/workspace/.netrc"}
	digest, err := workspace.Dehydrate(ctx, src, store, excludes, nil)
	if err != nil {
		fail("dehydrate source workspace: %v", err)
	}
	fmt.Printf("WS_SMOKE dehydrated to digest %s\n", digest)

	if err := workspace.Hydrate(ctx, dst, store, digest); err != nil {
		fail("hydrate digest into destination workspace: %v", err)
	}
	fmt.Println("WS_SMOKE hydrated digest into destination /workspace")

	// Assert every known file came back byte-identical on the destination.
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		got, err := dst.ReadFile(path)
		if err != nil {
			fail("read %s from destination workspace: %v", path, err)
		}
		if !bytes.Equal(got, want[path]) {
			fail("MISMATCH %s: destination has %d bytes, want %d", path, len(got), len(want[path]))
		}
		fmt.Printf("WS_SMOKE OK %s (%d bytes identical)\n", path, len(got))
	}

	// The excluded secret must NOT have crossed into the revision.
	if _, err := dst.ReadFile(secretPath); err == nil {
		fail("SECRET LEAK: %s reached the destination workspace; dehydrate exclude failed", secretPath)
	}
	fmt.Printf("WS_SMOKE OK secret %s excluded from the revision\n", secretPath)

	fmt.Println("WS_SMOKE PASS: workspace round trip byte-identical, secret excluded")
}

// connect dials a guest agent over the Firecracker vsock UDS on the agent port.
// A dial failure is a SETUP error (the VM did not boot or the agent is not
// listening), distinct from a transfer FAILURE.
func connect(udsPath, role string) *vsock.Client {
	client, err := vsock.Connect(udsPath, vsock.AgentPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SETUP: connect to %s guest agent at %s: %v\n", role, udsPath, err)
		os.Exit(2)
	}
	return client
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAILURE: "+format+"\n", args...)
	os.Exit(1)
}
