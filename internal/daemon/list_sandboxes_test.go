package daemon

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paperclipinc/sandbox/internal/fork"
)

// TestServerListSandboxesMergesActivity drives two forks through the Server,
// touches one via an exec call, and asserts ListSandboxes reports both with a
// non-zero created-at while only the touched sandbox carries a last-activity
// stamp.
func TestServerListSandboxesMergesActivity(t *testing.T) {
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}

	api := NewSandboxAPI(t.TempDir())
	// Deterministic clock for last-activity stamps.
	fixed := time.Unix(1_700_000_000, 0)
	api.now = func() time.Time { return fixed }

	srv := NewServer(engine, api)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	if _, err := srv.Fork(ctx, "py", "sb-touched", nil, nil, nil, "tok-touched"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Fork(ctx, "py", "sb-idle", nil, nil, nil, "tok-idle"); err != nil {
		t.Fatal(err)
	}

	// Before any exec, neither sandbox has activity.
	for _, info := range srv.ListSandboxes() {
		if info.LastActivityUnix != 0 {
			t.Fatalf("sandbox %s has last-activity %d before any call, want 0", info.SandboxId, info.LastActivityUnix)
		}
	}

	// Exec against sb-touched: auth passes (token registered), the mock has no
	// agent so the request 404s after touch. The touch is the point.
	resp, body := postExec(t, ts.URL, "sb-touched", "tok-touched")
	if resp.StatusCode != 404 {
		t.Fatalf("exec on sb-touched: status = %d, body = %s, want 404 (auth passed, no agent)", resp.StatusCode, body)
	}

	infos := srv.ListSandboxes()
	if len(infos) != 2 {
		t.Fatalf("ListSandboxes = %d, want 2", len(infos))
	}
	byID := map[string]struct {
		created  int64
		activity int64
	}{}
	for _, info := range infos {
		byID[info.SandboxId] = struct {
			created  int64
			activity int64
		}{info.CreatedAtUnix, info.LastActivityUnix}
	}

	touched, ok := byID["sb-touched"]
	if !ok {
		t.Fatal("ListSandboxes missing sb-touched")
	}
	if touched.created == 0 {
		t.Fatal("sb-touched has zero created-at")
	}
	if touched.activity != fixed.Unix() {
		t.Fatalf("sb-touched last-activity = %d, want %d", touched.activity, fixed.Unix())
	}

	idle, ok := byID["sb-idle"]
	if !ok {
		t.Fatal("ListSandboxes missing sb-idle")
	}
	if idle.created == 0 {
		t.Fatal("sb-idle has zero created-at")
	}
	if idle.activity != 0 {
		t.Fatalf("sb-idle last-activity = %d, want 0 (never accessed)", idle.activity)
	}

	// Last-activity is exposed directly too.
	if _, ok := api.LastActivity("sb-idle"); ok {
		t.Fatal("LastActivity(sb-idle) reports a stamp, want none")
	}
	if got, ok := api.LastActivity("sb-touched"); !ok || !got.Equal(fixed) {
		t.Fatalf("LastActivity(sb-touched) = %v, %v; want %v, true", got, ok, fixed)
	}

	// Unregister clears the activity record.
	srv.Terminate(ctx, "sb-touched")
	if _, ok := api.LastActivity("sb-touched"); ok {
		t.Fatal("LastActivity(sb-touched) still present after Terminate")
	}
}
