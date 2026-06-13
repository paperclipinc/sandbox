package daemon

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/fork"
	forkdpb "github.com/paperclipinc/mitos/proto/forkd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGRPCPullTemplateRecordsAndRegisters drives the PullTemplate RPC against
// the mock engine: the call is recorded (source URL + digest + token length,
// never the token value) and the template becomes present in the node's
// capacity so the controller sees it after distribution.
func TestGRPCPullTemplateRecordsAndRegisters(t *testing.T) {
	client, engine := newTestClient(t)
	ctx := context.Background()

	const (
		id     = "py"
		digest = "aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44"
		src    = "https://holder:9091/cas"
		token  = "peer-secret"
	)
	resp, err := client.PullTemplate(ctx, &forkdpb.PullTemplateRequest{
		TemplateId:     id,
		ManifestDigest: digest,
		SourceUrl:      src,
		PullToken:      token,
	})
	if err != nil {
		t.Fatalf("PullTemplate: %v", err)
	}
	if resp.TemplateId != id {
		t.Fatalf("response template id = %q, want %q", resp.TemplateId, id)
	}

	calls := engine.PullCalls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d pulls, want 1", len(calls))
	}
	got := calls[0]
	if got.SourceURL != src || got.ManifestDigest != digest || got.TemplateID != id {
		t.Fatalf("recorded pull = %+v, want src %q digest %q id %q", got, src, digest, id)
	}
	if got.TokenLen != len(token) {
		t.Fatalf("recorded token length %d, want %d", got.TokenLen, len(token))
	}

	cap := engine.GetCapacity()
	found := false
	for _, tid := range cap.TemplateIDs {
		if tid == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("template %q not present after pull; capacity = %v", id, cap.TemplateIDs)
	}
}

// TestGRPCPullTemplateRejectsMalformedID asserts the handler rejects a
// traversal-capable template id with InvalidArgument before the engine runs.
func TestGRPCPullTemplateRejectsMalformedID(t *testing.T) {
	client, engine := newTestClient(t)
	_, err := client.PullTemplate(context.Background(), &forkdpb.PullTemplateRequest{
		TemplateId:     "../escape",
		ManifestDigest: "aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44",
		SourceUrl:      "https://holder:9091/cas",
		PullToken:      "t",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v (err %v), want InvalidArgument", status.Code(err), err)
	}
	if len(engine.PullCalls()) != 0 {
		t.Fatal("a malformed template id reached the engine")
	}
}

// TestCASServingEnabledGate asserts CASServing.enabled requires all of store,
// token, TLS, AND a dedicated CAS listen address, so a token without TLS (or
// without a CAS listener) never mounts the surface.
func TestCASServingEnabledGate(t *testing.T) {
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	cases := []struct {
		name string
		cfg  *CASServing
		want bool
	}{
		{"nil", nil, false},
		{"token only", &CASServing{Token: "t"}, false},
		{"store+token no tls", &CASServing{Store: store, Token: "t"}, false},
		{"all but addr", &CASServing{Store: store, Token: "t", TLS: minimalTLS()}, false},
		{"all set", &CASServing{Store: store, Token: "t", TLS: minimalTLS(), Addr: ":9092"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.enabled(); got != tc.want {
				t.Fatalf("enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSandboxHTTPNotForcedToTLSWhenCASEnabled is the regression guard for the
// review finding: enabling CAS distribution must NOT force the sandbox HTTP API
// (exec/files/metrics/healthz) onto TLS, and must NOT mount /cas/ on that mux.
// SDK clients connect over http://, so the sandbox server's scheme must be
// unchanged. ServeHTTP serves the sandbox mux in plaintext and the CAS surface
// on a SEPARATE TLS listener (ServeCAS), so this test starts ServeHTTP with CAS
// enabled and asserts an http:// healthz works while /cas/ is absent.
func TestSandboxHTTPNotForcedToTLSWhenCASEnabled(t *testing.T) {
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	engine := fork.NewMockEngine()
	sandboxAPI := NewSandboxAPI(t.TempDir())

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen http: %v", err)
	}
	httpAddr := httpLn.Addr().String()
	_ = httpLn.Close()
	casLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen cas: %v", err)
	}
	casAddr := casLn.Addr().String()
	_ = casLn.Close()

	cfg := &CASServing{Store: store, Token: "peer-secret", TLS: minimalTLS(), Addr: casAddr}
	if !cfg.enabled() {
		t.Fatal("CASServing not enabled for the test setup")
	}
	go ServeHTTP(httpAddr, engine, sandboxAPI, cfg)

	// Wait for the sandbox HTTP listener to come up, then prove it answers over
	// PLAINTEXT http:// (not TLS): an https:// or TLS-forced server would fail
	// this plaintext GET.
	base := "http://" + httpAddr
	var resp *http.Response
	for attempt := 0; attempt < 50; attempt++ {
		resp, err = http.Get(base + "/healthz") //nolint:noctx,bodyclose // test
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("plaintext GET /healthz failed (sandbox API was forced to TLS?): %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status %d, want 200", resp.StatusCode)
	}

	// The /cas/ surface must NOT be mounted on the sandbox mux: a request for it
	// here must not be the token-gated CAS handler. The sandbox mux has no /cas/
	// route, so it 404s rather than 403s (the CAS handler's no-token response).
	casResp, err := http.Get(base + "/cas/manifest/deadbeef") //nolint:noctx,bodyclose // test
	if err != nil {
		t.Fatalf("GET /cas on sandbox mux: %v", err)
	}
	_ = casResp.Body.Close()
	if casResp.StatusCode == http.StatusForbidden {
		t.Fatal("the token-gated CAS handler is mounted on the sandbox mux (403); it must live only on the separate CAS listener")
	}
	if casResp.StatusCode != http.StatusNotFound {
		t.Fatalf("/cas on sandbox mux: status %d, want 404 (no route)", casResp.StatusCode)
	}
}

// TestServeCASSurfaceTokenGated mounts the gated CAS handler exactly as
// ServeHTTP does and proves the token gate: a request without the token is 403,
// with it the manifest is served. This is the daemon-side wiring of the surface
// a peer pulls from.
func TestServeCASSurfaceTokenGated(t *testing.T) {
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	mem := writeTempFile(t, "hello world")
	m, err := store.PutSnapshot(map[string]string{"mem": mem}, cas.Metadata{VMMVersion: "v1"})
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	const token = "peer-secret"
	mux := http.NewServeMux()
	mux.Handle("/cas/", cas.RequirePullToken(token, cas.NewHTTPHandler(store)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No token -> 403.
	resp, err := http.Get(srv.URL + "/cas/manifest/" + string(m.Digest())) //nolint:noctx,bodyclose // test
	if err != nil {
		t.Fatalf("GET without token: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("without token: status %d, want 403", resp.StatusCode)
	}

	// Right token -> a peer transport pulls into its own store.
	peer, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("peer cas.New: %v", err)
	}
	transport := cas.NewHTTPTransport(srv.URL, srv.Client()).WithBearerToken(token)
	if err := cas.Pull(context.Background(), peer, transport, m.Digest()); err != nil {
		t.Fatalf("Pull with token: %v", err)
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

// minimalTLS returns a non-nil tls.Config for the enabled() gate test. It need
// not be loadable: enabled() only checks for a non-nil config.
func minimalTLS() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13}
}
