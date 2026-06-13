package workspace

import (
	"path/filepath"
	"sort"
	"strings"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
)

// CapturePaths reduces a claim's spec.outputs into the set of workspace-relative
// subtree prefixes the dehydrate should capture. A Path output narrows the
// capture to that subtree; the prefixes are normalized to workspace-relative
// slash form (the same shape as the captured tar member names), so a
// "/workspace/dist" output becomes "dist".
//
// With no Path output (only Diff or Git outputs, or none at all) CapturePaths
// returns nil, which FilterFiles treats as the whole-workspace default (the
// slice-2 behavior). Returning nil rather than an empty slice keeps the
// "capture everything" and "capture nothing listed" cases unambiguous.
func CapturePaths(outputs []v1alpha1.OutputSpec) []string {
	seen := map[string]struct{}{}
	var paths []string
	for _, o := range outputs {
		p := normalizeCapturePath(o.Path)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	return paths
}

// normalizeCapturePath cleans a /workspace output path into a workspace-relative
// slash prefix, mirroring normalizeExcludes. An empty or root-only path yields
// "" (the whole-workspace marker).
func normalizeCapturePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.ToSlash(filepath.Clean(p))
	p = strings.TrimPrefix(p, WorkspacePath+"/")
	p = strings.TrimPrefix(p, "/")
	if p == "" || p == "." {
		return ""
	}
	return p
}

// FilterFiles keeps only the members of the captured name -> hostpath file set
// that fall under one of the capture prefixes. A nil (or empty) capture set is
// the whole-workspace default and returns files unchanged. A prefix matches a
// file whose name equals it (a single file output) or sits under it as a
// directory; "dist" matches "dist/app.js" but never "distractor/x.txt".
func FilterFiles(files map[string]string, capturePaths []string) map[string]string {
	if len(capturePaths) == 0 {
		return files
	}
	out := map[string]string{}
	for name, path := range files {
		if underAnyPrefix(name, capturePaths) {
			out[name] = path
		}
	}
	return out
}

func underAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if name == p || strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

// Diff is the changed-path summary of one revision against its parent: the
// added, removed, and modified workspace-relative file names. Modified means
// the file exists in both manifests but its chunk-digest sequence differs (its
// content changed). The lists are sorted for a deterministic record.
type Diff struct {
	Added    []string `json:"added,omitempty"`
	Removed  []string `json:"removed,omitempty"`
	Modified []string `json:"modified,omitempty"`
}

// Empty reports whether the diff records no change (an unchanged tree).
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Modified) == 0
}

// DiffManifests computes the changed-path diff of child against parent by
// comparing each file's chunk-digest sequence (its content hash). A file only in
// child is Added; a file only in parent is Removed; a file in both whose chunk
// digests differ is Modified. An identical file set with identical chunks
// produces an empty diff. This is a content diff, not a rename-aware diff: a
// rename shows as a delete plus an add on the workspace side (git handles
// renames on the repo-paths side).
func DiffManifests(parent, child cas.Manifest) Diff {
	pf := chunkSig(parent)
	cf := chunkSig(child)

	var d Diff
	for name, csig := range cf {
		psig, ok := pf[name]
		switch {
		case !ok:
			d.Added = append(d.Added, name)
		case psig != csig:
			d.Modified = append(d.Modified, name)
		}
	}
	for name := range pf {
		if _, ok := cf[name]; !ok {
			d.Removed = append(d.Removed, name)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Modified)
	return d
}

// chunkSig maps each file name to a string signature of its ordered chunk
// digests, so two files compare equal exactly when their content hashes match.
func chunkSig(m cas.Manifest) map[string]string {
	out := make(map[string]string, len(m.Files))
	for _, fe := range m.Files {
		var b strings.Builder
		for i, c := range fe.Chunks {
			if i > 0 {
				b.WriteByte('|')
			}
			b.WriteString(string(c.Digest))
		}
		out[fe.Name] = b.String()
	}
	return out
}
