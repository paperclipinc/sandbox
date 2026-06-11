package deviceplugin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	v1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const testResource = "agentrun.dev/kvm"

// startPluginServer serves the given Plugin over an in-memory bufconn and
// returns a DevicePlugin client plus a cleanup func.
func startPluginServer(t *testing.T, p *Plugin) (v1beta1.DevicePluginClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1beta1.RegisterDevicePluginServer(srv, p)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		srv.Stop()
	}
	return v1beta1.NewDevicePluginClient(conn), cleanup
}

// firstListAndWatch reads the first ListAndWatchResponse from the stream.
func firstListAndWatch(t *testing.T, client v1beta1.DevicePluginClient) *v1beta1.ListAndWatchResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.ListAndWatch(ctx, &v1beta1.Empty{})
	if err != nil {
		t.Fatalf("ListAndWatch: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("ListAndWatch Recv: %v", err)
	}
	return resp
}

func TestListAndWatchAdvertisesDevicesWhenKVMPresent(t *testing.T) {
	p := NewPlugin(testResource, 3, []string{"/dev/kvm"}, func() bool { return true })
	client, cleanup := startPluginServer(t, p)
	defer cleanup()

	resp := firstListAndWatch(t, client)
	if len(resp.GetDevices()) != 3 {
		t.Fatalf("expected 3 devices when KVM present, got %d", len(resp.GetDevices()))
	}
	for _, d := range resp.GetDevices() {
		if d.GetHealth() != v1beta1.Healthy {
			t.Errorf("device %q health = %q, want %q", d.GetID(), d.GetHealth(), v1beta1.Healthy)
		}
	}
}

func TestListAndWatchAdvertisesZeroWhenKVMAbsent(t *testing.T) {
	p := NewPlugin(testResource, 100, []string{"/dev/kvm"}, func() bool { return false })
	client, cleanup := startPluginServer(t, p)
	defer cleanup()

	resp := firstListAndWatch(t, client)
	if len(resp.GetDevices()) != 0 {
		t.Fatalf("expected 0 devices when KVM absent, got %d", len(resp.GetDevices()))
	}
}

func TestAllocateReturnsDeviceSpecs(t *testing.T) {
	p := NewPlugin(testResource, 10, []string{"/dev/kvm", "/dev/net/tun"}, func() bool { return true })
	client, cleanup := startPluginServer(t, p)
	defer cleanup()

	resp, err := client.Allocate(context.Background(), &v1beta1.AllocateRequest{
		ContainerRequests: []*v1beta1.ContainerAllocateRequest{
			{DevicesIDs: []string{"kvm-0"}},
		},
	})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(resp.GetContainerResponses()) != 1 {
		t.Fatalf("expected 1 container response, got %d", len(resp.GetContainerResponses()))
	}
	specs := resp.GetContainerResponses()[0].GetDevices()
	if len(specs) != 2 {
		t.Fatalf("expected 2 device specs, got %d", len(specs))
	}
	want := map[string]bool{"/dev/kvm": false, "/dev/net/tun": false}
	for _, s := range specs {
		if s.GetHostPath() != s.GetContainerPath() {
			t.Errorf("host path %q != container path %q", s.GetHostPath(), s.GetContainerPath())
		}
		if s.GetPermissions() != "rw" {
			t.Errorf("device %q permissions = %q, want rw", s.GetHostPath(), s.GetPermissions())
		}
		if _, ok := want[s.GetHostPath()]; !ok {
			t.Errorf("unexpected device path %q", s.GetHostPath())
			continue
		}
		want[s.GetHostPath()] = true
	}
	for path, seen := range want {
		if !seen {
			t.Errorf("device path %q missing from Allocate response", path)
		}
	}
}

func TestAllocatePerContainerRequest(t *testing.T) {
	p := NewPlugin(testResource, 10, []string{"/dev/kvm"}, func() bool { return true })
	client, cleanup := startPluginServer(t, p)
	defer cleanup()

	resp, err := client.Allocate(context.Background(), &v1beta1.AllocateRequest{
		ContainerRequests: []*v1beta1.ContainerAllocateRequest{
			{DevicesIDs: []string{"kvm-0"}},
			{DevicesIDs: []string{"kvm-1"}},
		},
	})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(resp.GetContainerResponses()) != 2 {
		t.Fatalf("expected 2 container responses, got %d", len(resp.GetContainerResponses()))
	}
}

func TestGetDevicePluginOptions(t *testing.T) {
	p := NewPlugin(testResource, 1, []string{"/dev/kvm"}, func() bool { return true })
	client, cleanup := startPluginServer(t, p)
	defer cleanup()

	opts, err := client.GetDevicePluginOptions(context.Background(), &v1beta1.Empty{})
	if err != nil {
		t.Fatalf("GetDevicePluginOptions: %v", err)
	}
	if opts.GetPreStartRequired() {
		t.Error("PreStartRequired should be false")
	}
	if opts.GetGetPreferredAllocationAvailable() {
		t.Error("GetPreferredAllocationAvailable should be false")
	}
}

// fakeKubelet is a minimal Registration gRPC server that captures the
// RegisterRequest the plugin sends.
type fakeKubelet struct {
	v1beta1.UnimplementedRegistrationServer
	mu      sync.Mutex
	last    *v1beta1.RegisterRequest
	gotCall chan struct{}
}

func (f *fakeKubelet) Register(_ context.Context, req *v1beta1.RegisterRequest) (*v1beta1.Empty, error) {
	f.mu.Lock()
	f.last = req
	f.mu.Unlock()
	close(f.gotCall)
	return &v1beta1.Empty{}, nil
}

func TestRegistrarRegistersWithKubelet(t *testing.T) {
	// Use a short base dir: unix socket paths are limited (~104 bytes on
	// darwin), and the default t.TempDir() path is long enough to overflow it.
	dir, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(dir)
	kubeletSock := filepath.Join(dir, "kubelet.sock")

	lis, err := net.Listen("unix", kubeletSock)
	if err != nil {
		t.Fatalf("listen fake kubelet socket: %v", err)
	}
	defer func() { _ = lis.Close() }()
	fake := &fakeKubelet{gotCall: make(chan struct{})}
	srv := grpc.NewServer()
	v1beta1.RegisterRegistrationServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	plugin := NewPlugin(testResource, 5, []string{"/dev/kvm"}, func() bool { return true })
	registrar := NewRegistrar(plugin, dir, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- registrar.Run(ctx) }()

	select {
	case <-fake.gotCall:
	case <-time.After(10 * time.Second):
		t.Fatal("kubelet Register was never called")
	}

	fake.mu.Lock()
	req := fake.last
	fake.mu.Unlock()
	if req.GetVersion() != v1beta1.Version {
		t.Errorf("Register Version = %q, want %q", req.GetVersion(), v1beta1.Version)
	}
	if req.GetEndpoint() != socketName {
		t.Errorf("Register Endpoint = %q, want %q", req.GetEndpoint(), socketName)
	}
	if req.GetResourceName() != testResource {
		t.Errorf("Register ResourceName = %q, want %q", req.GetResourceName(), testResource)
	}

	// The plugin socket should exist in the kubelet dir after registration.
	probe, err := net.Dial("unix", filepath.Join(dir, socketName))
	if err != nil {
		t.Errorf("plugin socket not servable: %v", err)
	} else {
		_ = probe.Close()
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
