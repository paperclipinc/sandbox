package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// NewHTTPHandler returns an http.Handler that serves a Store's read surface for
// incremental pull:
//
//	GET  /cas/manifest/{digest}  canonical manifest bytes
//	POST /cas/has                JSON []Digest body, JSON map[Digest]bool reply
//	GET  /cas/chunk/{digest}     raw chunk bytes
//
// Only digests are ever exposed; no secret values cross this surface. Authn and
// authz (mTLS gating) are the caller's responsibility, layered around this
// handler.
//
// Every digest taken from the request (the {digest} path segment, or each entry
// in the /cas/has body) is validated against the strict sha256 hex allowlist
// before any store or filesystem access. A malformed digest returns 400 Bad
// Request: this is the barrier that blocks path traversal via the request URL
// (e.g. an encoded "../../etc/passwd"). /cas/has rejects the whole request if
// any digest in the body is invalid.
func NewHTTPHandler(store *Store) http.Handler {
	h := &httpHandler{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cas/manifest/{digest}", h.getManifest)
	mux.HandleFunc("POST /cas/has", h.postHas)
	mux.HandleFunc("GET /cas/chunk/{digest}", h.getChunk)
	return mux
}

type httpHandler struct {
	store *Store
}

func (h *httpHandler) getManifest(w http.ResponseWriter, r *http.Request) {
	d := Digest(r.PathValue("digest"))
	if err := d.Validate(); err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}
	m, err := h.store.GetManifest(d)
	if err != nil {
		http.Error(w, fmt.Sprintf("manifest %s not found", d), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(m.Canonical()) //nolint:errcheck // client disconnect is not actionable
}

func (h *httpHandler) postHas(w http.ResponseWriter, r *http.Request) {
	var digests []Digest
	if err := json.NewDecoder(r.Body).Decode(&digests); err != nil {
		http.Error(w, "invalid digest list", http.StatusBadRequest)
		return
	}
	// Reject the whole request if any digest is malformed. The body is
	// attacker-controlled, so an invalid digest is treated as a bad request
	// rather than silently coerced to not-present.
	for _, d := range digests {
		if err := d.Validate(); err != nil {
			http.Error(w, "invalid digest", http.StatusBadRequest)
			return
		}
	}
	present := make(map[Digest]bool, len(digests))
	for _, d := range digests {
		present[d] = h.store.HasChunk(d)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(present) //nolint:errcheck // client disconnect is not actionable
}

func (h *httpHandler) getChunk(w http.ResponseWriter, r *http.Request) {
	d := Digest(r.PathValue("digest"))
	if err := d.Validate(); err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}
	f, err := os.Open(h.store.chunkPath(d)) //nolint:gosec // digest validated above against the strict hex allowlist
	if err != nil {
		http.Error(w, fmt.Sprintf("chunk %s not found", d), http.StatusNotFound)
		return
	}
	defer f.Close() //nolint:errcheck // read-only file
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f) //nolint:errcheck // client disconnect is not actionable
}

// HTTPTransport is a Transport that talks to a NewHTTPHandler endpoint over
// HTTP. It is the transport the future P2P layer uses between nodes.
type HTTPTransport struct {
	baseURL string
	client  *http.Client
}

// NewHTTPTransport builds a transport against baseURL (the root the handler is
// mounted at, e.g. http://node:9092). If client is nil, http.DefaultClient is
// used.
func NewHTTPTransport(baseURL string, client *http.Client) *HTTPTransport {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPTransport{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
	}
}

// HasChunks asks the remote which of digests it holds.
func (t *HTTPTransport) HasChunks(ctx context.Context, digests []Digest) (map[Digest]bool, error) {
	body, err := json.Marshal(digests)
	if err != nil {
		return nil, fmt.Errorf("marshal digests: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/cas/has", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build has request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post has: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("has returned status %d", resp.StatusCode)
	}
	var present map[Digest]bool
	if err := json.NewDecoder(resp.Body).Decode(&present); err != nil {
		return nil, fmt.Errorf("decode has response: %w", err)
	}
	return present, nil
}

// GetChunk streams a chunk's bytes from the remote. The caller closes the
// reader. The bytes are not trusted until verified by PutChunk.
func (t *HTTPTransport) GetChunk(ctx context.Context, d Digest) (io.ReadCloser, error) {
	u := t.baseURL + "/cas/chunk/" + url.PathEscape(string(d))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build chunk request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get chunk %s: %w", d, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() //nolint:errcheck // discarding error body
		return nil, fmt.Errorf("get chunk %s returned status %d", d, resp.StatusCode)
	}
	return resp.Body, nil
}

// GetManifest fetches and decodes a manifest from the remote.
func (t *HTTPTransport) GetManifest(ctx context.Context, d Digest) (Manifest, error) {
	u := t.baseURL + "/cas/manifest/" + url.PathEscape(string(d))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("build manifest request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return Manifest{}, fmt.Errorf("get manifest %s: %w", d, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body
	if resp.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("get manifest %s returned status %d", d, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %s: %w", d, err)
	}
	return decodeManifest(data)
}
