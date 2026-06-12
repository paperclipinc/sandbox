package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// remotePattern mirrors the +kubebuilder:validation:Pattern on
// api/v1alpha1 GitOutput.Remote. The CRD enforces it at admission; this test
// pins the regex so a representative arg-injection / dangerous-transport remote
// is rejected and the legitimate forms are admitted. Keep the two in sync.
const remotePattern = `^(https://|http://|ssh://|git://|file://|[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:).+`

func TestRemotePatternRejectsArgInjectionAndBadTransports(t *testing.T) {
	re := regexp.MustCompile(remotePattern)
	bad := []string{
		"--receive-pack=touch /tmp/pwned",
		"-oProxyCommand=x",
		"ext::sh -c touch /tmp/pwned",
		"fd::3",
		"",
	}
	for _, r := range bad {
		if re.MatchString(r) {
			t.Errorf("remote pattern must reject %q", r)
		}
	}
	good := []string{
		"https://github.com/acme/repo.git",
		"http://internal.example/repo.git",
		"ssh://git@github.com/acme/repo.git",
		"git://example.com/repo.git",
		"file:///srv/git/repo.git",
		"git@github.com:acme/repo.git",
	}
	for _, r := range good {
		if !re.MatchString(r) {
			t.Errorf("remote pattern must admit %q", r)
		}
	}
}

// requireGit skips the test when git is not on PATH, so the unit suite is not
// flaky on a minimal image. CI's linux runner has git.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; skipping git rendezvous test")
	}
}

// gitOut runs git in dir and returns trimmed stdout, failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRenderBranch(t *testing.T) {
	got, err := RenderBranch("attempt/{{.name}}", "agent-7f3a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "attempt/agent-7f3a" {
		t.Fatalf("RenderBranch = %q, want attempt/agent-7f3a", got)
	}
	// An empty template falls back to the claim name on a deterministic prefix.
	got, err = RenderBranch("", "agent-7f3a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "attempt/agent-7f3a" {
		t.Fatalf("RenderBranch empty template = %q, want attempt/agent-7f3a", got)
	}
}

func TestRendezvousPushesToLocalBareRepo(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	// A local bare repo stands in for the rendezvous remote.
	bare := filepath.Join(t.TempDir(), "rendezvous.git")
	gitOut(t, t.TempDir(), "init", "--bare", bare)

	repoFiles := map[string]string{
		"repo/main.go":   "package main\n",
		"repo/README.md": "# attempt\n",
	}
	branch := "attempt/agent-7f3a"
	if err := Rendezvous(ctx, repoFiles, bare, branch); err != nil {
		t.Fatalf("Rendezvous: %v", err)
	}

	// The branch must exist on the remote and carry exactly one commit.
	refs := gitOut(t, bare, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if !strings.Contains(refs, branch) {
		t.Fatalf("remote refs %q missing branch %q", refs, branch)
	}
	count := gitOut(t, bare, "rev-list", "--count", branch)
	if count != "1" {
		t.Fatalf("branch %q has %s commits, want 1", branch, count)
	}

	// The pushed content must match the repo paths (the workspace-relative names
	// are preserved on the remote tree).
	tree := gitOut(t, bare, "ls-tree", "-r", "--name-only", branch)
	for name := range repoFiles {
		if !strings.Contains(tree, name) {
			t.Fatalf("pushed tree %q missing %q", tree, name)
		}
	}
	got := gitOut(t, bare, "show", branch+":repo/main.go")
	if got != "package main" {
		t.Fatalf("pushed repo/main.go = %q, want the source content", got)
	}
}

// TestRenderBranchRejectsLeadingDash proves a custom branch template that
// renders to a value starting with "-" is rejected, so it cannot inject a flag
// into the git push even with the "--" separator (defense in depth).
func TestRenderBranchRejectsLeadingDash(t *testing.T) {
	if _, err := RenderBranch("{{.name}}", "-oProxyCommand=touch /tmp/x"); err == nil {
		t.Fatal("RenderBranch must reject a branch beginning with '-'")
	}
	if _, err := RenderBranch("-q", "ignored"); err == nil {
		t.Fatal("RenderBranch must reject a literal '-q' branch")
	}
}

// TestRendezvousRemoteArgInjectionIsContained proves the confirmed RCE is
// closed: a remote of "--receive-pack=touch <sentinel>" must NOT run the
// command. The "--" separator before the positional args makes git parse the
// flag-shaped value as a remote name, so the push fails to connect and no
// command executes. We assert the sentinel file is absent and an error returns.
func TestRendezvousRemoteArgInjectionIsContained(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "pwned")
	remote := "--receive-pack=touch " + sentinel

	repoFiles := map[string]string{"repo/main.go": "package main\n"}
	err := Rendezvous(context.Background(), repoFiles, remote, "attempt/agent-7f3a")
	if err == nil {
		t.Fatal("Rendezvous with a flag-shaped remote must return an error, not succeed")
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatalf("RCE: sentinel %q was created, the --receive-pack remote ran a command", sentinel)
	}
}

func TestRendezvousNoFilesIsNoOp(t *testing.T) {
	requireGit(t)
	// No repo files: a {git} output with nothing to push is a no-op, not an error.
	if err := Rendezvous(context.Background(), nil, "unused-remote", "attempt/x"); err != nil {
		t.Fatalf("empty Rendezvous should be a no-op, got %v", err)
	}
}
