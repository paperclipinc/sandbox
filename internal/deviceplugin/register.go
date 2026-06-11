package deviceplugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	v1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// socketName is the basename of the unix socket the plugin serves on, placed
// under the kubelet device-plugins directory. The kubelet registers the plugin
// by this endpoint basename (RegisterRequest.Endpoint).
const socketName = "agentrun-kvm.sock"

// retryInterval is how long Run waits before re-serving and re-registering
// after the serve loop returns (the kubelet restarted, or the socket vanished).
const retryInterval = 5 * time.Second

// Registrar serves a Plugin on a unix socket under the kubelet device-plugins
// directory and registers it with the kubelet. It re-registers on failure so a
// kubelet restart (which removes the plugin socket and drops the registration)
// is recovered automatically.
type Registrar struct {
	plugin *Plugin
	// kubeletDir is the device-plugins directory: the plugin socket is created
	// here and the kubelet registry socket (kubelet.sock) is dialed here.
	// Injectable so tests can point it at a t.TempDir with a fake kubelet.
	kubeletDir string
	logger     *slog.Logger
}

// NewRegistrar builds a Registrar. An empty kubeletDir defaults to the standard
// v1beta1 DevicePluginPath (/var/lib/kubelet/device-plugins). A nil logger
// defaults to a stderr text logger.
func NewRegistrar(plugin *Plugin, kubeletDir string, logger *slog.Logger) *Registrar {
	if kubeletDir == "" {
		kubeletDir = v1beta1.DevicePluginPath
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Registrar{plugin: plugin, kubeletDir: kubeletDir, logger: logger}
}

// socketPath is the absolute path of the plugin's serving socket.
func (r *Registrar) socketPath() string {
	return filepath.Join(r.kubeletDir, socketName)
}

// kubeletSocketPath is the absolute path of the kubelet registry socket.
func (r *Registrar) kubeletSocketPath() string {
	return filepath.Join(r.kubeletDir, "kubelet.sock")
}

// Run serves the plugin and registers it with the kubelet, retrying on
// failure, until ctx is cancelled. Each iteration: serve the plugin on its
// unix socket, register with the kubelet, then block until either the gRPC
// server returns or ctx is done; on a non-cancel return it waits retryInterval
// and retries (re-registering), so a kubelet restart is recovered.
func (r *Registrar) Run(ctx context.Context) error {
	for {
		if err := r.serveAndRegister(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.logger.Error("device plugin serve/register failed; retrying", "error", err, "retry_in", retryInterval)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

// serveAndRegister starts the gRPC server on the plugin socket, registers with
// the kubelet, and blocks until ctx is done or the server stops. It always
// stops the server and removes the socket before returning.
func (r *Registrar) serveAndRegister(ctx context.Context) error {
	socket := r.socketPath()
	// A stale socket from a previous run blocks bind; remove it first.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale plugin socket %s: %w", socket, err)
	}
	lis, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on plugin socket %s: %w", socket, err)
	}

	server := grpc.NewServer()
	v1beta1.RegisterDevicePluginServer(server, r.plugin)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(lis) }()

	// Register with the kubelet only after the server is listening (the socket
	// is bound above, so the kubelet can dial it as soon as we register).
	if err := r.register(ctx); err != nil {
		server.Stop()
		_ = os.Remove(socket)
		return fmt.Errorf("register with kubelet: %w", err)
	}
	r.logger.Info("device plugin registered with kubelet",
		"resource", r.plugin.ResourceName(), "endpoint", socketName)

	defer func() {
		server.Stop()
		_ = os.Remove(socket)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("plugin gRPC server stopped: %w", err)
		}
		return nil
	}
}

// register dials the kubelet registry socket and sends a RegisterRequest with
// the plugin's endpoint basename and resource name.
func (r *Registrar) register(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// The connection is lazy (grpc.NewClient does not block on dial); the
	// Register RPC below establishes it and surfaces any connect failure.
	conn, err := grpc.NewClient("unix:"+r.kubeletSocketPath(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial kubelet socket %s: %w", r.kubeletSocketPath(), err)
	}
	defer conn.Close()

	client := v1beta1.NewRegistrationClient(conn)
	_, err = client.Register(dialCtx, &v1beta1.RegisterRequest{
		Version:      v1beta1.Version,
		Endpoint:     socketName,
		ResourceName: r.plugin.ResourceName(),
	})
	if err != nil {
		return fmt.Errorf("kubelet Register RPC: %w", err)
	}
	return nil
}
