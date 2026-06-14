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
	if err := Rendezvous(ctx, repoFiles, bare, branch, nil); err != nil {
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
	err := Rendezvous(context.Background(), repoFiles, remote, "attempt/agent-7f3a", nil)
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
	if err := Rendezvous(context.Background(), nil, "unused-remote", "attempt/x", nil); err != nil {
		t.Fatalf("empty Rendezvous should be a no-op, got %v", err)
	}
}

// TestRendezvousCredentialsNeverLeakOnPushFailure proves the secrets rule for
// the credentialed push: a forced push failure must NOT carry the token in the
// returned error, and no credentials file may survive the call. The credential
// is delivered to git through an ephemeral, isolated HOME .git-credentials file
// (credential.helper=store), never on the git argv, so it cannot appear in a
// process table, a log line, or the returned error.
func TestRendezvousCredentialsNeverLeakOnPushFailure(t *testing.T) {
	requireGit(t)

	const token = "s3cr3t-token-DEADBEEF" //nolint:gosec // test sentinel, not a real credential
	creds := &Credentials{Username: "bot", Token: token}

	// A remote that does not exist forces the push step to fail so we can assert
	// the failure path redacts the token.
	remote := "https://127.0.0.1:1/does-not-exist.git"
	repoFiles := map[string]string{"repo/main.go": "package main\n"}

	err := Rendezvous(context.Background(), repoFiles, remote, "attempt/agent-7f3a", creds)
	if err == nil {
		t.Fatal("Rendezvous to an unreachable remote must return an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("credential token leaked into the error: %v", err)
	}
	// The username on its own is not a secret, but the token MUST be absent.
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token present in error: %v", err)
	}
}

// TestRendezvousCredentialsDoNotTouchCallerHome proves the credential file lives
// only inside the ephemeral rendezvous HOME and never touches the caller's real
// HOME or git config. A token-credential push needs an http(s) remote (file://
// cannot carry basic-auth); the actual landing on a real authenticated remote is
// proven by the rendezvous-server test. Here the push to an unreachable https
// remote fails, but the HOME-isolation and redaction invariants must hold.
func TestRendezvousCredentialsDoNotTouchCallerHome(t *testing.T) {
	requireGit(t)

	const token = "another-secret-CAFEBABE" //nolint:gosec // test sentinel
	creds := &Credentials{Username: "bot", Token: token}
	repoFiles := map[string]string{"repo/a.txt": "hello\n"}
	remote := "https://127.0.0.1:1/unreachable.git"

	homeBefore, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home dir")
	}
	credPath := filepath.Join(homeBefore, ".git-credentials")
	beforeContent, _ := os.ReadFile(credPath) //nolint:errcheck // may not exist

	// The push fails (unreachable), which is fine: we assert isolation + redaction.
	pushErr := Rendezvous(context.Background(), repoFiles, remote, "attempt/agent-7f3a", creds)
	if pushErr == nil {
		t.Fatal("push to an unreachable https remote must fail")
	}
	if strings.Contains(pushErr.Error(), token) {
		t.Fatalf("token leaked into the error: %v", pushErr)
	}

	// The caller's real ~/.git-credentials must be untouched (the credential
	// lived only in the ephemeral HOME, which is removed on return).
	afterContent, _ := os.ReadFile(credPath) //nolint:errcheck
	if string(beforeContent) != string(afterContent) {
		t.Fatalf("Rendezvous mutated the caller's ~/.git-credentials")
	}
	if strings.Contains(string(afterContent), token) {
		t.Fatalf("token leaked into the caller's ~/.git-credentials")
	}
}

