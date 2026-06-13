//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/paperclipinc/mitos/internal/guestenv"
	"github.com/paperclipinc/mitos/internal/vsock"
	"golang.org/x/sys/unix"
)

// ptyOutputChunkBytes bounds one PTY read before it is framed. 32 KiB keeps a
// frame small relative to vsock.MaxMessageBytes and flushes output promptly.
const ptyOutputChunkBytes = 32 << 10

// openPTY opens a new pseudo-terminal pair via /dev/ptmx and returns the master
// file and the slave path (/dev/pts/N). The caller opens the slave on the child
// side. Raw syscalls via golang.org/x/sys/unix keep the guest free of any
// third-party PTY dependency.
func openPTY() (master *os.File, slavePath string, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// Unlock the slave (TIOCSPTLCK = 0).
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, "", fmt.Errorf("unlock pts: %w", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, "", fmt.Errorf("get pts number: %w", err)
	}
	return m, fmt.Sprintf("/dev/pts/%d", n), nil
}

// setWinsize applies cols/rows to the PTY master. The kernel then delivers
// SIGWINCH to the foreground process group automatically.
func setWinsize(master *os.File, cols, rows int) error {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Col: uint16(cols),
		Row: uint16(rows),
	})
}

// handlePtyStream allocates a PTY, starts the shell as a session leader on the
// slave, and pumps PTY<->vsock bidirectionally on the DEDICATED conn: a reader
// goroutine decodes input/resize frames from the host, the main loop frames PTY
// output back, and on shell exit it writes the terminal exit frame. The shell
// runs in its own session/process group so a connection drop or the host's
// kill kills the whole tree.
//
// sc is the dispatcher's scanner, handed over (not freshly allocated): it may
// already hold input/resize frames that arrived coalesced with the open-request
// line in a single read (bufio.Scanner reads in chunks). Reusing it ensures
// those early frames are consumed rather than dropped by a fresh scanner.
func handlePtyStream(conn net.Conn, sc *bufio.Scanner, req *vsock.PtyRequest) {
	master, slavePath, err := openPTY()
	if err != nil {
		writePtyFrame(conn, vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: 1, Error: err.Error()})
		return
	}
	defer master.Close()

	if err := setWinsize(master, req.Cols, req.Rows); err != nil {
		writePtyFrame(conn, vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: 1, Error: fmt.Sprintf("set winsize: %v", err)})
		return
	}

	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		writePtyFrame(conn, vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: 1, Error: fmt.Sprintf("open slave: %v", err)})
		return
	}

	shell := req.Command
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = req.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = "/workspace"
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// New session + controlling terminal on the slave: the shell becomes a
	// session leader in its own process group, so the kernel routes SIGWINCH
	// to it and a group kill reaches every child. Ctty is the child-side fd
	// NUMBER, which os/exec assigns from the order of Stdin/Stdout/Stderr +
	// ExtraFiles: slave is wired to all three standard streams, so it lands on
	// child fd 0. (It is NOT the parent's slave.Fd() raw descriptor; passing
	// that trips "Setctty set but Ctty not valid in child".)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	configuredMu.Lock()
	configured := make(map[string]string, len(configuredEnv))
	for k, v := range configuredEnv {
		configured[k] = v
	}
	configuredMu.Unlock()
	cmd.Env = append(guestenv.Merge(os.Environ(), configured, req.Env), "TERM=xterm-256color")

	if err := cmd.Start(); err != nil {
		slave.Close()
		writePtyFrame(conn, vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: 1, Error: fmt.Sprintf("start shell: %v", err)})
		return
	}
	// The child holds the slave now; the parent closes its copy so the master
	// sees EOF when the shell exits.
	slave.Close()

	var writeMu sync.Mutex
	killGroup := func() {
		if cmd.Process != nil {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		}
	}

	// Reader goroutine: host->guest. Decodes input/resize frames. A scan error
	// (host hung up or ctx-cancel closed the conn) kills the shell group.
	go func() {
		for sc.Scan() {
			var f vsock.PtyFrame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				continue
			}
			switch f.Kind {
			case vsock.PtyInput:
				if _, err := master.Write(f.Data); err != nil {
					killGroup()
					return
				}
			case vsock.PtyResize:
				_ = setWinsize(master, f.Cols, f.Rows)
			}
		}
		// Host closed the connection: kill the shell.
		killGroup()
	}()

	// Main loop: guest->host. Frame PTY output until the master reports EOF
	// (shell exited or all slave fds closed).
	buf := make([]byte, ptyOutputChunkBytes)
	for {
		n, rerr := master.Read(buf)
		if n > 0 {
			writeMu.Lock()
			writePtyFrame(conn, vsock.PtyFrame{Kind: vsock.PtyOutput, Data: append([]byte(nil), buf[:n]...)})
			writeMu.Unlock()
		}
		if rerr != nil {
			break
		}
	}

	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	writeMu.Lock()
	writePtyFrame(conn, vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: exitCode})
	writeMu.Unlock()
}

// writePtyFrame marshals one frame and writes it as a single newline-delimited
// line. A write error means the host hung up; the caller's read loop ends when
// the master closes.
func writePtyFrame(conn net.Conn, f vsock.PtyFrame) {
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(b, '\n'))
}
