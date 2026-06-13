package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// rendezvousAuthorName and rendezvousAuthorEmail are the deterministic commit
// identity the engine uses for a rendezvous push. The rendezvous commit is
// machine-produced state, not a human author; a fixed identity keeps the commit
// reproducible and clearly attributed to the engine.
const (
	rendezvousAuthorName  = "mitos rendezvous"
	rendezvousAuthorEmail = "rendezvous@mitos.run"
	rendezvousMessage     = "mitos: workspace rendezvous"
)

// RenderBranch renders a per-attempt branch name from a text/template using the
// claim (or sandbox) name as {{.name}}. An empty template falls back to a
// deterministic "attempt/<name>" so a {git} output without an explicit branch
// still lands on a distinct per-attempt branch (git is the merge layer; each
// attempt is its own branch). The result is validated as a git ref name.
func RenderBranch(tmpl, name string) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = "attempt/{{.name}}"
	}
	t, err := template.New("branch").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse branch template %q: %w", tmpl, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]string{"name": name}); err != nil {
		return "", fmt.Errorf("render branch template %q: %w", tmpl, err)
	}
	branch := strings.TrimSpace(buf.String())
	if branch == "" {
		return "", fmt.Errorf("branch template %q rendered empty for name %q", tmpl, name)
	}
	if strings.ContainsAny(branch, " ~^:?*[\\") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return "", fmt.Errorf("rendered branch %q is not a valid git ref name", branch)
	}
	// A leading "-" makes the branch parse as a git flag in any option position,
	// so reject it even though the push uses a "--" separator (defense in depth):
	// a custom branch template starting with "-" must never reach the git CLI.
	if strings.HasPrefix(branch, "-") {
		return "", fmt.Errorf("rendered branch %q must not begin with '-'", branch)
	}
	return branch, nil
}

// Rendezvous materializes repoFiles into a temp worktree, makes a single
// deterministic commit, and pushes it to remote on branch. repoFiles maps
// workspace-relative repo-path names to their content (resolved from the
// workspace spec.git.paths). It uses the git CLI via exec: git is present on the
// runners and images, so this adds no dependency. Empty repoFiles is a no-op
// (a {git} output with no spec.git.paths content is honest about having nothing
// to push). Git is the merge layer: this pushes a branch, it never merges.
//
// A push failure is returned (with the git output for remediation, sans any
// secret since the content is repo paths only), so the caller surfaces it on a
// condition rather than swallowing it.
func Rendezvous(ctx context.Context, repoFiles map[string]string, remote, branch string) error {
	if len(repoFiles) == 0 {
		return nil
	}
	if strings.TrimSpace(remote) == "" {
		return fmt.Errorf("git rendezvous: empty remote")
	}
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("git rendezvous: empty branch")
	}

	work, err := os.MkdirTemp("", "ws-rendezvous-*")
	if err != nil {
		return fmt.Errorf("git rendezvous temp dir: %w", err)
	}
	defer os.RemoveAll(work) //nolint:errcheck // best-effort cleanup

	// Write each repo file under the worktree at its workspace-relative name,
	// rejecting any traversal so a crafted name cannot escape the worktree.
	names := make([]string, 0, len(repoFiles))
	for name := range repoFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dst, err := safeJoin(work, filepath.Clean(filepath.ToSlash(name)))
		if err != nil {
			return fmt.Errorf("git rendezvous file %q: %w", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("git rendezvous mkdir for %q: %w", name, err)
		}
		if err := os.WriteFile(dst, []byte(repoFiles[name]), 0o644); err != nil { //nolint:gosec // repo content, not a secret
			return fmt.Errorf("git rendezvous write %q: %w", name, err)
		}
	}

	// A deterministic, isolated commit: -c overrides avoid depending on any
	// ambient git config, and the fixed author/committer make the commit
	// reproducible. The branch is created with the initial commit so the push
	// target name is exactly the rendered branch.
	steps := [][]string{
		{"init", "-q", "-b", branch},
		{"add", "-A"},
		{
			"-c", "user.name=" + rendezvousAuthorName,
			"-c", "user.email=" + rendezvousAuthorEmail,
			"-c", "commit.gpgsign=false",
			"commit", "-q",
			"--author", fmt.Sprintf("%s <%s>", rendezvousAuthorName, rendezvousAuthorEmail),
			"-m", rendezvousMessage,
		},
		// The "--" separator forces remote and branch to be parsed as positional
		// arguments, never as flags. This closes the confirmed arg-injection RCE
		// where a remote of "--receive-pack=<cmd>" would otherwise be parsed as a
		// flag and run an arbitrary command on the pushing host.
		{"push", "--", remote, branch},
	}
	// An empty HOME and GIT_CONFIG_NOSYSTEM=1 isolate the push from ambient git
	// config (a controller image ~/.gitconfig or /etc/gitconfig), so no on-host
	// config can re-enable the ext::/fd:: transports or otherwise alter the push.
	gitHome, err := os.MkdirTemp("", "ws-rendezvous-home-*")
	if err != nil {
		return fmt.Errorf("git rendezvous home dir: %w", err)
	}
	defer os.RemoveAll(gitHome) //nolint:errcheck // best-effort cleanup
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME="+rendezvousAuthorName,
		"GIT_AUTHOR_EMAIL="+rendezvousAuthorEmail,
		"GIT_COMMITTER_NAME="+rendezvousAuthorName,
		"GIT_COMMITTER_EMAIL="+rendezvousAuthorEmail,
		// Fixed dates keep the commit, and therefore the pushed object, stable.
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
		// Defense in depth: ignore /etc/gitconfig and any user gitconfig.
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+gitHome,
	)
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = work
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
