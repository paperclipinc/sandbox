package apierr

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestEncodeWritesEnvelopeWithCodeAndRemediation(t *testing.T) {
	rr := httptest.NewRecorder()
	Encode(rr, Error{
		Code:        "not_found",
		Message:     "sandbox not found",
		Cause:       "no sandbox registered for id sb-1",
		Remediation: "Confirm the sandbox id exists and is Ready before calling.",
		Status:      404,
	})

	if rr.Code != 404 {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var got struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Cause       string `json:"cause"`
			Remediation string `json:"remediation"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(rr.Body.Bytes())).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", got.Error.Code)
	}
	if got.Error.Remediation == "" {
		t.Fatal("remediation must not be empty")
	}
	if got.Error.Message != "sandbox not found" {
		t.Fatalf("message = %q", got.Error.Message)
	}
}

func TestCatalogueEntriesAllCarryCodeAndRemediation(t *testing.T) {
	for name, e := range Catalogue {
		if e.Code == "" {
			t.Errorf("catalogue %q: empty code", name)
		}
		if e.Remediation == "" {
			t.Errorf("catalogue %q (%s): empty remediation", name, e.Code)
		}
		if e.Status < 400 || e.Status > 599 {
			t.Errorf("catalogue %q (%s): status %d not a 4xx/5xx", name, e.Code, e.Status)
		}
	}
}

func TestWithCausePreservesCatalogueFieldsAndDoesNotMutate(t *testing.T) {
	base := Catalogue["not_found"]
	withCause := base.WithCause("no sandbox registered for id sb-9")
	if withCause.Cause != "no sandbox registered for id sb-9" {
		t.Fatalf("cause = %q", withCause.Cause)
	}
	if base.Cause != "" {
		t.Fatal("WithCause must not mutate the catalogue entry")
	}
	if withCause.Code != base.Code || withCause.Remediation != base.Remediation {
		t.Fatal("WithCause must preserve code and remediation")
	}
}

func TestEnvelopeJSONKeysAreStable(t *testing.T) {
	rr := httptest.NewRecorder()
	Encode(rr, Catalogue["not_found"].WithCause("x"))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["error"]; !ok {
		t.Fatal("top-level key must be \"error\"")
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw["error"], &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	for _, k := range []string{"code", "message", "remediation"} {
		if _, ok := inner[k]; !ok {
			t.Errorf("error object missing required key %q", k)
		}
	}
}
