package sandboxtable

import (
	"strings"
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mkFork(name, source string, replicas, ready, total int32) v1alpha1.SandboxFork {
	return v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SandboxForkSpec{
			SourceRef: v1alpha1.LocalObjectReference{Name: source},
			Replicas:  replicas,
		},
		Status: v1alpha1.SandboxForkStatus{ReadyForks: ready, TotalForks: total},
	}
}

func TestBuildLineageMultiLevelChainAndSiblings(t *testing.T) {
	now := metav1.Now().Time
	claims := []v1alpha1.SandboxClaim{
		mkClaim("root", "default", "web", v1alpha1.SandboxReady, "node-1", "ep", 0, now),
	}
	// root -> fork-a (two siblings fork-a, fork-b off root); fork-a -> fork-a1
	// (a second level), proving a fork can itself be a source.
	forks := []v1alpha1.SandboxFork{
		mkFork("fork-b", "root", 1, 1, 1),
		mkFork("fork-a", "root", 2, 2, 2),
		mkFork("fork-a1", "fork-a", 1, 0, 1),
	}
	roots := BuildLineage(claims, forks)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	r := roots[0]
	if r.Name != "root" || r.Kind != "claim" {
		t.Fatalf("root = %q/%q, want root/claim", r.Name, r.Kind)
	}
	if len(r.Children) != 2 {
		t.Fatalf("root should have 2 children, got %d", len(r.Children))
	}
	// Sorted by name: fork-a before fork-b.
	if r.Children[0].Name != "fork-a" || r.Children[1].Name != "fork-b" {
		t.Fatalf("children = %q,%q, want fork-a,fork-b", r.Children[0].Name, r.Children[1].Name)
	}
	if len(r.Children[0].Children) != 1 || r.Children[0].Children[0].Name != "fork-a1" {
		t.Fatalf("fork-a should have child fork-a1, got %+v", r.Children[0].Children)
	}
}

func TestFormatLineageRendersIndentedTree(t *testing.T) {
	now := metav1.Now().Time
	claims := []v1alpha1.SandboxClaim{
		mkClaim("root", "default", "web", v1alpha1.SandboxReady, "node-1", "ep", 0, now),
	}
	forks := []v1alpha1.SandboxFork{
		mkFork("fork-a", "root", 2, 2, 2),
		mkFork("fork-b", "root", 1, 0, 1),
		mkFork("fork-a1", "fork-a", 1, 1, 1),
	}
	out := FormatLineage(BuildLineage(claims, forks))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), out)
	}
	// Root has no glyph; children are indented under it.
	if !strings.HasPrefix(lines[0], "root") {
		t.Errorf("line 0 should start with root: %q", lines[0])
	}
	if !strings.Contains(lines[0], "claim") || !strings.Contains(lines[0], "node-1") {
		t.Errorf("root line should carry kind+node: %q", lines[0])
	}
	// fork-a is a non-last child, so a tee glyph; fork-a1 nests one level deeper.
	if !strings.Contains(lines[1], "fork-a") || !strings.Contains(lines[1], "|--") {
		t.Errorf("fork-a line should be a tee branch: %q", lines[1])
	}
	if !strings.Contains(lines[2], "fork-a1") {
		t.Errorf("line 2 should be fork-a1: %q", lines[2])
	}
	// fork-a1 indentation is deeper than fork-a.
	if indent(lines[2]) <= indent(lines[1]) {
		t.Errorf("fork-a1 should indent deeper than fork-a: %q vs %q", lines[2], lines[1])
	}
	// fork-b is the last child of root, so a corner glyph.
	if !strings.Contains(lines[3], "fork-b") || !strings.Contains(lines[3], "`--") {
		t.Errorf("fork-b line should be a corner branch: %q", lines[3])
	}
}

func TestFormatLineageMissingPhaseAndNodeAreDashes(t *testing.T) {
	forks := []v1alpha1.SandboxFork{mkFork("lonely", "absent-source", 1, 0, 1)}
	out := FormatLineage(BuildLineage(nil, forks))
	// Orphan fork with no ready replicas and no node: phase + node are dashes.
	if !strings.Contains(out, "lonely") {
		t.Fatalf("orphan fork should appear: %q", out)
	}
	fields := strings.Fields(out)
	// fields: lonely fork - -
	if len(fields) < 4 || fields[2] != "-" || fields[3] != "-" {
		t.Errorf("missing phase/node should be dashes, got fields %v", fields)
	}
}

func TestFormatLineageEmpty(t *testing.T) {
	out := FormatLineage(BuildLineage(nil, nil))
	if !strings.Contains(out, "No sandboxes found") {
		t.Errorf("empty lineage should report no sandboxes, got %q", out)
	}
}

func indent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " |`-"))
}
