package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTPBackend implements SandboxBackend over the standalone sandbox-server REST
// API (cmd/sandbox-server). It is the simplest real backend: one process, plain
// HTTP, a single launch-time bearer token. A Kubernetes claim backend (create a
// SandboxClaim, read its token Secret, exec via forkd) is a planned follow-up
// and is intentionally not implemented here.
//
// Token scoping: every request carries the launch-time bearer token, so the MCP
// server can do exactly what that token authorizes on the sandbox-server and
// nothing more. The token is never logged and never placed in an error message;
// see do, which redacts any echo of the token from a response body before using
// it as error context.
//
// Pool-to-template mapping: the MCP sandbox_create tool takes a "pool" name. The
// sandbox-server has no pools; it forks a sandbox from a named template. The
// HTTP backend therefore treats the pool argument as the template id and forks
// from it (POST /v1/fork {template, id}). On a real k8s deployment a pool maps
// to a SandboxPool; that mapping belongs to the future k8s backend.
type HTTPBackend struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPBackend builds a backend against the sandbox-server at baseURL. When
// token is non-empty it is sent as "Authorization: Bearer <token>" on every
// request. A nil client defaults to http.DefaultClient.
func NewHTTPBackend(baseURL, token string, client *http.Client) *HTTPBackend {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPBackend{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  client,
	}
}

// newSandboxID returns a random hex id for a sandbox or fork. crypto/rand makes
// collisions across concurrent callers negligible without a uuid dependency.
func newSandboxID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read never fails on supported platforms; degrade rather than panic.
		return "sbx-fallback"
	}
	return "sbx-" + hex.EncodeToString(b[:])
}

// do issues an HTTP request to path with an optional JSON body and decodes a
// 2xx JSON response into out (when out is non-nil). A non-2xx status becomes an
// error carrying the response body as context, with any echo of the bearer
// token redacted first so the secret never reaches a caller, a log, or an LLM.
func (b *HTTPBackend) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s request: %w", path, err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, b.redact(string(respBody)))
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// redact removes any occurrence of the bearer token from s. A hostile or
// misconfigured server might echo the Authorization header into its error body;
// this guarantees the token never escapes through an error string.
func (b *HTTPBackend) redact(s string) string {
	if b.token == "" {
		return s
	}
	return strings.ReplaceAll(s, b.token, "[REDACTED]")
}

type forkResponse struct {
	ID         string `json:"id"`
	TemplateID string `json:"template_id"`
}

// Create forks a new sandbox from the pool, treating pool as the sandbox-server
// template id. It generates the sandbox id client-side and returns it.
func (b *HTTPBackend) Create(ctx context.Context, pool string) (string, error) {
	id := newSandboxID()
	var resp forkResponse
	if err := b.do(ctx, http.MethodPost, "/v1/fork", map[string]any{
		"template": pool,
		"id":       id,
	}, &resp); err != nil {
		return "", err
	}
	if resp.ID != "" {
		return resp.ID, nil
	}
	return id, nil
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Exec runs command in the sandbox via POST /v1/exec. A timeoutSec of 0 omits
// the field so the server default applies.
func (b *HTTPBackend) Exec(ctx context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error) {
	reqBody := map[string]any{
		"sandbox": sandboxID,
		"command": command,
	}
	if timeoutSec > 0 {
		reqBody["timeout"] = timeoutSec
	}
	var resp execResponse
	if err := b.do(ctx, http.MethodPost, "/v1/exec", reqBody, &resp); err != nil {
		return ExecResult{}, err
	}
	return ExecResult(resp), nil
}

type readFileResponse struct {
	Content string `json:"content"`
}

// ReadFile reads path from the sandbox via POST /v1/files/read.
func (b *HTTPBackend) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	var resp readFileResponse
	if err := b.do(ctx, http.MethodPost, "/v1/files/read", map[string]any{
		"sandbox": sandboxID,
		"path":    path,
	}, &resp); err != nil {
		return "", err
	}
	return resp.Content, nil
}

// WriteFile writes content to path in the sandbox via POST /v1/files/write.
func (b *HTTPBackend) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	return b.do(ctx, http.MethodPost, "/v1/files/write", map[string]any{
		"sandbox": sandboxID,
		"path":    path,
		"content": content,
	}, nil)
}

// Fork forks the sandbox replicas times. The sandbox-server fork endpoint has
// no replicas parameter, so this issues one POST /v1/fork per replica. Caveat:
// the sandbox-server /v1/fork "template" field is a template lookup, so this
// passes the source sandbox id as the template key; on the standalone server a
// fork-of-a-fork therefore requires that key to resolve to a known template.
// The richer k8s SandboxFork path (true fork-of-a-running-sandbox) belongs to
// the future k8s backend. A replicas value below 1 is treated as 1.
func (b *HTTPBackend) Fork(ctx context.Context, sandboxID string, replicas int) ([]string, error) {
	if replicas < 1 {
		replicas = 1
	}
	ids := make([]string, 0, replicas)
	for i := 0; i < replicas; i++ {
		id := newSandboxID()
		var resp forkResponse
		if err := b.do(ctx, http.MethodPost, "/v1/fork", map[string]any{
			"template": sandboxID,
			"id":       id,
		}, &resp); err != nil {
			return ids, err
		}
		if resp.ID != "" {
			ids = append(ids, resp.ID)
		} else {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Terminate destroys the sandbox via DELETE /v1/sandboxes/{id}.
func (b *HTTPBackend) Terminate(ctx context.Context, sandboxID string) error {
	return b.do(ctx, http.MethodDelete, "/v1/sandboxes/"+sandboxID, nil, nil)
}
