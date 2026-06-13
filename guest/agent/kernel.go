//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// defaultDriverPath is where guest/rootfs/build.sh installs kernel_driver.py.
const defaultDriverPath = "/opt/mitos/kernel_driver.py"

// kernelConfig configures the manager. The zero value resolves python from
// PATH ("python3") and driverPath to defaultDriverPath; tests override both.
type kernelConfig struct {
	python     string
	driverPath string
}

// driverEvent is one JSON line emitted by kernel_driver.py.
type driverEvent struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Text      string            `json:"text,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
	Name      string            `json:"name,omitempty"`
	Value     string            `json:"value,omitempty"`
	Traceback []string          `json:"traceback,omitempty"`
	Status    string            `json:"status,omitempty"`
}

// kernelManager owns the single in-guest kernel driver process for the sandbox.
// It is started lazily on the first run and persists for the sandbox lifetime so
// state (the kernel namespace) survives across run calls. Executions are
// serialized by mu: one execute request is in flight at a time.
type kernelManager struct {
	cfg kernelConfig

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   *bufio.Writer
	stdout  *bufio.Scanner
	started bool
	dead    bool
}

func newKernelManager(cfg kernelConfig) *kernelManager {
	if cfg.python == "" {
		cfg.python = "python3"
	}
	if cfg.driverPath == "" {
		cfg.driverPath = defaultDriverPath
	}
	return &kernelManager{cfg: cfg}
}

// errorFrames emits a KernelUnavailable error frame followed by a terminal exit
// 127 frame, the optional-kernel failure path. msg is actionable remediation.
func errorFrames(emit func(vsock.ExecStreamFrame), msg string) {
	emit(vsock.ExecStreamFrame{
		Kind: vsock.FrameError,
		ErrorInfo: &vsock.ErrorFrame{
			Name:  "KernelUnavailable",
			Value: msg,
		},
	})
	emit(vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: 127})
}

// run executes code in the kernel and calls emit for each translated frame,
// ending with a terminal exit frame. Returns a non-nil error only for an
// unexpected transport failure mid-stream; an absent kernel or a guest-code
// exception is reported via frames (and run returns nil).
func (k *kernelManager) run(code, language string, _ int, emit func(vsock.ExecStreamFrame)) error {
	if language != "" && language != "python" {
		errorFrames(emit, fmt.Sprintf("unsupported language %q: this base image provides only a python kernel", language))
		return nil
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	if err := k.ensureStarted(); err != nil {
		errorFrames(emit, fmt.Sprintf("kernel unavailable: %v; rebuild the base image with FULL_ROOTFS=1 so ipykernel and %s are installed", err, k.cfg.driverPath))
		return nil
	}

	execID := "e"
	reqLine, err := json.Marshal(map[string]string{"id": execID, "code": code})
	if err != nil {
		errorFrames(emit, fmt.Sprintf("encode request: %v", err))
		return nil
	}
	if _, err := k.stdin.Write(append(reqLine, '\n')); err != nil {
		k.dead = true
		errorFrames(emit, fmt.Sprintf("write to kernel: %v", err))
		return nil
	}
	if err := k.stdin.Flush(); err != nil {
		k.dead = true
		errorFrames(emit, fmt.Sprintf("flush to kernel: %v", err))
		return nil
	}

	for k.stdout.Scan() {
		line := k.stdout.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev driverEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			k.dead = true
			return fmt.Errorf("decode kernel event: %w", err)
		}
		switch ev.Kind {
		case "ready":
			// late ready (should not happen post-start); ignore.
		case "stdout":
			emit(vsock.ExecStreamFrame{Kind: vsock.FrameChunk, Stream: vsock.StreamStdout, Data: []byte(ev.Text)})
		case "stderr":
			emit(vsock.ExecStreamFrame{Kind: vsock.FrameChunk, Stream: vsock.StreamStderr, Data: []byte(ev.Text)})
		case "result":
			emit(vsock.ExecStreamFrame{
				Kind:   vsock.FrameResult,
				Result: &vsock.ResultFrame{Text: ev.Text, Data: ev.Data},
			})
		case "error":
			emit(vsock.ExecStreamFrame{
				Kind: vsock.FrameError,
				ErrorInfo: &vsock.ErrorFrame{
					Name:      ev.Name,
					Value:     ev.Value,
					Traceback: ev.Traceback,
				},
			})
		case "done":
			exitCode := 0
			if ev.Status == "error" {
				exitCode = 1
			}
			emit(vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: exitCode})
			return nil
		}
	}
	// Driver closed stdout without a done: treat as a dead kernel.
	k.dead = true
	if err := k.stdout.Err(); err != nil {
		return fmt.Errorf("kernel stream: %w", err)
	}
	errorFrames(emit, "kernel exited unexpectedly")
	return nil
}

// ensureStarted lazily launches the driver process once. Caller holds k.mu.
func (k *kernelManager) ensureStarted() error {
	if k.dead {
		return fmt.Errorf("kernel previously died")
	}
	if k.started {
		return nil
	}
	if _, err := os.Stat(k.cfg.driverPath); err != nil {
		return fmt.Errorf("driver %s not found", k.cfg.driverPath)
	}

	cmd := exec.Command(k.cfg.python, k.cfg.driverPath)
	// Run the kernel in /workspace when it exists (the guest's working tree); a
	// missing dir would fail cmd.Start, so only set it when present.
	if fi, statErr := os.Stat("/workspace"); statErr == nil && fi.IsDir() {
		cmd.Dir = "/workspace"
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("kernel stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("kernel stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start kernel driver: %w", err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 1024*1024), vsock.MaxMessageBytes)
	// Wait for the ready line so the kernel namespace is live before the first run.
	if !scanner.Scan() {
		_ = cmd.Process.Kill()
		return fmt.Errorf("kernel driver produced no ready line")
	}
	var ev driverEvent
	if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil || ev.Kind != "ready" {
		_ = cmd.Process.Kill()
		return fmt.Errorf("kernel driver did not signal ready")
	}

	k.cmd = cmd
	k.stdin = bufio.NewWriter(stdinPipe)
	k.stdout = scanner
	k.started = true
	return nil
}

// shutdown kills the driver process. Safe to call when never started.
func (k *kernelManager) shutdown() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cmd != nil && k.cmd.Process != nil {
		_ = k.cmd.Process.Kill()
	}
	k.dead = true
}

// handleRunCodeStream runs req in the package kernel and writes each resulting
// ExecStreamFrame as an NDJSON line to w (the vsock conn). It always ends with
// an exit frame, including when the kernel is unavailable.
func handleRunCodeStream(w io.Writer, req *vsock.RunCodeRequest) {
	enc := json.NewEncoder(w)
	emit := func(fr vsock.ExecStreamFrame) {
		// A write error means the host hung up; nothing actionable to do but stop.
		_ = enc.Encode(&fr)
	}
	if err := guestKernel.run(req.Code, req.Language, req.Timeout, emit); err != nil {
		emit(vsock.ExecStreamFrame{
			Kind:      vsock.FrameError,
			ErrorInfo: &vsock.ErrorFrame{Name: "KernelStreamError", Value: err.Error()},
		})
		emit(vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: 1})
	}
}
