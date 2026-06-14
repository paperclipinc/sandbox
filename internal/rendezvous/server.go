// Package rendezvous is a minimal authenticated git-http rendezvous server: the
// real external remote the {git} workspace output pushes per-attempt branches
// to. It speaks the git smart-HTTP protocol by shelling out to the stock
// `git http-backend` CGI, gated behind HTTP basic-auth whose credentials come
// from configuration (a Kubernetes Secret in deployment). It is the
// counterpart to the credentialed push in internal/workspace/git.go: that side
// supplies the token, this side checks it.
//
// Security: the configured token is a secret VALUE. It is compared in constant
// time and is NEVER logged, never written to a response body, and never placed
// on a child-process argv. The git-http-backend child receives only the request
// over CGI env+stdin; the token is consumed by the auth check before the child
// runs.
package rendezvous

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Config configures the rendezvous server.
type Config struct {
	// Root is the directory holding the bare git repositories. Each pushed repo
	// path "<name>.git" is created here on first push.
	Root string
	// Username is the basic-auth username the push must present. Not a secret.
	Username string
	// Token is the basic-auth password the push must present. A secret VALUE:
	// never logged, never echoed, compared in constant time.
	Token string
	// Realm is the basic-auth realm advertised in the WWW-Authenticate challenge.
	// Defaults to "mitos-rendezvous".
	Realm string
}

// Server is the authenticated git-http rendezvous handler.
type Server struct {
	cfg      Config
	execPath string // git core exec path holding git-http-backend
	gitBin   string
}

// New validates the config, discovers git, and returns the server. It returns an
// error (never carrying the token) when git is missing or the root is unusable.
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, fmt.Errorf("rendezvous: empty repository root")
	}
	if strings.TrimSpace(cfg.Username) == "" || strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("rendezvous: username and token are required")
	}
	if cfg.Realm == "" {
		cfg.Realm = "mitos-rendezvous"
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("rendezvous: git not found on PATH: %w", err)
	}
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return nil, fmt.Errorf("rendezvous: create root %q: %w", cfg.Root, err)
	}
	// git-http-backend lives in git's core exec path, which may not be on PATH.
	out, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		return nil, fmt.Errorf("rendezvous: resolve git exec-path: %w", err)
	}
	execPath := strings.TrimSpace(string(out))
	return &Server{cfg: cfg, execPath: execPath, gitBin: gitBin}, nil
}

// ServeHTTP authenticates the request, ensures the target bare repo exists, then
// hands the request to git-http-backend over CGI.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(r) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q", s.cfg.Realm))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Ensure the bare repo for this request exists so a first push lands. The
	// repo name is the first path segment ending in ".git"; reject traversal.
	if err := s.ensureRepo(r.URL.Path); err != nil {
		// err never contains the token; it names the path only.
		http.Error(w, "invalid repository path", http.StatusBadRequest)
		return
	}

	handler := &cgi.Handler{
		Path: filepath.Join(s.execPath, "git-http-backend"),
		Dir:  s.cfg.Root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.cfg.Root,
			// Allow serving every repo under the root without a per-repo
			// http.uploadpack/receivepack flag; auth is enforced above.
			"GIT_HTTP_EXPORT_ALL=1",
			"HOME=" + s.cfg.Root,
			"GIT_CONFIG_NOSYSTEM=1",
		},
	}
	handler.ServeHTTP(w, r)
}

// authenticated checks HTTP basic-auth against the configured credentials in
// constant time. A missing or malformed header is unauthenticated.
func (s *Server) authenticated(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.Token)) == 1
	return userOK && passOK
}

// ensureRepo creates the bare repository named by the first ".git" path segment
// if it does not already exist, rejecting any path traversal so a crafted path
// cannot escape the root.
func (s *Server) ensureRepo(urlPath string) error {
	name := repoNameFromPath(urlPath)
	if name == "" {
		return fmt.Errorf("no repository in path %q", urlPath)
	}
	// Reject traversal and absolute escapes: the cleaned join must stay inside root.
	repoDir := filepath.Join(s.cfg.Root, name)
	rel, err := filepath.Rel(s.cfg.Root, repoDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("repository path %q escapes root", urlPath)
	}
	if _, err := os.Stat(repoDir); err == nil {
		return nil
	}
	cmd := exec.Command(s.gitBin, "init", "-q", "--bare", repoDir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("init bare repo %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	// git-http-backend refuses receive-pack (push) unless the repo opts in. Auth
	// is already enforced at the HTTP layer, so enabling it here is safe.
	cfg := exec.Command(s.gitBin, "-C", repoDir, "config", "http.receivepack", "true")
	cfg.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cfg.CombinedOutput(); err != nil {
		return fmt.Errorf("enable receive-pack on %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// repoNameFromPath extracts the "<name>.git" segment from a smart-HTTP URL path
// such as "/proj.git/info/refs" or "/proj.git/git-receive-pack". It returns an
// empty string when no ".git" segment is present.
func repoNameFromPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	for _, seg := range strings.Split(p, "/") {
		if strings.HasSuffix(seg, ".git") {
			// A single clean segment, no traversal markers.
			if seg == "." || seg == ".." || strings.Contains(seg, "\x00") {
				return ""
			}
			return seg
		}
	}
	return ""
}
