//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/paperclipinc/mitos/internal/guestenv"
	"github.com/paperclipinc/mitos/internal/vsock"
	"golang.org/x/sys/unix"
)

// Guest agent: runs as init (PID 1) inside the Firecracker VM.
// Sets up the filesystem, then listens on vsock for commands.

var startTime = time.Now()

// configuredEnv holds claim-time env+secrets delivered via the configure
// message. Values are never logged. Guarded by configuredMu.
var (
	configuredMu  sync.Mutex
	configuredEnv = map[string]string{}
)

func main() {
	if os.Getpid() == 1 {
		initSystem()
	}

	fmt.Println("sandbox-agent: starting on vsock port", vsock.AgentPort)

	listener, err := listenVsock(vsock.AgentPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: listen error: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()

	fmt.Println("sandbox-agent: ready")

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-agent: accept error: %v\n", err)
			continue
		}
		go handleConnection(conn)
	}
}

func initSystem() {
	// Mount essential filesystems
	mounts := []struct {
		source string
		target string
		fstype string
		flags  uintptr
	}{
		{"proc", "/proc", "proc", 0},
		{"sysfs", "/sys", "sysfs", 0},
		{"devtmpfs", "/dev", "devtmpfs", 0},
		{"tmpfs", "/tmp", "tmpfs", 0},
		{"tmpfs", "/run", "tmpfs", 0},
	}

	for _, m := range mounts {
		// PID-1 init: setup failures are non-fatal by design; log and continue.
		if err := os.MkdirAll(m.target, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", m.target, err)
		}
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, ""); err != nil {
			fmt.Fprintf(os.Stderr, "mount %s: %v\n", m.target, err)
		}
	}

	// Create workspace directory (non-fatal on failure, like mounts above)
	if err := os.MkdirAll("/workspace", 0755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir /workspace: %v\n", err)
	}

	// Set hostname (non-fatal on failure)
	if err := syscall.Sethostname([]byte("sandbox")); err != nil {
		fmt.Fprintf(os.Stderr, "sethostname: %v\n", err)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	// The line buffer must hold the largest framed message; for the bulk tar ops
	// that is the base64 JSON of up to vsock.MaxTarBytes. vsock.MaxMessageBytes
	// is the matching cap on the host client.
	scanner.Buffer(make([]byte, 1024*1024), vsock.MaxMessageBytes)

	for scanner.Scan() {
		var req vsock.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			writeResponse(conn, vsock.Response{OK: false, Error: fmt.Sprintf("invalid request: %v", err)})
			continue
		}

		resp := handleRequest(&req)
		writeResponse(conn, resp)
	}
}

func handleRequest(req *vsock.Request) vsock.Response {
	switch req.Type {
	case vsock.TypePing:
		return vsock.Response{
			OK:   true,
			Ping: &vsock.PingResponse{Uptime: time.Since(startTime).Seconds()},
		}

	case vsock.TypeExec:
		if req.Exec == nil {
			return vsock.Response{OK: false, Error: "exec request is nil"}
		}
		return handleExec(req.Exec)

	case vsock.TypeReadFile:
		if req.ReadFile == nil {
			return vsock.Response{OK: false, Error: "read_file request is nil"}
		}
		return handleReadFile(req.ReadFile)

	case vsock.TypeWriteFile:
		if req.WriteFile == nil {
			return vsock.Response{OK: false, Error: "write_file request is nil"}
		}
		return handleWriteFile(req.WriteFile)

	case vsock.TypeListDir:
		if req.ListDir == nil {
			return vsock.Response{OK: false, Error: "list_dir request is nil"}
		}
		return handleListDir(req.ListDir)

	case vsock.TypeMkdir:
		if req.Mkdir == nil {
			return vsock.Response{OK: false, Error: "mkdir request is nil"}
		}
		if err := os.MkdirAll(req.Mkdir.Path, 0755); err != nil {
			return vsock.Response{OK: false, Error: err.Error()}
		}
		return vsock.Response{OK: true}

	case vsock.TypeRemove:
		if req.Remove == nil {
			return vsock.Response{OK: false, Error: "remove request is nil"}
		}
		if err := os.RemoveAll(req.Remove.Path); err != nil {
			return vsock.Response{OK: false, Error: err.Error()}
		}
		return vsock.Response{OK: true}

	case vsock.TypeConfigure:
		if req.Configure == nil {
			return vsock.Response{OK: false, Error: "configure request is nil"}
		}
		return handleConfigure(req.Configure)

	case vsock.TypeNotifyForked:
		if req.NotifyForked == nil {
			return vsock.Response{OK: false, Error: "notify_forked request is nil"}
		}
		return handleNotifyForked(req.NotifyForked)

	case vsock.TypeTarDir:
		if req.TarDir == nil {
			return vsock.Response{OK: false, Error: "tar_dir request is nil"}
		}
		return handleTarDir(req.TarDir)

	case vsock.TypeUntarDir:
		if req.UntarDir == nil {
			return vsock.Response{OK: false, Error: "untar_dir request is nil"}
		}
		return handleUntarDir(req.UntarDir)

	default:
		return vsock.Response{OK: false, Error: fmt.Sprintf("unknown request type: %s", req.Type)}
	}
}

