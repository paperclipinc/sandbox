// Package apierr defines the LLM-legible error envelope returned by the forkd
// sandbox API and the standalone sandbox-server. Every runtime error carries a
// stable machine code, a one-line message, an underlying cause, and an
// actionable remediation, per docs/api/v2-spec.md section 2.3 and issue #28.
//
// Security: an Error never carries a secret value. Cause and message are built
// from sandbox ids, paths, and operation names only; callers must never place a
// token, secret value, or credential into any field. Logging an Error logs the
// code and message, never the request body.
package apierr

import (
	"encoding/json"
	"net/http"
)

// Error is one LLM-legible error. Status is the HTTP status to send and is not
// serialized into the body.
type Error struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Cause       string         `json:"cause,omitempty"`
	Remediation string         `json:"remediation"`
	Context     map[string]any `json:"context,omitempty"`
	Status      int            `json:"-"`
}

// WithCause returns a copy of e with the cause set. It does not mutate e, so a
// Catalogue entry can be reused safely. The cause must never contain a secret
// value.
func (e Error) WithCause(cause string) Error {
	e.Cause = cause
	return e
}

// WithContext returns a copy of e with the context map set. The context must
// never contain a secret value.
func (e Error) WithContext(ctx map[string]any) Error {
	e.Context = ctx
	return e
}

// envelope is the wire shape: {"error": {...}}.
type envelope struct {
	Error Error `json:"error"`
}

// Encode writes e as the JSON envelope with e.Status. Code and Remediation are
// always populated by the Catalogue, so a well-formed Error always satisfies the
// CI lint that rejects an error response lacking code or remediation.
func Encode(w http.ResponseWriter, e Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(envelope{Error: e})
}

// Catalogue is the stable set of runtime error codes. Handlers pick the closest
// entry and attach a cause with WithCause. Adding an entry is a documented
// surface change; keep docs/api/v2-spec.md and the error-code catalogue in step.
var Catalogue = map[string]Error{
	"invalid_json": {
		Code:        "invalid_json",
		Message:     "request body is not valid JSON",
		Remediation: "Send a JSON body matching the sandbox API contract for this endpoint.",
		Status:      http.StatusBadRequest,
	},
	"body_too_large": {
		Code:        "body_too_large",
		Message:     "request body exceeds the size limit",
		Remediation: "Reduce the payload; file content is hex-encoded and bounded by the server.",
		Status:      http.StatusRequestEntityTooLarge,
	},
	"unauthorized": {
		Code:        "unauthorized",
		Message:     "the bearer token is missing or invalid for this sandbox",
		Remediation: "Send Authorization: Bearer <token> with the per-sandbox token from the <name>-sandbox-token Secret.",
		Status:      http.StatusUnauthorized,
	},
	"not_found": {
		Code:        "not_found",
		Message:     "no such sandbox",
		Remediation: "Confirm the sandbox id exists and is Ready before calling.",
		Status:      http.StatusNotFound,
	},
	"exec_failed": {
		Code:        "exec_failed",
		Message:     "the command could not be executed in the sandbox",
		Remediation: "Inspect the cause; if it is a transport error retry, otherwise check the forkd logs for the guest agent state.",
		Status:      http.StatusInternalServerError,
	},
	"file_failed": {
		Code:        "file_failed",
		Message:     "the file operation failed in the sandbox",
		Remediation: "Confirm the path exists and is writable; inspect the cause for the underlying error.",
		Status:      http.StatusInternalServerError,
	},
	"internal": {
		Code:        "internal",
		Message:     "an internal error occurred",
		Remediation: "Retry the request; if it persists, inspect the forkd or sandbox-server logs.",
		Status:      http.StatusInternalServerError,
	},
}

// Lookup returns the catalogue entry for code, falling back to the generic
// internal error so a handler always has a well-formed Error.
func Lookup(code string) Error {
	if e, ok := Catalogue[code]; ok {
		return e
	}
	return Catalogue["internal"]
}
