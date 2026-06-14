package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/paperclipinc/mitos/internal/apierr"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// ptySubprotocol is the WebSocket subprotocol the PTY endpoint speaks. Clients
// (the SDKs and browser xterm.js front ends) must offer it.
const ptySubprotocol = "mitos.pty.v1"

// ptyAuth authenticates a PTY WebSocket upgrade. Unlike requireBearer (which
// peeks a JSON request body), the upgrade is a bodyless GET, so the sandbox id
// comes from the ?sandbox= query parameter and the token from the
// Authorization: Bearer header. Semantics match requireBearer exactly:
//   - no token registered: 401 (fail closed) unless allowTokenless
//   - missing/malformed Authorization: 401
//   - mismatch: 401 (constant-time compare)
//
// Token values are never logged. Returns the resolved sandbox id on success.
func (api *SandboxAPI) ptyAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	requested := r.URL.Query().Get("sandbox")
	if requested == "" {
		writeErr(w, "missing sandbox query parameter", http.StatusBadRequest)
		return "", false
	}

	// In single-sandbox mode (husk-stub) the ?sandbox= id is whatever the SDK
	// sent (the husk pod name); resolve it to the one served sandbox id so the
	// token lookup hits the single registered token and the returned id routes
	// the PTY to the single VM. In forkd's default multi-sandbox mode this is the
	// request id unchanged, so the per-id gate is byte-identical.
	sandbox := api.resolveSandboxID(requested)

	api.mu.RLock()
	token, hasToken := api.tokens[sandbox]
	api.mu.RUnlock()

	if !hasToken {
		if api.allowTokenless {
			return sandbox, true
		}
		writeErr(w, "unauthorized: no token registered for sandbox", http.StatusUnauthorized)
		return "", false
	}

	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeErr(w, "unauthorized: bearer token required", http.StatusUnauthorized)
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
		writeErr(w, "unauthorized: invalid token", http.StatusUnauthorized)
		return "", false
	}
	return sandbox, true
}

// handlePty upgrades to a WebSocket and bridges it to a dedicated vsock PTY
// stream: client text frames (input/resize) are written to the guest, guest
// output/exit frames are forwarded to the client. A PTY is a live interactive
// shell into the VM, so the upgrade is gated by the per-sandbox bearer token
// (ptyAuth). The endpoint is registered OUTSIDE requireBearer because the
// upgrade carries no JSON body for that middleware to peek.
func (api *SandboxAPI) handlePty(w http.ResponseWriter, r *http.Request) {
	sandbox, ok := api.ptyAuth(w, r)
	if !ok {
		return
	}
	api.touch(sandbox)

	if _, err := api.getAgent(sandbox); err != nil {
		writeErr(w, err.Error(), http.StatusNotFound)
		return
	}

	// Per-sandbox concurrent-stream cap (production-blocker #2, cap 3): a PTY
	// holds a dedicated vsock connection for the session lifetime, so it counts
	// against the same per-sandbox ceiling as streaming exec and run_code. Check
	// the cap BEFORE the WebSocket upgrade so a rejection is a clean 429 envelope
	// rather than a post-upgrade close; existing streams are never touched.
	release, ok := api.acquireStream(sandbox)
	if !ok {
		writeAPIErr(w, apierr.Catalogue["too_many_streams"].WithCause(fmt.Sprintf("sandbox %s is at its concurrent-stream limit", sandbox)))
		return
	}
	defer release()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{ptySubprotocol},
	})
	if err != nil {
		return // Accept already wrote the failure status.
	}
	ctx := r.Context()
	defer c.Close(websocket.StatusNormalClosure, "")

	sc, err := api.dialStream(sandbox)
	if err != nil {
		c.Close(websocket.StatusInternalError, "pty backend unavailable")
		return
	}
	defer sc.Close()

	// Parse the open request from the query (cols/rows). Command is
	// intentionally NOT taken from the client to avoid letting a client spawn an
	// arbitrary first process; the guest defaults to /bin/sh. cols/rows are
	// bounded smallints.
	openReq := &vsock.PtyRequest{
		Cols: atoiDefault(r.URL.Query().Get("cols"), 80),
		Rows: atoiDefault(r.URL.Query().Get("rows"), 24),
	}

	// Reader pump: WebSocket -> guest. Decodes the client's input/resize frames
	// and forwards them on the vsock stream via SendInput/Resize.
	go func() {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				sc.Close() // dropping the vsock conn makes the guest kill the shell
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var f vsock.PtyFrame
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			switch f.Kind {
			case vsock.PtyInput:
				_ = sc.SendInput(f.Data)
			case vsock.PtyResize:
				_ = sc.Resize(f.Cols, f.Rows)
			}
		}
	}()

	// Writer pump runs on this goroutine: guest output frames -> WebSocket. The
	// terminal exit frame is forwarded then the loop returns.
	exit, perr := sc.Pty(ctx, openReq, func(out []byte) error {
		fb, mErr := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyOutput, Data: out})
		if mErr != nil {
			return mErr
		}
		return c.Write(ctx, websocket.MessageText, fb)
	})
	if perr != nil && !errors.Is(perr, context.Canceled) {
		// The stream failed mid-session; emit a terminal exit frame with the
		// actionable cause (never a secret) and close.
		eb, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: 1, Error: fmt.Sprintf("pty stream failed: %v", perr)})
		_ = c.Write(ctx, websocket.MessageText, eb)
		return
	}
	if exit != nil {
		eb, _ := json.Marshal(*exit)
		_ = c.Write(ctx, websocket.MessageText, eb)
	}

	api.auditor.Record(AuditEvent{
		SandboxID: sandbox,
		Op:        "pty",
		Detail:    fmt.Sprintf("cols=%d rows=%d", openReq.Cols, openReq.Rows),
		OK:        true,
	})
}

// atoiDefault parses s as a positive int, returning def on any parse failure or
// non-positive value. Used to bound cols/rows from the query string.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
		if n > 100000 { // absurd; clamp to default
			return def
		}
	}
	if n <= 0 {
		return def
	}
	return n
}
