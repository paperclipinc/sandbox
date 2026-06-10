package firecracker

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// recordedReq captures one API call the fake Firecracker server received.
type recordedReq struct {
	method string
	path   string
	body   []byte
}

// fakeFCServer is a minimal HTTP server over a unix socket that records every
// request and answers 204. It stands in for the Firecracker API so the client
// methods can be unit tested without launching Firecracker.
type fakeFCServer struct {
	mu       sync.Mutex
	requests []recordedReq
}

func startFakeFCServer(t *testing.T) (*fakeFCServer, *Client) {
	t.Helper()
	// A short socket dir: unix socket paths are length-limited (~104 bytes on
	// darwin), and t.TempDir() can exceed that.
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeFCServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		srv.mu.Lock()
		srv.requests = append(srv.requests, recordedReq{method: r.Method, path: r.URL.Path, body: body})
		srv.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	httpSrv := &http.Server{Handler: mux}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	return srv, ConnectVM(sock)
}

func (s *fakeFCServer) find(t *testing.T, method, path string) recordedReq {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.method == method && r.path == path {
			return r
		}
	}
	t.Fatalf("no %s %s request recorded; got %+v", method, path, s.requests)
	return recordedReq{}
}

func TestSetNetwork(t *testing.T) {
	srv, c := startFakeFCServer(t)
	if err := c.SetNetwork("eth0", "02:11:22:33:44:55", "sbtap0"); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/network-interfaces/eth0")
	var got NetworkInterface
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := NetworkInterface{IfaceID: "eth0", GuestMAC: "02:11:22:33:44:55", HostDevName: "sbtap0"}
	if got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
}

func TestLoadSnapshotWithNetworkOverrides(t *testing.T) {
	srv, c := startFakeFCServer(t)
	overrides := []NetworkOverride{{IfaceID: "eth0", HostDevName: "sbtapfork1"}}
	if err := c.LoadSnapshotWithOverrides("/mem", "/vmstate", true, overrides); err != nil {
		t.Fatalf("LoadSnapshotWithOverrides: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/snapshot/load")
	var got SnapshotLoad
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.NetworkOverrides) != 1 || got.NetworkOverrides[0] != overrides[0] {
		t.Errorf("network_overrides = %+v, want %+v", got.NetworkOverrides, overrides)
	}
	if got.SnapshotPath != "/vmstate" || got.MemFilePath != "/mem" || !got.ResumeVM {
		t.Errorf("load fields wrong: %+v", got)
	}
}

func TestLoadSnapshotOmitsOverridesWhenNone(t *testing.T) {
	srv, c := startFakeFCServer(t)
	if err := c.LoadSnapshot("/mem", "/vmstate", true); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/snapshot/load")
	// network_overrides must be omitted entirely (omitempty) when none given,
	// so older snapshots without a NIC restore exactly as before.
	if got := string(req.body); contains(got, "network_overrides") {
		t.Errorf("expected no network_overrides key, got %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
