//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// writeFakeDriver writes a shell script that mimics kernel_driver.py: it prints
// a ready line, then for each stdin request line emits canned event lines and a
// done. It lets us drive the manager without Python or ipykernel.
func writeFakeDriver(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake_driver.sh")
	// printf (not echo) so the literal backslash-n inside the JSON string stays
	// a two-byte escape, not a real newline that would split the NDJSON line.
	// %s with a single argument prints the format verbatim and adds no newline,
	// so each printf ends with an explicit \n that terminates exactly one frame.
	script := `#!/bin/sh
printf '%s\n' '{"id":"","kind":"ready"}'
while IFS= read -r line; do
  printf '%s\n' '{"id":"x","kind":"stdout","text":"hi\n"}'
  printf '%s\n' '{"id":"x","kind":"result","text":"42","data":{"text/plain":"42","image/png":"aGVsbG8="}}'
  printf '%s\n' '{"id":"x","kind":"done","status":"ok"}'
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake driver: %v", err)
	}
	return path
}

func TestKernelManagerTranslatesEvents(t *testing.T) {
	dir := t.TempDir()
	driver := writeFakeDriver(t, dir)

	km := newKernelManager(kernelConfig{
		python:     "/bin/sh",
		driverPath: driver,
	})
	defer km.shutdown()

	var frames []vsock.ExecStreamFrame
	err := km.run("print('hi')\n42", "python", 30, func(fr vsock.ExecStreamFrame) {
		frames = append(frames, fr)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %+v", len(frames), frames)
	}
	if frames[0].Kind != vsock.FrameChunk || frames[0].Stream != vsock.StreamStdout || string(frames[0].Data) != "hi\n" {
		t.Fatalf("frame 0 = %+v", frames[0])
	}
	if frames[1].Kind != vsock.FrameResult || frames[1].Result == nil ||
		frames[1].Result.Text != "42" || frames[1].Result.Data["image/png"] != "aGVsbG8=" {
		t.Fatalf("frame 1 = %+v", frames[1])
	}
	if frames[2].Kind != vsock.FrameExit || frames[2].ExitCode != 0 {
		t.Fatalf("frame 2 = %+v", frames[2])
	}
}

func TestKernelManagerUnavailable(t *testing.T) {
	km := newKernelManager(kernelConfig{
		python:     "/bin/sh",
		driverPath: "/nonexistent/kernel_driver.py",
	})
	defer km.shutdown()

	var frames []vsock.ExecStreamFrame
	err := km.run("1", "python", 30, func(fr vsock.ExecStreamFrame) {
		frames = append(frames, fr)
	})
	if err != nil {
		t.Fatalf("run should not hard-error, it frames the failure: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("want error+exit frames, got %+v", frames)
	}
	if frames[0].Kind != vsock.FrameError || frames[0].ErrorInfo == nil || frames[0].ErrorInfo.Name != "KernelUnavailable" {
		t.Fatalf("frame 0 = %+v", frames[0])
	}
	last := frames[len(frames)-1]
	if last.Kind != vsock.FrameExit || last.ExitCode != 127 {
		t.Fatalf("last frame = %+v", last)
	}
}

func TestKernelManagerRejectsNonPython(t *testing.T) {
	dir := t.TempDir()
	driver := writeFakeDriver(t, dir)
	km := newKernelManager(kernelConfig{python: "/bin/sh", driverPath: driver})
	defer km.shutdown()

	var frames []vsock.ExecStreamFrame
	_ = km.run("puts 1", "ruby", 30, func(fr vsock.ExecStreamFrame) {
		frames = append(frames, fr)
	})
	if frames[0].Kind != vsock.FrameError || frames[0].ErrorInfo == nil || frames[0].ErrorInfo.Name != "KernelUnavailable" {
		t.Fatalf("want KernelUnavailable for ruby, got %+v", frames[0])
	}
}
