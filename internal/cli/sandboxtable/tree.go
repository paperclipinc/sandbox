package sandboxtable

import (
	"sort"
	"strings"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
)

// TreeNode is one entry in the rendered fork/lineage DAG: a SandboxClaim or a
// SandboxFork, with the children that name it as their source. Name is the
// object name, Phase is its lifecycle phase (empty renders as a dash), Node is
// the forkd node it landed on (empty renders as a dash), and Kind is "claim" or
// "fork" so the renderer can label the lineage roots.
type TreeNode struct {
	Name     string
	Kind     string
	Phase    string
	Node     string
	Children []*TreeNode
}

// BuildLineage assembles the parent->child lineage DAG from the cluster's
// SandboxClaims and SandboxForks. A claim is a lineage root; a fork is a child
// of whatever object its Spec.SourceRef names (a claim OR another fork, so a
// multi-level fork chain nests). Forks that name the same source are siblings.
// A fork whose source is not among the supplied objects is treated as its own
// root so it is never silently dropped. Output roots and children are sorted by
// name for a deterministic tree.
func BuildLineage(claims []v1alpha1.SandboxClaim, forks []v1alpha1.SandboxFork) []*TreeNode {
	nodes := make(map[string]*TreeNode)
	var roots []*TreeNode

	for i := range claims {
		c := &claims[i]
		nodes[c.Name] = &TreeNode{
			Name:  c.Name,
			Kind:  "claim",
			Phase: string(c.Status.Phase),
			Node:  c.Status.Node,
		}
	}
	for i := range forks {
		f := &forks[i]
		nodes[f.Name] = &TreeNode{
			Name: f.Name,
			Kind: "fork",
			// A SandboxFork carries no single Phase/Node of its own (it fans out
			// to N forks); the per-fork detail lives in ps. The lineage view
			// shows the readiness rollup as the phase so the tree stays honest
			// about a fork that has not produced its replicas yet.
			Phase: forkPhase(f),
		}
	}

	// Link every fork under its source. Claims are always roots.
	for i := range claims {
		roots = append(roots, nodes[claims[i].Name])
	}
	for i := range forks {
		f := &forks[i]
		child := nodes[f.Name]
		parent, ok := nodes[f.Spec.SourceRef.Name]
		if ok && f.Spec.SourceRef.Name != "" {
			parent.Children = append(parent.Children, child)
			continue
		}
		// Orphan: source not present in this view. Surface it as a root rather
		// than dropping it, so the operator still sees the object.
		roots = append(roots, child)
	}

	sortTree(roots)
	return roots
}

// forkPhase renders a SandboxFork's readiness as a phase-like string: "Ready"
// when every replica is ready, otherwise "<ready>/<total>". An unset total
// falls back to the spec replica count.
func forkPhase(f *v1alpha1.SandboxFork) string {
	total := f.Status.TotalForks
	if total == 0 {
		total = f.Spec.Replicas
	}
	if total > 0 && f.Status.ReadyForks == total {
		return string(v1alpha1.SandboxReady)
	}
	return ""
}

// sortTree sorts a node slice and every node's children by name, recursively,
// so the rendered tree is deterministic.
func sortTree(nodes []*TreeNode) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	for _, n := range nodes {
		sortTree(n.Children)
	}
}

// FormatLineage renders the lineage roots as an indented ASCII tree. Each line
// is "<branch><name>  <kind>  <phase>  <node>", with branch glyphs in the style
// of `kubectl tree` / `tree(1)`. Missing phase or node render as a dash. An
// empty set returns a "No sandboxes found" message so an empty cluster reads the
// same as ls.
func FormatLineage(roots []*TreeNode) string {
	if len(roots) == 0 {
		return "No sandboxes found.\n"
	}
	var b strings.Builder
	for i, r := range roots {
		writeTreeNode(&b, r, "", i == len(roots)-1, true)
	}
	return b.String()
}

// writeTreeNode renders one node and recurses into its children. prefix is the
// accumulated indentation for descendants, last reports whether this node is the
// final sibling (so the branch glyph is the corner), and root suppresses the
// glyph for top-level lineage roots.
func writeTreeNode(b *strings.Builder, n *TreeNode, prefix string, last, root bool) {
	branch := ""
	childPrefix := prefix
	if !root {
		if last {
			branch = prefix + "`-- "
			childPrefix = prefix + "    "
		} else {
			branch = prefix + "|-- "
			childPrefix = prefix + "|   "
		}
	}
	b.WriteString(branch)
	b.WriteString(n.Name)
	b.WriteString("  ")
	b.WriteString(n.Kind)
	b.WriteString("  ")
	b.WriteString(orDash(n.Phase))
	b.WriteString("  ")
	b.WriteString(orDash(n.Node))
	b.WriteByte('\n')
	for i, c := range n.Children {
		writeTreeNode(b, c, childPrefix, i == len(n.Children)-1, false)
	}
}
