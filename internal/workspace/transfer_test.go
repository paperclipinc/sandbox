package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/paperclipinc/mitos/internal/cas"
)

// fakeAgent fakes the guest agent's bulk tar transport over a host-side
// directory standing in for the guest /workspace. TarDir tars that directory;
// UntarDir extracts into it (sanitizing members), so a Dehydrate -> Hydrate round
// trip through a CAS store can be verified end to end without a VM.
type fakeAgent struct {
	root string
}

func newFakeAgent(t *testing.T) *fakeAgent {
	return &fakeAgent{root: t.TempDir()}
}

func (a *fakeAgent) writeFile(t *testing.T, rel, content string) {
	full := filepath.Join(a.root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (a *fakeAgent) TarDir(path string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.Walk(a.root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(a.root, p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: fi.Size(), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a *fakeAgent) UntarDir(path string, data []byte) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		target, err := safeJoin(a.root, filepath.Clean(hdr.Name))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeFileFrom(target, tr); err != nil {
			return err
		}
	}
	return nil
}

func (a *fakeAgent) listFiles(t *testing.T) map[string]string {
	out := map[string]string{}
	err := filepath.Walk(a.root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(a.root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newStore(t *testing.T) *cas.Store {
	s, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDehydrateHydrateRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	src := newFakeAgent(t)
	want := map[string]string{
		"main.go":           "package main",
		"sub/nested.txt":    "nested content",
		"sub/deep/data.bin": "\x00\x01binary\xff",
		"empty":             "",
	}
	for rel, content := range want {
		src.writeFile(t, rel, content)
	}

	digest, err := Dehydrate(ctx, src, store, nil, nil)
	if err != nil {
		t.Fatalf("Dehydrate: %v", err)
	}
	if err := digest.Validate(); err != nil {
		t.Fatalf("Dehydrate returned invalid digest: %v", err)
	}

	dst := newFakeAgent(t)
	if err := Hydrate(ctx, dst, store, digest); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	got := dst.listFiles(t)
	if len(got) != len(want) {
		t.Fatalf("round trip file count = %d, want %d (got %v)", len(got), len(want), keys(got))
	}
	for rel, content := range want {
		if got[rel] != content {
			t.Errorf("round trip %s = %q, want %q", rel, got[rel], content)
		}
	}
}

func TestDehydrateUnchangedTreeDedups(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	a := newFakeAgent(t)
	a.writeFile(t, "a.txt", "alpha")
	a.writeFile(t, "b/c.txt", "charlie")

	d1, err := Dehydrate(ctx, a, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A second, byte-identical tree dehydrates to the SAME digest (content
	// addressing): the revision content dedups.
	b := newFakeAgent(t)
	b.writeFile(t, "a.txt", "alpha")
	b.writeFile(t, "b/c.txt", "charlie")
	d2, err := Dehydrate(ctx, b, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if d1 != d2 {
		t.Fatalf("unchanged tree produced different digests: %s != %s", d1, d2)
	}

	// A changed tree must produce a different digest.
	c := newFakeAgent(t)
	c.writeFile(t, "a.txt", "alpha")
	c.writeFile(t, "b/c.txt", "DELTA")
	d3, err := Dehydrate(ctx, c, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d3 == d1 {
		t.Fatal("changed tree produced the same digest; content addressing broken")
	}
}

func TestDehydrateExcludesSecretPaths(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	a := newFakeAgent(t)
	a.writeFile(t, "keep.txt", "keep me")
	a.writeFile(t, ".netrc", "machine secret login bot password hunter2")
	a.writeFile(t, ".secrets/token", "super-secret-token")

	excludes := []string{"/workspace/.netrc", ".secrets"}
	digest, err := Dehydrate(ctx, a, store, excludes, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Hydrate into a fresh tree and assert the secrets are gone.
	dst := newFakeAgent(t)
	if err := Hydrate(ctx, dst, store, digest); err != nil {
		t.Fatal(err)
	}
	got := dst.listFiles(t)
	if _, ok := got["keep.txt"]; !ok {
		t.Error("excluded dehydrate dropped a non-secret file")
	}
	if _, ok := got[".netrc"]; ok {
		t.Error(".netrc was captured into the revision; secret exclusion failed")
	}
	if _, ok := got[".secrets/token"]; ok {
		t.Error(".secrets/token was captured into the revision; secret exclusion failed")
	}
}

func TestDehydrateCapturePathsOnlyCapturesSubtree(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	a := newFakeAgent(t)
	a.writeFile(t, "dist/app.js", "built")
	a.writeFile(t, "dist/sub/page.js", "built page")
	a.writeFile(t, "src/main.go", "package main")
	a.writeFile(t, "README.md", "readme")

	// Capture only the dist subtree (the {path: /workspace/dist} output).
	digest, err := Dehydrate(ctx, a, store, nil, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	dst := newFakeAgent(t)
	if err := Hydrate(ctx, dst, store, digest); err != nil {
		t.Fatal(err)
	}
	got := dst.listFiles(t)
	if _, ok := got["dist/app.js"]; !ok {
		t.Error("captured revision missing dist/app.js")
	}
	if _, ok := got["dist/sub/page.js"]; !ok {
		t.Error("captured revision missing dist/sub/page.js")
	}
	if _, ok := got["src/main.go"]; ok {
		t.Error("captured revision included src/main.go outside the dist capture path")
	}
	if _, ok := got["README.md"]; ok {
		t.Error("captured revision included README.md outside the dist capture path")
	}
}

func TestUnpackTarRejectsTraversal(t *testing.T) {
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
	if _, err := unpackTar(buf.Bytes(), dst, nil); err == nil {
		t.Fatal("unpackTar accepted a ../escape member; want rejection")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
