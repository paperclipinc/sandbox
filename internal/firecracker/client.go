package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Client talks to a single Firecracker process via its Unix socket API.
type Client struct {
	socketPath string
	http       *http.Client
	process    *os.Process
}

// StartVM launches a Firecracker process and returns a client connected to it.
func StartVM(cfg VMConfig) (*Client, error) {
	socketPath := cfg.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(cfg.WorkDir, "firecracker.sock")
	}

	os.Remove(socketPath)

	args := []string{
		"--api-sock", socketPath,
	}
	if cfg.ID != "" {
		args = append(args, "--id", cfg.ID)
	}

	cmd := exec.Command(cfg.FirecrackerBin, args...)
	cmd.Dir = cfg.WorkDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	client := &Client{
		socketPath: socketPath,
		process:    cmd.Process,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}

	if err := client.waitReady(5 * time.Second); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("firecracker not ready: %w", err)
	}

	return client, nil
}

// ConnectVM connects to an already-running Firecracker instance.
func ConnectVM(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(c.socketPath); err == nil {
			if _, err := c.get("/"); err == nil {
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for socket %s", c.socketPath)
}

// --- VM Configuration ---

func (c *Client) SetBootSource(kernel string, bootArgs string) error {
	return c.put("/boot-source", BootSource{
		KernelImagePath: kernel,
		BootArgs:        bootArgs,
	})
}

func (c *Client) SetMachineConfig(vcpus int, memMB int) error {
	return c.put("/machine-config", MachineConfig{
		VcpuCount:  vcpus,
		MemSizeMib: memMB,
	})
}

func (c *Client) AddDrive(driveID string, path string, readOnly bool, rootDevice bool) error {
	return c.put("/drives/"+driveID, Drive{
		DriveID:      driveID,
		PathOnHost:   path,
		IsReadOnly:   readOnly,
		IsRootDevice: rootDevice,
	})
}

func (c *Client) SetVsock(guestCID int, udsPath string) error {
	return c.put("/vsock", Vsock{
		GuestCID: guestCID,
		UdsPath:  udsPath,
	})
}

// --- VM Lifecycle ---

func (c *Client) Start() error {
	return c.put("/actions", Action{ActionType: "InstanceStart"})
}

func (c *Client) Pause() error {
	return c.patch("/vm", VMState{State: "Paused"})
}

func (c *Client) Resume() error {
	return c.patch("/vm", VMState{State: "Resumed"})
}

// --- Snapshot Operations ---

func (c *Client) CreateSnapshot(memPath, snapshotPath string) error {
	return c.put("/snapshot/create", SnapshotCreate{
		SnapshotType: "Full",
		SnapshotPath: snapshotPath,
		MemFilePath:  memPath,
	})
}

func (c *Client) LoadSnapshot(memPath, snapshotPath string, resumeVM bool) error {
	return c.put("/snapshot/load", SnapshotLoad{
		SnapshotPath:        snapshotPath,
		MemFilePath:         memPath,
		EnableDiffSnapshots: false,
		ResumeVM:            resumeVM,
	})
}

// --- Process Management ---

func (c *Client) Kill() error {
	if c.process != nil {
		return c.process.Kill()
	}
	return nil
}

func (c *Client) PID() int {
	if c.process != nil {
		return c.process.Pid
	}
	return 0
}

// --- HTTP helpers ---

func (c *Client) put(path string, body interface{}) error {
	return c.do(http.MethodPut, path, body)
}

func (c *Client) patch(path string, body interface{}) error {
	return c.do(http.MethodPatch, path, body)
}

func (c *Client) get(path string) ([]byte, error) {
	resp, err := c.http.Get("http://localhost" + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, string(data))
	}
	return data, nil
}

func (c *Client) do(method, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	return nil
}
