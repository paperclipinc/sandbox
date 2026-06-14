package workspace

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
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

// validateRemoteNoUserinfo rejects a rendezvous remote URL that embeds
// credentials in its userinfo component (for example https://user:token@host).
// Such a URL would persist the token into the claim/revision GitPushRecord.Remote
// status, defeating the secrets rule. Authentication must instead flow through
// CredentialsSecretRef, which delivers the token to git only via an ephemeral,
// isolated credentials file (never in status, never on the argv).
//
// The check is scoped to credential-bearing userinfo: an http(s) remote with ANY
// userinfo is rejected (the userinfo there is a basic-auth credential), and any
// scheme whose userinfo carries a password is rejected. A bare username with no
// password on an ssh remote (the conventional "git@host" form, where auth is by
// key) is left alone, as is an scp-like remote that url.Parse cannot decompose.
//
// The returned error NEVER echoes the userinfo back: only the scheme and host are
// named, so the embedded credential cannot leak into a log line or condition.
func validateRemoteNoUserinfo(remote string) error {
	u, err := url.Parse(strings.TrimSpace(remote))
	if err != nil || u.User == nil {
		// A parse failure here is the scp-like form ("git@host:path"), which
		// url.Parse cannot decompose; the remote regex admits it and ssh key auth
		// carries no secret in the URL. A nil User means no userinfo at all.
		return nil
	}
	_, hasPassword := u.User.Password()
	httpScheme := u.Scheme == "http" || u.Scheme == "https"
	if !hasPassword && !httpScheme {
		// A bare username on a non-http scheme (ssh "git@host") is conventional and
		// carries no secret; leave it alone.
		return nil
	}
	// Name only the scheme and host; the userinfo (the credential) is dropped.
	scheme := u.Scheme
	if scheme == "" {
		scheme = "(none)"
	}
	return fmt.Errorf(
		"git rendezvous remote embeds credentials in its URL userinfo (scheme %s, host %s): a token in the remote URL would be persisted into the revision/claim status; remove the userinfo and supply the push token via spec.git.CredentialsSecretRef instead",
		scheme, u.Host,
	)
}

// Credentials carries the resolved git push credentials for an authenticated
// rendezvous remote. The Token is a secret: it is NEVER logged, never placed on
// the git argv (so it cannot appear in a process table), and never returned in
// an error. It reaches git only through an ephemeral, mode 0o600
// .git-credentials file inside the isolated rendezvous HOME, which is removed
// when Rendezvous returns. A nil *Credentials means an unauthenticated push
// (the local-bare-repo and file:// case).
type Credentials struct {
	// Username is the basic-auth username for the remote (for example a git
	// forge user or "x-access-token" for a token-only forge). Not a secret.
	Username string
	// Token is the secret credential (a personal access token or password).
	// Treated as a secret value everywhere: never logged, never on argv, never
	// in an error string.
	Token string
}

