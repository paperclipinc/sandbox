package vsock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Client communicates with the guest agent over vsock (or Unix socket for testing).
type Client struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

func newClient(conn net.Conn) *Client {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
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
	scanner.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
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

// TarDir asks the guest agent to tar the directory at path and returns the tar
// bytes. The guest restricts path to the workspace-transfer allowlist and bounds
// the tar to MaxTarBytes. This is the host half of the bulk workspace dehydrate.
func (c *Client) TarDir(path string) ([]byte, error) {
	resp, err := c.send(&Request{
		Type:   TypeTarDir,
		TarDir: &TarDirRequest{Path: path},
	})
	if err != nil {
		return nil, err
	}
	if resp.TarDir == nil {
		return nil, fmt.Errorf("tar_dir: empty response")
	}
	return resp.TarDir.Tar, nil
}

// UntarDir asks the guest agent to extract tar into the directory at path. The
// caller must keep tar within MaxTarBytes (this is the size the guest accepts and
// the line buffer holds). The guest sanitizes every member against traversal.
// This is the host half of the bulk workspace hydrate.
func (c *Client) UntarDir(path string, tar []byte) error {
	if len(tar) > MaxTarBytes {
		return fmt.Errorf("untar_dir: tar size %d exceeds max %d", len(tar), MaxTarBytes)
	}
	_, err := c.send(&Request{
		Type:     TypeUntarDir,
		UntarDir: &UntarDirRequest{Path: path, Tar: tar},
	})
	return err
}

// StreamConn is a DEDICATED vsock connection for one streaming exec. It is kept
// separate from Client.conn so a long-running stream never interleaves with the
// shared connection's one-shot Response calls (Ping, file ops, aggregated Exec).
type StreamConn struct {
	conn    net.Conn
	scanner *bufio.Scanner
	// writeMu guards all host->guest writes on a bidirectional PTY stream so
	// concurrent input/resize frames never interleave mid-line on the wire.
	writeMu sync.Mutex
	// ptyReady is closed by Pty once the open request is on the wire. SendInput
	// and Resize block on it so an input/resize frame can never reach the guest
	// before the request line that the guest's first read consumes; without this
	// a caller that fires Resize concurrently with Pty could have the guest mistake
	// the resize frame for the request line. Created lazily under initOnce.
	initOnce  sync.Once
	readyOnce sync.Once
	ptyReady  chan struct{}
}

// ptyReadyCh lazily creates and returns the ptyReady channel so both Pty and the
// frame writers observe the same instance.
func (s *StreamConn) ptyReadyCh() chan struct{} {
	s.initOnce.Do(func() {
		s.ptyReady = make(chan struct{})
	})
	return s.ptyReady
}

// signalPtyReady marks the open request as written so blocked input/resize
// writers may proceed. Idempotent.
func (s *StreamConn) signalPtyReady() {
	ch := s.ptyReadyCh()
	s.readyOnce.Do(func() { close(ch) })
}

// DialStream opens a fresh vsock connection to the guest agent for one
// streaming exec, performing the Firecracker UDS CONNECT preamble. The caller
// must Close it when the stream ends or its HTTP client disconnects.
func DialStream(udsPath string, guestPort int) (*StreamConn, error) {
	conn, err := net.DialTimeout("unix", udsPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial stream vsock UDS: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", guestPort))); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
	if !sc.Scan() {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: no response")
	}
	if resp := sc.Text(); len(resp) < 2 || resp[:2] != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %s", resp)
	}
	return &StreamConn{conn: conn, scanner: sc}, nil
}

// DialStreamUnix dials a plain unix socket that already speaks the CONNECT
// preamble (the standalone server's unix fallback and tests).
func DialStreamUnix(sockPath string) (*StreamConn, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial stream unix: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", AgentPort))); err != nil {
		conn.Close()
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
	if !sc.Scan() {
		conn.Close()
		return nil, fmt.Errorf("stream unix: no preamble response")
	}
	return &StreamConn{conn: conn, scanner: sc}, nil
}

// Close shuts the dedicated stream connection. Closing it while the guest is
// still running cancels the guest exec (the guest sees the connection drop).
func (s *StreamConn) Close() error {
	return s.conn.Close()
}

// ChunkFunc receives one stream's bytes as they arrive. Returning a non-nil
// error stops the stream early (the caller should then Close the StreamConn).
type ChunkFunc func(stream StreamName, data []byte) error

// ExecStream runs command on the guest and invokes onChunk for each stdout or
// stderr chunk as it arrives, returning the terminal ExecStreamFrame (exit
// code, exec time, and any spawn error). The request is sent once; frames are
// read until the FrameExit line. If ctx is cancelled the connection is closed,
// which the guest observes and uses to kill the process group.
func (s *StreamConn) ExecStream(ctx context.Context, req *ExecRequest, onChunk ChunkFunc) (*ExecStreamFrame, error) {
	data, err := json.Marshal(&Request{Type: TypeExecStream, ExecStream: req})
	if err != nil {
		return nil, err
	}
	if _, err := s.conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("send exec_stream: %w", err)
	}

	// Closing the connection on ctx cancel unblocks the scanner below.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.conn.Close()
		case <-done:
		}
	}()

	for s.scanner.Scan() {
		var f ExecStreamFrame
		if err := json.Unmarshal(s.scanner.Bytes(), &f); err != nil {
			return nil, fmt.Errorf("decode exec_stream frame: %w", err)
		}
		switch f.Kind {
		case FrameChunk:
			if err := onChunk(f.Stream, f.Data); err != nil {
				return nil, err
			}
		case FrameExit:
			return &f, nil
		default:
			return nil, fmt.Errorf("unknown exec_stream frame kind: %q", f.Kind)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, fmt.Errorf("recv exec_stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("exec_stream: connection closed before exit frame")
}

// RunCode runs a code snippet in the guest's stateful kernel over this
// dedicated stream and invokes onFrame for each ExecStreamFrame the guest emits
// (chunk frames for stdout/stderr, result frames for rich artifacts, an error
// frame for a structured exception, and a terminal exit). The kernel is started
// lazily by the guest on the first call and persists for the sandbox lifetime,
// so state set by an earlier RunCode is visible here. Returns when the guest
// sends an exit frame; if ctx is cancelled the connection is closed, which the
// guest observes. Code is not logged.
func (s *StreamConn) RunCode(ctx context.Context, req *RunCodeRequest, onFrame func(ExecStreamFrame)) error {
	data, err := json.Marshal(&Request{Type: TypeRunCode, RunCode: req})
	if err != nil {
		return err
	}
	if _, err := s.conn.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("send run_code: %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.conn.Close()
		case <-done:
		}
	}()

	for s.scanner.Scan() {
		var f ExecStreamFrame
		if err := json.Unmarshal(s.scanner.Bytes(), &f); err != nil {
			return fmt.Errorf("decode run_code frame: %w", err)
		}
		onFrame(f)
		if f.Kind == FrameExit {
			return nil
		}
	}
	if err := s.scanner.Err(); err != nil {
		return fmt.Errorf("recv run_code: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("run_code: connection closed before exit frame")
}

// Exec runs command to completion over the stream and returns the aggregated
// stdout/stderr and exit code, matching the one-shot ExecResponse shape. It is
// the streaming-native equivalent of Client.Exec and is what the HTTP /v1/exec
// handler uses so blocking and streaming share one guest code path.
func (s *StreamConn) Exec(command, workingDir string, env map[string]string, timeout int) (*ExecResponse, error) {
	var out, errb strings.Builder
	exit, err := s.ExecStream(context.Background(), &ExecRequest{
		Command:    command,
		WorkingDir: workingDir,
		Env:        env,
		Timeout:    timeout,
	}, func(stream StreamName, data []byte) error {
		if stream == StreamStdout {
			out.Write(data)
		} else {
			errb.Write(data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if exit.Error != "" {
		return nil, fmt.Errorf("exec_stream: %s", exit.Error)
	}
	return &ExecResponse{
		ExitCode:   exit.ExitCode,
		Stdout:     out.String(),
		Stderr:     errb.String(),
		ExecTimeMs: exit.ExecTimeMs,
	}, nil
}

// OutputFunc receives one slice of raw PTY output bytes as it arrives.
// Returning a non-nil error stops the stream early.
type OutputFunc func(data []byte) error

// Pty opens an interactive pseudo-terminal in the guest and streams its output
// to onOutput, returning the terminal PtyFrame (exit code, and any spawn
// error). Unlike ExecStream this connection is BIDIRECTIONAL: the caller writes
// input and resize frames concurrently via SendInput and Resize while Pty reads
// output frames. If ctx is cancelled the connection is closed, which the guest
// observes and uses to kill the shell process group.
func (s *StreamConn) Pty(ctx context.Context, req *PtyRequest, onOutput OutputFunc) (*PtyFrame, error) {
	data, err := json.Marshal(&Request{Type: TypePty, Pty: req})
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	_, werr := s.conn.Write(append(data, '\n'))
	s.writeMu.Unlock()
	// Unblock any SendInput/Resize callers now that the open request is on the
	// wire (even on error: a blocked writer should observe the closed conn, not
	// hang forever).
	s.signalPtyReady()
	if werr != nil {
		return nil, fmt.Errorf("send pty: %w", werr)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.conn.Close()
		case <-done:
		}
	}()

	for s.scanner.Scan() {
		var f PtyFrame
		if err := json.Unmarshal(s.scanner.Bytes(), &f); err != nil {
			return nil, fmt.Errorf("decode pty frame: %w", err)
		}
		switch f.Kind {
		case PtyOutput:
			if err := onOutput(f.Data); err != nil {
				return nil, err
			}
		case PtyExit:
			return &f, nil
		default:
			return nil, fmt.Errorf("unexpected pty frame kind: %q", f.Kind)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, fmt.Errorf("recv pty: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("pty: connection closed before exit frame")
}

// SendInput writes one input frame (raw keystroke bytes) to the guest PTY. Safe
// to call concurrently with Pty; the write is mutex-guarded.
func (s *StreamConn) SendInput(data []byte) error {
	return s.writeFrame(PtyFrame{Kind: PtyInput, Data: data})
}

// Resize writes one resize frame; the guest applies it to the PTY master with
// TIOCSWINSZ, and the kernel delivers SIGWINCH to the foreground group.
func (s *StreamConn) Resize(cols, rows int) error {
	return s.writeFrame(PtyFrame{Kind: PtyResize, Cols: cols, Rows: rows})
}

func (s *StreamConn) writeFrame(f PtyFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	// Wait until Pty has put the open request on the wire so this frame cannot
	// be mistaken for the request by the guest's first read.
	<-s.ptyReadyCh()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write pty frame: %w", err)
	}
	return nil
}
