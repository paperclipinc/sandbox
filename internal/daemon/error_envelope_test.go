package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decodeEnvelope asserts the body is {"error":{code,message,remediation,...}}
// and returns the inner error.
func decodeEnvelope(t *testing.T, body []byte) struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Cause       string `json:"cause"`
	Remediation string `json:"remediation"`
} {
	t.Helper()
	var env struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Cause       string `json:"cause"`
			Remediation string `json:"remediation"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v (body=%q)", err, body)
	}
	if env.Error.Code == "" {
		t.Fatalf("error.code empty (body=%q)", body)
	}
	if env.Error.Remediation == "" {
		t.Fatalf("error.remediation empty (body=%q)", body)
	}
	return env.Error
}

func newEnvelopeTestAPI(t *testing.T) *SandboxAPI {
	t.Helper()
	api := NewSandboxAPI(t.TempDir())
	api.AllowTokenless()
	return api
}

func TestExecInvalidJSONReturnsEnvelope(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader([]byte("not json")))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	got := decodeEnvelope(t, rr.Body.Bytes())
	if got.Code != "invalid_json" {
		t.Fatalf("code = %q, want invalid_json", got.Code)
	}
}

func TestExecUnknownSandboxReturnsNotFoundEnvelope(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	body, _ := json.Marshal(map[string]any{"sandbox": "sb-missing", "command": "true"})
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	got := decodeEnvelope(t, rr.Body.Bytes())
	if got.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", got.Code)
	}
}
