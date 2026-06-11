package agentcli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// runCLI is a test helper that runs the CLI with a fresh FakeBackend and
// captured output, returning the exit code and the two buffers.
func runCLI(t *testing.T, backend Backend, args ...string) (int, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Run(context.Background(), args, backend, &out, &errw)
	return code, &out, &errw
}

func methods(calls []FakeCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Method
	}
	return out
}

func TestRunCommandCreatesExecsTerminates(t *testing.T) {
	fb := NewFakeBackend()
	fb.ExecResultV = ExecResult{ExitCode: 0, Stdout: "hi\n", Stderr: ""}

	code, out, _ := runCLI(t, fb, "run", "echo hi")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := methods(fb.RecordedCalls())
	want := []string{"create", "exec", "terminate"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("recorded calls = %v, want %v", got, want)
	}
	if !strings.Contains(out.String(), "hi") {
		t.Fatalf("stdout = %q, want it to contain stdout 'hi'", out.String())
	}
}

func TestRunCommandPassesPoolAndTimeout(t *testing.T) {
	fb := NewFakeBackend()
	code, _, _ := runCLI(t, fb, "run", "--pool", "mypool", "--timeout", "45", "ls")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	calls := fb.RecordedCalls()
	if calls[0].Pool != "mypool" {
		t.Fatalf("create pool = %q, want mypool", calls[0].Pool)
	}
	if calls[1].TimeoutSec != 45 {
		t.Fatalf("exec timeout = %d, want 45", calls[1].TimeoutSec)
	}
	if calls[1].Command != "ls" {
		t.Fatalf("exec command = %q, want ls", calls[1].Command)
	}
}

func TestRunCommandReturnsExecExitCode(t *testing.T) {
	fb := NewFakeBackend()
	fb.ExecResultV = ExecResult{ExitCode: 7, Stdout: "", Stderr: "boom\n"}

	code, _, errw := runCLI(t, fb, "run", "false")

	if code != 7 {
		t.Fatalf("exit code = %d, want 7 (exec exit code)", code)
	}
	if !strings.Contains(errw.String(), "boom") {
		t.Fatalf("stderr = %q, want it to contain 'boom'", errw.String())
	}
	// Terminate must still run even when exec is nonzero.
	if got := methods(fb.RecordedCalls()); got[len(got)-1] != "terminate" {
		t.Fatalf("last call = %v, want terminate", got)
	}
}

func TestRunCommandTerminatesEvenOnExecError(t *testing.T) {
	fb := NewFakeBackend()
	fb.Errors["exec"] = errors.New("exec failed")

	code, _, errw := runCLI(t, fb, "run", "echo hi")

	if code == 0 {
		t.Fatalf("exit code = %d, want nonzero on exec error", code)
	}
	got := methods(fb.RecordedCalls())
	if got[len(got)-1] != "terminate" {
		t.Fatalf("calls = %v, want terminate last even on exec error", got)
	}
	if !strings.Contains(errw.String(), "exec failed") {
		t.Fatalf("stderr = %q, want exec error reported", errw.String())
	}
}

func TestSandboxCreatePrintsID(t *testing.T) {
	fb := NewFakeBackend()
	fb.CreateID = "sbx-abc"
	code, out, _ := runCLI(t, fb, "sandbox", "create", "--pool", "p")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "sbx-abc") {
		t.Fatalf("stdout = %q, want it to contain the id", out.String())
	}
	if fb.RecordedCalls()[0].Pool != "p" {
		t.Fatalf("create pool = %q, want p", fb.RecordedCalls()[0].Pool)
	}
}

func TestSandboxLsFormatsList(t *testing.T) {
	fb := NewFakeBackend()
	fb.ListInfos = []SandboxInfo{
		{Name: "sbx-1", Pool: "python", Phase: "Ready", Node: "node-a", Endpoint: "10.0.0.1:9091", Age: 90 * time.Second},
	}
	code, out, _ := runCLI(t, fb, "sandbox", "ls", "-n", "team")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	s := out.String()
	for _, want := range []string{"NAME", "POOL", "PHASE", "NODE", "ENDPOINT", "AGE", "sbx-1", "python", "Ready", "node-a", "10.0.0.1:9091", "1m"} {
		if !strings.Contains(s, want) {
			t.Fatalf("ls output = %q, want it to contain %q", s, want)
		}
	}
	if fb.RecordedCalls()[0].Namespace != "team" {
		t.Fatalf("list namespace = %q, want team", fb.RecordedCalls()[0].Namespace)
	}
}

