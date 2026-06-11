package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capturedRequest records what the backend sent so assertions can inspect the
// method, path, headers, and decoded body of each HTTP call.
type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

// recordingServer returns an httptest.Server that records every request into
// got and replies with the per-path canned response. The handler func decides
// the response body and status for a given request.
func recordingServer(t *testing.T, got *[]capturedRequest, handler func(cr capturedRequest) (int, any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cr := capturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &cr.Body)
		}
		*got = append(*got, cr)
		status, body := handler(cr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
}

func TestHTTPBackendCreate(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"id": cr.Body["id"], "template_id": cr.Body["template"]}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	id, err := b.Create(context.Background(), "python")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty id")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 request, got %d", len(got))
	}
	req := got[0]
	if req.Method != http.MethodPost || req.Path != "/v1/fork" {
		t.Fatalf("Create sent %s %s, want POST /v1/fork", req.Method, req.Path)
	}
	if req.Auth != "Bearer tok-123" {
		t.Fatalf("Create auth = %q, want Bearer tok-123", req.Auth)
	}
	if req.Body["template"] != "python" {
		t.Fatalf("Create body template = %v, want python", req.Body["template"])
	}
	if req.Body["id"] != id {
		t.Fatalf("Create body id %v != returned id %s", req.Body["id"], id)
	}
}

func TestHTTPBackendExec(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"exit_code": 7, "stdout": "out", "stderr": "err"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	res, err := b.Exec(context.Background(), "sbx-1", "echo hi", 12)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("Exec result = %+v", res)
	}
	req := got[0]
	if req.Method != http.MethodPost || req.Path != "/v1/exec" {
		t.Fatalf("Exec sent %s %s, want POST /v1/exec", req.Method, req.Path)
	}
	if req.Auth != "Bearer tok-123" {
		t.Fatalf("Exec auth = %q", req.Auth)
	}
	if req.Body["sandbox"] != "sbx-1" || req.Body["command"] != "echo hi" {
		t.Fatalf("Exec body = %v", req.Body)
	}
	if req.Body["timeout"] != float64(12) {
		t.Fatalf("Exec timeout = %v, want 12", req.Body["timeout"])
	}
}

func TestHTTPBackendReadFile(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"content": "hello", "size": 5}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	content, err := b.ReadFile(context.Background(), "sbx-1", "/etc/hosts")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "hello" {
		t.Fatalf("ReadFile content = %q", content)
	}
	req := got[0]
	if req.Method != http.MethodPost || req.Path != "/v1/files/read" {
		t.Fatalf("ReadFile sent %s %s, want POST /v1/files/read", req.Method, req.Path)
	}
	if req.Body["sandbox"] != "sbx-1" || req.Body["path"] != "/etc/hosts" {
		t.Fatalf("ReadFile body = %v", req.Body)
	}
}

func TestHTTPBackendWriteFile(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"status": "ok"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	if err := b.WriteFile(context.Background(), "sbx-1", "/tmp/x", "data"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	req := got[0]
	if req.Method != http.MethodPost || req.Path != "/v1/files/write" {
		t.Fatalf("WriteFile sent %s %s, want POST /v1/files/write", req.Method, req.Path)
	}
	if req.Body["sandbox"] != "sbx-1" || req.Body["path"] != "/tmp/x" || req.Body["content"] != "data" {
		t.Fatalf("WriteFile body = %v", req.Body)
	}
}

func TestHTTPBackendFork(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"id": cr.Body["id"], "template_id": cr.Body["template"]}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	ids, err := b.Fork(context.Background(), "sbx-1", 3)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("Fork returned %d ids, want 3", len(ids))
	}
	if len(got) != 3 {
		t.Fatalf("Fork made %d requests, want 3", len(got))
	}
	seen := map[string]bool{}
	for i, req := range got {
		if req.Method != http.MethodPost || req.Path != "/v1/fork" {
			t.Fatalf("Fork req %d = %s %s", i, req.Method, req.Path)
		}
		if req.Auth != "Bearer tok-123" {
			t.Fatalf("Fork req %d auth = %q", i, req.Auth)
		}
		id, _ := req.Body["id"].(string)
		if id == "" || seen[id] {
			t.Fatalf("Fork req %d had empty or duplicate id %q", i, id)
		}
		seen[id] = true
	}
}

