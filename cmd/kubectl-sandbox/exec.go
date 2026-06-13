package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// execResult is the decoded sandbox exec response: the exit code and the
// captured streams. It mirrors the forkd /v1/exec response (vsock.ExecResponse).
type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// runExec resolves the claim's endpoint and per-sandbox bearer token, then runs
// the command over the sandbox HTTP API and streams the result to stdout/stderr.
// The exit code of the in-sandbox command becomes this process's exit code so
// scripts can branch on it.
func runExec(namespace, name string, cmd []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, endpoint, token, err := resolveSandboxAuth(ctx, c, namespace, name)
	if err != nil {
		return err
	}

	res, err := execSandbox(ctx, http.DefaultClient, endpoint, token, ref, strings.Join(cmd, " "))
	if err != nil {
		return err
	}
	if res.Stdout != "" {
		fmt.Fprint(os.Stdout, res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}

// resolveSandboxAuth reads the claim and its owned <claim>-sandbox-token Secret
// to recover the sandbox ref, the forkd HTTP endpoint, and the bearer token the
// sandbox API requires. The ref mirrors the SDK: Status.SandboxID, falling back
// to the claim name. A missing claim, a not-running claim, or a missing token
// Secret each returns a clear, actionable error rather than a hang.
func resolveSandboxAuth(ctx context.Context, c client.Client, namespace, name string) (ref, endpoint, token string, err error) {
	var claim v1alpha1.SandboxClaim
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", "", fmt.Errorf("sandbox %q not found in namespace %q", name, namespace)
		}
		return "", "", "", fmt.Errorf("get claim: %w", err)
	}
	if claim.Status.Phase != v1alpha1.SandboxReady {
		return "", "", "", fmt.Errorf("sandbox %q is %s, not Ready: exec needs a running sandbox", name, orUnknownPhase(claim.Status.Phase))
	}
	endpoint = claim.Status.Endpoint
	if endpoint == "" {
		return "", "", "", fmt.Errorf("sandbox %q has no endpoint yet: exec needs a running sandbox", name)
	}
	ref = claim.Status.SandboxID
	if ref == "" {
		ref = claim.Name
	}

	secretName := name + "-sandbox-token"
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", "", fmt.Errorf("token Secret %q not found: the sandbox API requires a bearer token; wait for the claim to go Ready or check the controller", secretName)
		}
		return "", "", "", fmt.Errorf("get token Secret %q: %w", secretName, err)
	}
	tokenBytes, ok := secret.Data["token"]
	if !ok || len(tokenBytes) == 0 {
		return "", "", "", fmt.Errorf("token Secret %q has no token key: cannot authenticate to the sandbox API", secretName)
	}
	// The token VALUE is the bearer credential. It is never logged or echoed.
	return ref, endpoint, string(tokenBytes), nil
}

// orUnknownPhase renders a SandboxPhase, or "Unknown" when empty, so the
// not-running error always names a phase.
func orUnknownPhase(p v1alpha1.SandboxPhase) string {
	if p == "" {
		return "Unknown"
	}
	return string(p)
}

// execSandbox POSTs the command to <endpoint>/v1/exec with the per-sandbox
// bearer token (the SAME gate the SDK uses; auth is never bypassed) and decodes
// the result. A 401 means the token was rejected; a non-2xx surfaces the
// server's message; a transport error is wrapped. The token value never appears
// in an error.
func execSandbox(ctx context.Context, httpc *http.Client, endpoint, token, ref, command string) (execResult, error) {
	body, err := json.Marshal(map[string]any{
		"sandbox": ref,
		"command": command,
	})
	if err != nil {
		return execResult{}, fmt.Errorf("encode exec request: %w", err)
	}
	url := fmt.Sprintf("http://%s/v1/exec", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return execResult{}, fmt.Errorf("build exec request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpc.Do(req)
	if err != nil {
		return execResult{}, fmt.Errorf("reach sandbox API at %s: %w (is the sandbox running and the endpoint routable?)", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return execResult{}, fmt.Errorf("sandbox API rejected the bearer token (401): the token Secret may be stale")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Redact any echo of the bearer token from the server's body before it
		// reaches an error string a caller may log.
		safe := strings.ReplaceAll(strings.TrimSpace(string(msg)), token, "[REDACTED]")
		return execResult{}, fmt.Errorf("sandbox API returned %d: %s", resp.StatusCode, safe)
	}

	var res execResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return execResult{}, fmt.Errorf("decode exec response: %w", err)
	}
	return res, nil
}
