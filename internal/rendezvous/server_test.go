package rendezvous

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/workspace"
)

// requireGit skips when git is absent so the unit suite is not flaky on a
// minimal image, mirroring internal/workspace/git_test.go discipline.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; skipping rendezvous server test")
	}
	if _, err := exec.LookPath("git-http-backend"); err != nil {
		// git-http-backend ships with git but may live in a libexec dir not on
		// PATH; the server discovers it via `git --exec-path`. Only skip when git
		// itself is missing.
		_ = err
	}
}

func gitRun(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// newTestRepoTree creates a tiny git working tree with one commit on a branch,
// returning its dir. It is the source a credentialed push originates from.
func newTestRepoTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	env := []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME=" + dir,
	}
	if out, err := gitRun(t, dir, env, "init", "-q", "-b", "attempt/x"); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if out, err := gitRun(t, dir, env, "add", "-A"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := gitRun(t, dir, env, "commit", "-q", "-m", "x"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return dir
}

func TestServerRejectsUnauthenticatedPushWith401(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	srv, err := New(Config{Root: root, Username: "bot", Token: "secret-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// An info/refs request without credentials must be 401.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		ts.URL+"/repo.git/info/refs?service=git-receive-pack", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request got %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 response must carry a WWW-Authenticate challenge")
	}
}

func TestServerAcceptsAuthenticatedPush(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	const token = "secret-token-123"
	srv, err := New(Config{Root: root, Username: "bot", Token: token})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	src := newTestRepoTree(t)
	// Push over HTTP basic-auth embedded in the URL; the server creates the repo
	// on first push.
	remote := strings.Replace(ts.URL, "http://", "http://bot:"+token+"@", 1) + "/proj.git"
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1", "HOME=" + src,
		// Allow pushing into a repo that does not yet exist server-side.
		"GIT_HTTP_LOW_SPEED_LIMIT=0",
	}
	if out, err := gitRun(t, src, env, "push", "--", remote, "attempt/x"); err != nil {
		t.Fatalf("authenticated push failed: %v\n%s", err, out)
	}

	// The branch must now exist in the server-side bare repo.
	serverRepo := filepath.Join(root, "proj.git")
	out, err := gitRun(t, serverRepo, nil, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		t.Fatalf("inspect server repo: %v\n%s", err, out)
	}
	if !strings.Contains(out, "attempt/x") {
		t.Fatalf("server repo missing pushed branch; refs=%q", out)
	}
}

// TestRendezvousCredentialedPushLandsOnServer closes the loop end to end: the
// workspace.Rendezvous credentialed push (the engine side) authenticates to this
// rendezvous server (the remote side) and lands the per-attempt branch, with the
// token delivered only through the ephemeral credentials file, never on argv.
func TestRendezvousCredentialedPushLandsOnServer(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	const token = "loop-token-ABCDEF"
	srv, err := New(Config{Root: root, Username: "bot", Token: token})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	remote := ts.URL + "/loop.git"
	creds := &workspace.Credentials{Username: "bot", Token: token}
	repoFiles := map[string]string{"repo/main.go": "package main\n"}
	branch := "attempt/agent-loop"

	if err := workspace.Rendezvous(context.Background(), repoFiles, remote, branch, creds); err != nil {
		t.Fatalf("credentialed Rendezvous to the server: %v", err)
	}

	serverRepo := filepath.Join(root, "loop.git")
	out, err := gitRun(t, serverRepo, nil, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		t.Fatalf("inspect server repo: %v\n%s", err, out)
	}
	if !strings.Contains(out, branch) {
		t.Fatalf("server repo missing pushed branch; refs=%q", out)
	}
}

func TestServerRejectsWrongToken(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	srv, err := New(Config{Root: root, Username: "bot", Token: "right-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		ts.URL+"/repo.git/info/refs?service=git-receive-pack", nil)
	req.SetBasicAuth("bot", "wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token got %d, want 401", resp.StatusCode)
	}
}