func TestHTTPBackendForkDefaultsToOne(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"id": cr.Body["id"]}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	ids, err := b.Fork(context.Background(), "sbx-1", 0)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != 1 || len(got) != 1 {
		t.Fatalf("Fork with 0 replicas made %d reqs / %d ids, want 1/1", len(got), len(ids))
	}
}

func TestHTTPBackendTerminate(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"status": "terminated"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	if err := b.Terminate(context.Background(), "sbx-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	req := got[0]
	if req.Method != http.MethodDelete || req.Path != "/v1/sandboxes/sbx-1" {
		t.Fatalf("Terminate sent %s %s, want DELETE /v1/sandboxes/sbx-1", req.Method, req.Path)
	}
	if req.Auth != "Bearer tok-123" {
		t.Fatalf("Terminate auth = %q", req.Auth)
	}
}

func TestHTTPBackendNoTokenOmitsHeader(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"id": cr.Body["id"]}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "", srv.Client())
	if _, err := b.Create(context.Background(), "python"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got[0].Auth != "" {
		t.Fatalf("expected no Authorization header without a token, got %q", got[0].Auth)
	}
}

func TestHTTPBackendNon2xxIsError(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusNotFound, map[string]any{"error": "template \"nope\" not found"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok-123", srv.Client())
	_, err := b.Create(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected an error on non-2xx response")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should carry response body context, got %q", err.Error())
	}
}

// TestHTTPBackendNeverLeaksToken asserts the token never appears in an error
// returned to the caller, even when the server echoes the Authorization header
// back in its error body. The backend must not log; this test guards the error
// path, the only string the backend surfaces.
func TestHTTPBackendNeverLeaksToken(t *testing.T) {
	const token = "super-secret-token-value"
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		// Hostile server echoes the presented auth header into its error body.
		return http.StatusInternalServerError, map[string]any{"error": cr.Auth}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, token, srv.Client())
	_, err := b.Exec(context.Background(), "sbx-1", "x", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestHTTPBackendUnsafeIDRejected asserts that Terminate and Exec with a
// path-traversal sandbox id return an error without sending any HTTP request.
func TestHTTPBackendUnsafeIDRejected(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, map[string]any{"status": "ok"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok", srv.Client())

	unsafeIDs := []string{"../../x", "../etc/passwd", "", "sbx/bad", "sbx..bad"}
	for _, id := range unsafeIDs {
		err := b.Terminate(context.Background(), id)
		if err == nil {
			t.Errorf("Terminate(%q): expected error, got nil", id)
		}
		_, err = b.Exec(context.Background(), id, "echo", 0)
		if err == nil {
			t.Errorf("Exec(%q): expected error, got nil", id)
		}
	}
	if len(got) != 0 {
		t.Errorf("backend sent %d requests, want 0", len(got))
	}
}

// TestHTTPBackendForkPartialIDsInError asserts that when a mid-loop fork fails,
// the returned error names the sandbox ids that were already created so the
// caller can terminate them.
func TestHTTPBackendForkPartialIDsInError(t *testing.T) {
	call := 0
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		call++
		if call == 1 {
			// First fork succeeds; capture the id it used.
			return http.StatusOK, map[string]any{"id": cr.Body["id"]}
		}
		// Second fork fails.
		return http.StatusInternalServerError, map[string]any{"error": "quota exceeded"}
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok", srv.Client())
	ids, err := b.Fork(context.Background(), "sbx-src", 2)
	if err == nil {
		t.Fatal("expected error on second fork, got nil")
	}
	// The error must name the id created in the first (successful) call.
	if len(ids) != 1 {
		t.Fatalf("expected 1 partial id returned, got %d: %v", len(ids), ids)
	}
	if !strings.Contains(err.Error(), ids[0]) {
		t.Errorf("error %q does not mention created id %q", err.Error(), ids[0])
	}
}

// TestHTTPBackendTerminatePathEscape asserts that a valid sandbox id that
// contains URL-special chars (none allowed by validSandboxID, but an id whose
// PathEscape is a no-op for safe chars) is embedded correctly.
func TestHTTPBackendTerminatePathEscape(t *testing.T) {
	var got []capturedRequest
	srv := recordingServer(t, &got, func(cr capturedRequest) (int, any) {
		return http.StatusOK, nil
	})
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, "tok", srv.Client())
	// sbx-abc is valid and path-safe; confirm the path is built correctly.
	if err := b.Terminate(context.Background(), "sbx-abc"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/v1/sandboxes/sbx-abc" {
		t.Fatalf("path = %q, want /v1/sandboxes/sbx-abc", got[0].Path)
	}
}