func handleConfigure(req *vsock.ConfigureRequest) vsock.Response {
	// The merge is additive: retrying configure with a different key set does
	// not remove previously delivered keys. The forkd delivery path sends
	// configure exactly once per fork, so this only matters for manual retries.
	configuredMu.Lock()
	for k, v := range req.Env {
		configuredEnv[k] = v
	}
	for k, v := range req.Secrets {
		configuredEnv[k] = v
	}
	n := len(configuredEnv)
	configuredMu.Unlock()

	fmt.Printf("sandbox-agent: configured %d environment variables\n", n)
	return vsock.Response{OK: true}
}

func handleExec(req *vsock.ExecRequest) vsock.Response {
	start := time.Now()

	timeout := time.Duration(req.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", req.Command)
	cmd.Dir = req.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = "/workspace"
	}

	configuredMu.Lock()
	configured := make(map[string]string, len(configuredEnv))
	for k, v := range configuredEnv {
		configured[k] = v
	}
	configuredMu.Unlock()
	cmd.Env = guestenv.Merge(os.Environ(), configured, req.Env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	elapsed := time.Since(start)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			exitCode = 124
		} else {
			exitCode = 1
		}
	}

	return vsock.Response{
		OK: true,
		Exec: &vsock.ExecResponse{
			ExitCode:   exitCode,
			Stdout:     stdout.String(),
			Stderr:     stderr.String(),
			ExecTimeMs: float64(elapsed.Microseconds()) / 1000.0,
		},
	}
}

func handleReadFile(req *vsock.ReadFileRequest) vsock.Response {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return vsock.Response{OK: false, Error: err.Error()}
	}

	info, _ := os.Stat(req.Path)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	return vsock.Response{
		OK: true,
		ReadFile: &vsock.ReadFileResponse{
			Content: data,
			Size:    size,
		},
	}
}

func handleWriteFile(req *vsock.WriteFileRequest) vsock.Response {
	mode := fs.FileMode(req.Mode)
	if mode == 0 {
		mode = 0644
	}

	dir := filepath.Dir(req.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return vsock.Response{OK: false, Error: err.Error()}
	}

	if err := os.WriteFile(req.Path, req.Content, mode); err != nil {
		return vsock.Response{OK: false, Error: err.Error()}
	}
	return vsock.Response{OK: true}
}

func handleListDir(req *vsock.ListDirRequest) vsock.Response {
	entries, err := os.ReadDir(req.Path)
	if err != nil {
		return vsock.Response{OK: false, Error: err.Error()}
	}

	var fileEntries []vsock.FileEntry
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		fileEntries = append(fileEntries, vsock.FileEntry{
			Name:       e.Name(),
			IsDir:      e.IsDir(),
			Size:       info.Size(),
			Mode:       uint32(info.Mode()),
			ModifiedAt: info.ModTime().Unix(),
		})
	}

	return vsock.Response{
		OK:      true,
		ListDir: &vsock.ListDirResponse{Entries: fileEntries},
	}
}

func writeResponse(conn net.Conn, resp vsock.Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: write response: %v\n", err)
	}
}

// vsockListener implements net.Listener using raw AF_VSOCK syscalls.
// Go's net package doesn't understand AF_VSOCK, so we handle accept at
// the syscall level and wrap each connection as a net.Conn via os.File.
type vsockListener struct {
	fd int
}

func (l *vsockListener) Accept() (net.Conn, error) {
	nfd, _, err := unix.Accept(l.fd)
	if err != nil {
		return nil, err
	}
	return &vsockConn{fd: nfd}, nil
}

// vsockConn wraps a raw vsock fd as net.Conn.
// Go's net.FileConn doesn't understand AF_VSOCK, so we implement
// Read/Write/Close directly on the fd.
type vsockConn struct {
	fd int
}

func (c *vsockConn) Read(b []byte) (int, error) {
	n, err := unix.Read(c.fd, b)
	if n == 0 && err == nil {
		return 0, io.EOF
	}
	return n, err
}

func (c *vsockConn) Write(b []byte) (int, error) {
	return unix.Write(c.fd, b)
}

func (c *vsockConn) Close() error {
	return unix.Close(c.fd)
}

func (c *vsockConn) LocalAddr() net.Addr                { return nil }
func (c *vsockConn) RemoteAddr() net.Addr               { return nil }
func (c *vsockConn) SetDeadline(t time.Time) error      { return nil }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return nil }

func (l *vsockListener) Close() error {
	return unix.Close(l.fd)
}

func (l *vsockListener) Addr() net.Addr {
	return nil
}

func listenVsock(port int) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err == nil {
		addr := &unix.SockaddrVM{
			CID:  0xFFFFFFFF, // VMADDR_CID_ANY
			Port: uint32(port),
		}
		if err := unix.Bind(fd, addr); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("vsock bind: %w", err)
		}
		if err := unix.Listen(fd, 128); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("vsock listen: %w", err)
		}
		fmt.Printf("sandbox-agent: listening on vsock CID=any port=%d\n", port)
		return &vsockListener{fd: fd}, nil
	}

	// Fallback: Unix socket (for local testing outside a VM)
	sockPath := fmt.Sprintf("/tmp/sandbox-agent-%d.sock", port)
	os.Remove(sockPath)
	fmt.Printf("sandbox-agent: vsock not available (%v), falling back to unix socket %s\n", err, sockPath)
	return net.Listen("unix", sockPath)
}

func init() {
	if _, err := os.Stat("/bin/sh"); err != nil {
		paths := []string{"/bin/busybox", "/usr/bin/sh", "/usr/bin/bash"}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				// Best-effort shell discovery; exec falls back to the error path if this fails.
				_ = os.Symlink(p, "/bin/sh")
				break
			}
		}
	}
	_ = strings.Join(nil, "")
}
