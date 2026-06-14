// Command rendezvous-server is the minimal authenticated git-http rendezvous
// server: the external remote a workspace {git} output pushes per-attempt
// branches to. It wraps internal/rendezvous and is deployed alongside the
// controller with its basic-auth token mounted from a Kubernetes Secret.
//
// Security: the token is read from a file (a mounted Secret) or, as a fallback,
// the RENDEZVOUS_TOKEN environment variable. It is NEVER accepted on an argv
// flag (which would expose it in the process table) and is never logged.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/paperclipinc/mitos/internal/rendezvous"
)

func main() {
	addr := flag.String("addr", ":9092", "listen address for the rendezvous git-http server")
	root := flag.String("root", "/var/lib/mitos/rendezvous", "directory holding the bare rendezvous repositories")
	username := flag.String("username", "x-access-token", "basic-auth username the push must present (not a secret)")
	tokenFile := flag.String("token-file", "", "path to a file holding the basic-auth token (a mounted Secret); falls back to RENDEZVOUS_TOKEN")
	realm := flag.String("realm", "mitos-rendezvous", "basic-auth realm advertised to clients")
	flag.Parse()

	token, err := loadToken(*tokenFile)
	if err != nil {
		// loadToken never includes the token value in its error.
		log.Fatalf("rendezvous-server: %v", err)
	}

	srv, err := rendezvous.New(rendezvous.Config{
		Root:     *root,
		Username: *username,
		Token:    token,
		Realm:    *realm,
	})
	if err != nil {
		log.Fatalf("rendezvous-server: %v", err)
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
	}
	// Log only non-secret facts: the address and root, never the token.
	log.Printf("rendezvous-server: listening on %s, repositories under %s", *addr, *root)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("rendezvous-server: %v", err)
	}
}

// loadToken reads the basic-auth token from tokenFile, or from the
// RENDEZVOUS_TOKEN environment variable when no file is given. It trims a single
// trailing newline (mounted Secrets often carry one). Its error never contains
// the token value, only the source path or variable name.
func loadToken(tokenFile string) (string, error) {
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file %q: %w", tokenFile, err)
		}
		t := strings.TrimRight(string(b), "\n")
		if t == "" {
			return "", fmt.Errorf("token file %q is empty", tokenFile)
		}
		return t, nil
	}
	t := os.Getenv("RENDEZVOUS_TOKEN")
	if strings.TrimSpace(t) == "" {
		return "", fmt.Errorf("no token: pass -token-file or set RENDEZVOUS_TOKEN")
	}
	return t, nil
}