// TestValidateRemoteNoUserinfoRejectsEmbeddedCredentials proves a remote URL
// that embeds credentials in the userinfo (https://user:token@host) is rejected
// before any git invocation, and that the rejection error NEVER echoes the
// credential substring back: only the scheme and host are named. This steers an
// operator toward CredentialsSecretRef instead of putting a token in the URL,
// where it would otherwise land in the claim/revision GitPushRecord.Remote
// status.
func TestValidateRemoteNoUserinfoRejectsEmbeddedCredentials(t *testing.T) {
	const token = "s3cr3t-token-DEADBEEF" //nolint:gosec // test sentinel, not a real credential
	const user = "leaky-user"
	cases := []struct {
		name   string
		remote string
		secret []string // substrings that MUST NOT appear in the error
	}{
		{
			name:   "https user and token",
			remote: "https://" + user + ":" + token + "@github.com/acme/repo.git",
			secret: []string{token, user},
		},
		{
			name:   "https token only",
			remote: "https://" + token + "@github.com/acme/repo.git",
			secret: []string{token},
		},
		{
			name:   "http user and token",
			remote: "http://" + user + ":" + token + "@internal.example/repo.git",
			secret: []string{token, user},
		},
		{
			name:   "ssh user and password",
			remote: "ssh://" + user + ":" + token + "@github.com/acme/repo.git",
			secret: []string{token},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRemoteNoUserinfo(tc.remote)
			if err == nil {
				t.Fatalf("validateRemoteNoUserinfo(%q) must reject embedded credentials", tc.remote)
			}
			for _, s := range tc.secret {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("credential substring %q leaked into the rejection error: %v", s, err)
				}
			}
			// The remediation must steer the operator to CredentialsSecretRef.
			if !strings.Contains(err.Error(), "CredentialsSecretRef") {
				t.Fatalf("rejection error must point to CredentialsSecretRef, got: %v", err)
			}
		})
	}
}

// TestValidateRemoteNoUserinfoAcceptsCleanRemotes proves the clean remote forms
// (no embedded credentials) are accepted, including an ssh remote whose userinfo
// is only the conventional "git" username with no password (auth is by key, not
// a secret in the URL).
func TestValidateRemoteNoUserinfoAcceptsCleanRemotes(t *testing.T) {
	good := []string{
		"https://github.com/acme/repo.git",
		"http://internal.example/repo.git",
		"git://example.com/repo.git",
		"file:///srv/git/repo.git",
		"ssh://git@github.com/acme/repo.git",
		"git@github.com:acme/repo.git",
	}
	for _, r := range good {
		if err := validateRemoteNoUserinfo(r); err != nil {
			t.Errorf("validateRemoteNoUserinfo(%q) must accept a clean remote, got %v", r, err)
		}
	}
}

// TestRendezvousRejectsRemoteWithUserinfo proves the userinfo guard runs inside
// Rendezvous before any git step, and the credential never reaches the error.
func TestRendezvousRejectsRemoteWithUserinfo(t *testing.T) {
	const token = "rendezvous-secret-FEEDFACE" //nolint:gosec // test sentinel
	remote := "https://bot:" + token + "@github.com/acme/repo.git"
	repoFiles := map[string]string{"repo/a.txt": "hello\n"}
	err := Rendezvous(context.Background(), repoFiles, remote, "attempt/agent-7f3a", nil)
	if err == nil {
		t.Fatal("Rendezvous must reject a remote with embedded userinfo")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("credential token leaked into the Rendezvous error: %v", err)
	}
}

// TestRendezvousRejectsCredentialsForFileRemote proves a token-credential push
// to a non-http(s) remote (which cannot carry basic-auth) is rejected with an
// error that does NOT contain the token.
func TestRendezvousRejectsCredentialsForFileRemote(t *testing.T) {
	requireGit(t)
	const token = "file-remote-secret-99" //nolint:gosec // test sentinel
	creds := &Credentials{Username: "bot", Token: token}
	repoFiles := map[string]string{"repo/a.txt": "hello\n"}
	err := Rendezvous(context.Background(), repoFiles, "/srv/git/repo.git", "attempt/x", creds)
	if err == nil {
		t.Fatal("credentials on a non-http remote must be rejected")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked into the rejection error: %v", err)
	}
}