// Rendezvous materializes repoFiles into a temp worktree, makes a single
// deterministic commit, and pushes it to remote on branch. repoFiles maps
// workspace-relative repo-path names to their content (resolved from the
// workspace spec.git.paths). It uses the git CLI via exec: git is present on the
// runners and images, so this adds no dependency. Empty repoFiles is a no-op
// (a {git} output with no spec.git.paths content is honest about having nothing
// to push). Git is the merge layer: this pushes a branch, it never merges.
//
// creds, when non-nil, authenticates the push to a real external rendezvous
// remote. The credential is delivered to git WITHOUT ever appearing on the
// argv: it is written to a mode 0o600 .git-credentials file inside the
// ephemeral, isolated HOME, with credential.helper=store reading only that
// file. The HOME (and therefore the credential file) is removed when this
// function returns. The token never enters a log line or the returned error.
//
// A push failure is returned (with the git output for remediation, scrubbed of
// the credential token), so the caller surfaces it on a condition rather than
// swallowing it.
func Rendezvous(ctx context.Context, repoFiles map[string]string, remote, branch string, creds *Credentials) error {
	if len(repoFiles) == 0 {
		return nil
	}
	if strings.TrimSpace(remote) == "" {
		return fmt.Errorf("git rendezvous: empty remote")
	}
	if err := validateRemoteNoUserinfo(remote); err != nil {
		return err
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
		nil, // placeholder for the push step, built below so it can carry creds
	}
	// An empty HOME and GIT_CONFIG_NOSYSTEM=1 isolate the push from ambient git
	// config (a controller image ~/.gitconfig or /etc/gitconfig), so no on-host
	// config can re-enable the ext::/fd:: transports or otherwise alter the push.
	gitHome, err := os.MkdirTemp("", "ws-rendezvous-home-*")
	if err != nil {
		return fmt.Errorf("git rendezvous home dir: %w", err)
	}
	defer os.RemoveAll(gitHome) //nolint:errcheck // best-effort cleanup

	// Build the push step. With credentials, we point git at a store credential
	// helper backed by an ephemeral, mode 0o600 .git-credentials file inside the
	// isolated HOME. The token is written ONLY to that file (never on argv, never
	// logged), and the file dies with the HOME on return. The "--" separator
	// forces remote and branch to be parsed as positional arguments, never as
	// flags. This closes the confirmed arg-injection RCE where a remote of
	// "--receive-pack=<cmd>" would otherwise be parsed as a flag and run an
	// arbitrary command on the pushing host.
	push := []string{"push", "--", remote, branch}
	if creds != nil && strings.TrimSpace(creds.Token) != "" {
		credFile, cerr := writeGitCredentials(gitHome, remote, creds)
		if cerr != nil {
			// cerr is built by writeGitCredentials and never includes the token.
			return cerr
		}
		// credential.helper=store reads only the scoped file in the isolated HOME.
		// The path is a non-secret temp path; the token is inside the file.
		push = []string{
			"-c", "credential.helper=store --file=" + credFile,
			"push", "--", remote, branch,
		}
	}
	steps[len(steps)-1] = push
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
			// Scrub the token from the git output defensively: git does not echo
			// the stored credential, but a misconfigured remote URL or a future
			// git could, so we never let the token reach the returned error.
			msg := scrubToken(strings.TrimSpace(string(out)), creds)
			return fmt.Errorf("git %s: %w: %s", args[0], err, msg)
		}
	}
	return nil
}

// writeGitCredentials writes a single mode 0o600 .git-credentials line into the
// isolated gitHome for remote, so credential.helper=store can authenticate the
// push without the token ever appearing on the git argv. It returns the file
// path. The token is written ONLY to the file; no error it returns includes the
// token value (only the non-secret remote host and the file path).
func writeGitCredentials(gitHome, remote string, creds *Credentials) (string, error) {
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		// A token-credential push requires an http(s) remote with a host; a
		// file:// or scp-like remote cannot carry basic-auth. Report without the
		// token.
		return "", fmt.Errorf("git rendezvous credentials: remote %q is not a credential-capable http(s) URL", remote)
	}
	// Build the credential line https://user:token@host. url.UserPassword
	// percent-encodes the username and token so a token with special characters
	// stays a single field.
	cu := *u
	cu.User = url.UserPassword(creds.Username, creds.Token)
	// Store only scheme://user:pass@host (path stripped) so the helper matches
	// any repo on that host, which is what credential.helper=store expects.
	cu.Path = ""
	cu.RawQuery = ""
	cu.Fragment = ""
	line := cu.String() + "\n"

	credFile := filepath.Join(gitHome, ".git-credentials")
	if err := os.WriteFile(credFile, []byte(line), 0o600); err != nil {
		// err from WriteFile is a filesystem error and never contains the token.
		return "", fmt.Errorf("git rendezvous credentials: write credential file: %w", err)
	}
	return credFile, nil
}

// scrubToken removes the credential token from a git output string before it is
// surfaced in an error. A nil creds or empty token is a no-op. This is defense
// in depth: the token never reaches git on the argv, so git should not echo it,
// but the secrets rule (CLAUDE.md) forbids the token from ever appearing in an
// error string regardless.
func scrubToken(s string, creds *Credentials) string {
	if creds == nil || creds.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, creds.Token, "[REDACTED]")
}
