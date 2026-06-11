package vsock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Client communicates with the guest agent over vsock (or Unix socket for testing).
type Client struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

func newClient(conn net.Conn) *Client {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)
	return &Client{conn: conn, scanner: scanner}
}

// Connect to a guest agent via the Firecracker vsock UDS path.
// Firecracker exposes vsock as a Unix socket on the host.
func Connect(udsPath string, guestPort int) (*Client, error) {
	conn, err := net.DialTimeout("unix", udsPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to vsock UDS: %w", err)
	}

	// Firecracker vsock UDS protocol: send "CONNECT <port>\n", expect "OK <port>\n"
	connectCmd := fmt.Sprintf("CONNECT %d\n", guestPort)
	if _, err := conn.Write([]byte(connectCmd)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)
	if scanner.Scan() {
		resp := scanner.Text()
		if len(resp) < 2 || resp[:2] != "OK" {
			conn.Close()
			return nil, fmt.Errorf("vsock CONNECT rejected: %s", resp)
		}
	} else {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: no response")
	}

	return &Client{conn: conn, scanner: scanner}, nil
}

// ConnectUnix connects via Unix socket (for local testing without KVM).
func ConnectUnix(sockPath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect unix: %w", err)
	}
	return newClient(conn), nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) send(req *Request) (*Response, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if _, err := c.conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("recv: %w", err)
		}
		return nil, fmt.Errorf("connection closed")
	}

	var resp Response
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if !resp.OK {
		return &resp, fmt.Errorf("agent error: %s", resp.Error)
	}
	return &resp, nil
}

func (c *Client) Ping() (float64, error) {
	resp, err := c.send(&Request{Type: TypePing})
	if err != nil {
		return 0, err
	}
	return resp.Ping.Uptime, nil
}

func (c *Client) Exec(command string, workingDir string, env map[string]string, timeout int) (*ExecResponse, error) {
	resp, err := c.send(&Request{
		Type: TypeExec,
		Exec: &ExecRequest{
			Command:    command,
			WorkingDir: workingDir,
			Env:        env,
			Timeout:    timeout,
		},
	})
	if err != nil {
		return nil, err
	}
	return resp.Exec, nil
}

// Configure delivers claim-time env and secrets to the guest agent.
func (c *Client) Configure(env, secrets map[string]string) error {
	_, err := c.send(&Request{
		Type:      TypeConfigure,
		Configure: &ConfigureRequest{Env: env, Secrets: secrets},
	})
	return err
}

// NotifyForked tells the guest agent a restore just happened so it can reseed
// the kernel CRNG, step the wall clock, and signal userspace runtimes.
// HostWallClockNanos is stamped at send time so the guest measures drift
// against the moment of delivery. Entropy is sensitive seed material and is
// never logged.
func (c *Client) NotifyForked(generation uint64, entropy []byte) (*NotifyForkedResponse, error) {
	return c.NotifyForkedWithNetwork(generation, entropy, nil)
}

// NotifyForkedWithNetwork is NotifyForked plus an optional per-fork network
// config the guest applies to eth0 (distinct guest IP + gateway). It is used
// when host-side networking is enabled so each fork, which restores the same
// snapshot-baked guest IP, is re-addressed to its allocator-assigned /30.
// Passing nil network is identical to NotifyForked. The IPs are safe to log.
func (c *Client) NotifyForkedWithNetwork(generation uint64, entropy []byte, network *NotifyForkedNetwork) (*NotifyForkedResponse, error) {
	return c.NotifyForkedWithConfig(generation, entropy, network, nil)
}

// NotifyForkedWithConfig is NotifyForkedWithNetwork plus the per-fork volume
// mount table the guest mounts after the restore. The host must have already
// rebound each baked placeholder drive to this fork's backing (PATCH /drives)
// before this call, so the devices are in place when the guest mounts them.
// Passing nil volumes is identical to NotifyForkedWithNetwork. Device nodes and
// paths are safe to log.
func (c *Client) NotifyForkedWithConfig(generation uint64, entropy []byte, network *NotifyForkedNetwork, volumes []VolumeMountEntry) (*NotifyForkedResponse, error) {
	resp, err := c.send(&Request{
		Type: TypeNotifyForked,
		NotifyForked: &NotifyForkedRequest{
			Generation:         generation,
			HostWallClockNanos: time.Now().UnixNano(),
			Entropy:            entropy,
			Network:            network,
			Volumes:            volumes,
		},
	})
	if err != nil {
		return nil, err
	}
	return resp.NotifyForked, nil
}

func (c *Client) ReadFile(path string) ([]byte, error) {
	resp, err := c.send(&Request{
		Type:     TypeReadFile,
		ReadFile: &ReadFileRequest{Path: path},
	})
	if err != nil {
		return nil, err
	}
	return resp.ReadFile.Content, nil
}

func (c *Client) WriteFile(path string, content []byte, mode uint32) error {
	_, err := c.send(&Request{
		Type:      TypeWriteFile,
		WriteFile: &WriteFileRequest{Path: path, Content: content, Mode: mode},
	})
	return err
}

func (c *Client) ListDir(path string) ([]FileEntry, error) {
	resp, err := c.send(&Request{
		Type:    TypeListDir,
		ListDir: &ListDirRequest{Path: path},
	})
	if err != nil {
		return nil, err
	}
	return resp.ListDir.Entries, nil
}

func (c *Client) Mkdir(path string) error {
	_, err := c.send(&Request{Type: TypeMkdir, Mkdir: &MkdirRequest{Path: path}})
	return err
}

func (c *Client) Remove(path string) error {
	_, err := c.send(&Request{Type: TypeRemove, Remove: &RemoveRequest{Path: path}})
	return err
}
