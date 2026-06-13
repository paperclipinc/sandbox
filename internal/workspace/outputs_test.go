package workspace

import (
	"reflect"
	"sort"
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
)

func TestCapturePaths(t *testing.T) {
	// No Path outputs: whole workspace (nil capture set).
	if got := CapturePaths(nil); got != nil {
		t.Fatalf("no outputs should capture the whole workspace (nil), got %v", got)
	}
	if got := CapturePaths([]v1alpha1.OutputSpec{{Diff: true}, {Git: &v1alpha1.GitOutput{}}}); got != nil {
		t.Fatalf("no Path outputs should capture the whole workspace (nil), got %v", got)
	}

	// Path outputs are normalized to workspace-relative slash prefixes.
	got := CapturePaths([]v1alpha1.OutputSpec{
		{Path: "/workspace/dist"},
		{Path: "/workspace/dist"}, // duplicate de-duped
		{Path: "src/gen/"},
		{Diff: true},
	})
	sort.Strings(got)
	want := []string{"dist", "src/gen"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CapturePaths = %v, want %v", got, want)
	}
}

func TestFilterFilesCapturesOnlyListedSubtrees(t *testing.T) {
	files := map[string]string{
		"dist/app.js":      "/tmp/a",
		"dist/sub/page.js": "/tmp/b",
		"src/main.go":      "/tmp/c",
		"README.md":        "/tmp/d",
		"distractor/x.txt": "/tmp/e", // must NOT match the "dist" prefix
	}
	out := FilterFiles(files, []string{"dist"})
	want := map[string]string{
		"dist/app.js":      "/tmp/a",
		"dist/sub/page.js": "/tmp/b",
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("FilterFiles = %v, want %v", out, want)
	}

	// A nil capture set is the whole-workspace default (unchanged map).
	all := FilterFiles(files, nil)
	if !reflect.DeepEqual(all, files) {
		t.Fatalf("nil capture must return the whole set, got %v", all)
	}
}

func TestDiffManifestsDetectsAddRemoveModify(t *testing.T) {
	c := func(s string) cas.ChunkRef { return cas.ChunkRef{Digest: cas.Digest(hexDigest(s)), Size: len(s)} }
	parent := cas.Manifest{Files: []cas.FileEntry{
		{Name: "keep.txt", Chunks: []cas.ChunkRef{c("aaaa")}},
		{Name: "mod.txt", Chunks: []cas.ChunkRef{c("bbbb")}},
		{Name: "gone.txt", Chunks: []cas.ChunkRef{c("cccc")}},
	}}
	child := cas.Manifest{Files: []cas.FileEntry{
		{Name: "keep.txt", Chunks: []cas.ChunkRef{c("aaaa")}},
		{Name: "mod.txt", Chunks: []cas.ChunkRef{c("dddd")}}, // different chunk digest
		{Name: "new.txt", Chunks: []cas.ChunkRef{c("eeee")}},
	}}

	d := DiffManifests(parent, child)
	assertPaths(t, "added", d.Added, []string{"new.txt"})
	assertPaths(t, "removed", d.Removed, []string{"gone.txt"})
	assertPaths(t, "modified", d.Modified, []string{"mod.txt"})
	if d.Empty() {
		t.Fatal("a changed tree must not diff to empty")
	}
}

func TestDiffManifestsUnchangedIsEmpty(t *testing.T) {
	c := func(s string) cas.ChunkRef { return cas.ChunkRef{Digest: cas.Digest(hexDigest(s)), Size: len(s)} }
	m := cas.Manifest{Files: []cas.FileEntry{
		{Name: "a.txt", Chunks: []cas.ChunkRef{c("1111")}},
		{Name: "b.txt", Chunks: []cas.ChunkRef{c("2222"), c("3333")}},
	}}
	d := DiffManifests(m, m)
	if !d.Empty() {
		t.Fatalf("an unchanged tree must diff to empty, got %+v", d)
	}
}

func assertPaths(t *testing.T, label string, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s = %v, want %v", label, g, w)
	}
}

// hexDigest returns a valid 64-hex digest seeded from s, for building synthetic
// ChunkRefs in the diff tests without touching a store.
func hexDigest(s string) string {
	const hexAlpha = "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = hexAlpha[(int(s[i%len(s)])+i)%16]
	}
	return string(out)
}