func TestSandboxLsEmpty(t *testing.T) {
	fb := NewFakeBackend()
	fb.ListInfos = nil
	code, out, _ := runCLI(t, fb, "sandbox", "ls")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "No sandboxes found") {
		t.Fatalf("ls output = %q, want the empty message", out.String())
	}
}

func TestSandboxExecDispatch(t *testing.T) {
	fb := NewFakeBackend()
	fb.ExecResultV = ExecResult{ExitCode: 3, Stdout: "o", Stderr: "e"}
	code, out, errw := runCLI(t, fb, "sandbox", "exec", "sbx-1", "echo", "hi")
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	calls := fb.RecordedCalls()
	if calls[0].Method != "exec" || calls[0].SandboxID != "sbx-1" {
		t.Fatalf("calls = %v, want exec on sbx-1", calls)
	}
	if calls[0].Command != "echo hi" {
		t.Fatalf("exec command = %q, want 'echo hi'", calls[0].Command)
	}
	if !strings.Contains(out.String(), "o") || !strings.Contains(errw.String(), "e") {
		t.Fatalf("out=%q err=%q, want stdout and stderr printed", out.String(), errw.String())
	}
}

func TestSandboxForkDispatch(t *testing.T) {
	fb := NewFakeBackend()
	fb.ForkIDs = []string{"f1", "f2", "f3"}
	code, out, _ := runCLI(t, fb, "sandbox", "fork", "sbx-1", "--replicas", "3")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	calls := fb.RecordedCalls()
	if calls[0].Method != "fork" || calls[0].SandboxID != "sbx-1" || calls[0].Replicas != 3 {
		t.Fatalf("calls = %v, want fork sbx-1 x3", calls)
	}
	for _, id := range []string{"f1", "f2", "f3"} {
		if !strings.Contains(out.String(), id) {
			t.Fatalf("fork output = %q, want it to contain %q", out.String(), id)
		}
	}
}

func TestSandboxTerminateDispatch(t *testing.T) {
	fb := NewFakeBackend()
	code, _, _ := runCLI(t, fb, "sandbox", "terminate", "sbx-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	calls := fb.RecordedCalls()
	if calls[0].Method != "terminate" || calls[0].SandboxID != "sbx-1" {
		t.Fatalf("calls = %v, want terminate sbx-1", calls)
	}
}

func TestUnknownSubcommandReturnsTwo(t *testing.T) {
	fb := NewFakeBackend()
	code, _, errw := runCLI(t, fb, "frobnicate")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "Usage") && !strings.Contains(errw.String(), "usage") {
		t.Fatalf("stderr = %q, want usage printed", errw.String())
	}
}

func TestMissingArgsReturnsTwo(t *testing.T) {
	cases := [][]string{
		{},
		{"run"},
		{"sandbox"},
		{"sandbox", "exec"},
		{"sandbox", "exec", "sbx-1"},
		{"sandbox", "fork"},
		{"sandbox", "terminate"},
	}
	for _, args := range cases {
		fb := NewFakeBackend()
		code, _, errw := runCLI(t, fb, args...)
		if code != 2 {
			t.Fatalf("args %v: exit code = %d, want 2", args, code)
		}
		if errw.Len() == 0 {
			t.Fatalf("args %v: want usage on stderr", args)
		}
	}
}

func TestHelpReturnsZero(t *testing.T) {
	fb := NewFakeBackend()
	for _, h := range []string{"--help", "-h"} {
		code, out, _ := runCLI(t, fb, h)
		if code != 0 {
			t.Fatalf("%s: exit code = %d, want 0", h, code)
		}
		if !strings.Contains(out.String(), "Usage") && !strings.Contains(out.String(), "usage") {
			t.Fatalf("%s: out = %q, want usage", h, out.String())
		}
	}
}

func TestBackendErrorReportedNonzero(t *testing.T) {
	fb := NewFakeBackend()
	fb.Errors["create"] = errors.New("cluster unreachable")
	code, _, errw := runCLI(t, fb, "sandbox", "create")
	if code == 0 {
		t.Fatalf("exit code = %d, want nonzero on backend error", code)
	}
	if !strings.Contains(errw.String(), "cluster unreachable") {
		t.Fatalf("stderr = %q, want the backend error", errw.String())
	}
}

func TestDevStubReturnsNotWired(t *testing.T) {
	// In Task 1 dev is a stub; Task 2 wires it. Either way an unwired backend
	// path must not panic and must report something to stderr with a nonzero
	// code when no runner is configured.
	fb := NewFakeBackend()
	code, _, errw := runCLI(t, fb, "dev", "up")
	if code == 0 {
		t.Fatalf("exit code = %d, want nonzero for unwired dev", code)
	}
	if errw.Len() == 0 {
		t.Fatalf("want a message on stderr for unwired dev")
	}
}
