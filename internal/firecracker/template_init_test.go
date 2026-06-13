package firecracker

import (
	"strings"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/vsock"
)

// newTestTM builds a TemplateManager with an injected connectInit seam and a
// no-op sleep so the readiness/init logic can be exercised without booting a VM.
func newTestTM(connect func(string) (execFunc, func(), error)) (*TemplateManager, *bool) {
	slept := new(bool)
	return &TemplateManager{
		connectInit:  connect,
		fallbackWait: 5 * time.Second,
		sleep:        func(time.Duration) { *slept = true },
	}, slept
}

// TestAwaitReady_ConnectsEvenWithNoInitCommands is the Fix A regression guard:
// with NO init commands the build must still wait for the guest agent (the
// connect+ping readiness signal) before snapshotting, so a half-booted VM is
// never captured. It asserts connectInit IS called and no fallback sleep is
// used when the agent answers.
func TestAwaitReady_ConnectsEvenWithNoInitCommands(t *testing.T) {
	connected := false
	f := &fakeExec{}
	tm, slept := newTestTM(func(string) (execFunc, func(), error) {
		connected = true
		return f.exec, func() {}, nil
	})
	if err := tm.awaitReadyAndRunInit("t1", "vsock.sock", nil); err != nil {
		t.Fatalf("awaitReadyAndRunInit: %v", err)
	}
	if !connected {
		t.Fatal("must connect to the guest agent for readiness even with no init commands")
	}
	if *slept {
		t.Fatal("must not use the no-agent fallback sleep when the agent answered")
	}
	if len(f.ran) != 0 {
		t.Fatalf("no init commands should have run, got %v", f.ran)
	}
}

// TestAwaitReady_FallsBackToSleepWhenNoAgent covers the edge back-compat case:
// a rootfs without the agent never answers, and with no init commands the build
// logs a warning and falls back to a fixed wait rather than failing.
func TestAwaitReady_FallsBackToSleepWhenNoAgent(t *testing.T) {
	tm, slept := newTestTM(func(string) (execFunc, func(), error) {
		return nil, nil, strErr("no agent")
	})
	if err := tm.awaitReadyAndRunInit("t1", "vsock.sock", nil); err != nil {
		t.Fatalf("no-agent + no-init must fall back, not fail: %v", err)
	}
	if !*slept {
		t.Fatal("expected the fixed fallback wait when the agent never answered")
	}
}

// TestAwaitReady_NoAgentWithInitCommandsFails: if init commands are requested
// but the agent never answers, there is no way to run them, so the build fails
// (it must never silently skip init and snapshot a half-built template).
func TestAwaitReady_NoAgentWithInitCommandsFails(t *testing.T) {
	tm, slept := newTestTM(func(string) (execFunc, func(), error) {
		return nil, nil, strErr("no agent")
	})
	err := tm.awaitReadyAndRunInit("t1", "vsock.sock", []string{"echo hi"})
	if err == nil {
		t.Fatal("expected an error when init commands are requested but no agent answers")
	}
	if !strings.Contains(err.Error(), "cannot run init commands") {
		t.Errorf("error should explain init cannot run: %v", err)
	}
	if *slept {
		t.Fatal("must not fall back to sleep when init commands were requested")
	}
}

// TestAwaitReady_RunsInitWhenAgentReady confirms the happy path still runs the
// init commands in order when the agent answers.
func TestAwaitReady_RunsInitWhenAgentReady(t *testing.T) {
	f := &fakeExec{}
	closed := false
	tm, _ := newTestTM(func(string) (execFunc, func(), error) {
		return f.exec, func() { closed = true }, nil
	})
	if err := tm.awaitReadyAndRunInit("t1", "vsock.sock", []string{"echo a", "echo b"}); err != nil {
		t.Fatalf("awaitReadyAndRunInit: %v", err)
	}
	if len(f.ran) != 2 || f.ran[0] != "echo a" || f.ran[1] != "echo b" {
		t.Errorf("init commands ran wrong: %v", f.ran)
	}
	if !closed {
		t.Error("connection cleanup must run")
	}
}

// fakeExec records the commands it is asked to run and returns a scripted
// response per command, standing in for the guest-agent vsock exec so the
// init-command safety logic can be tested without booting Firecracker.
type fakeExec struct {
	ran      []string
	response map[string]*vsock.ExecResponse
	err      error
}

func (f *fakeExec) exec(command string) (*vsock.ExecResponse, error) {
	f.ran = append(f.ran, command)
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.response[command]; ok {
		return r, nil
	}
	return &vsock.ExecResponse{ExitCode: 0}, nil
}

func TestRunInitCommands_AllSucceed(t *testing.T) {
	f := &fakeExec{}
	cmds := []string{"echo a", "pip install flask"}
	if err := runInitCommands(f.exec, cmds); err != nil {
		t.Fatalf("runInitCommands: unexpected error: %v", err)
	}
	if len(f.ran) != 2 || f.ran[0] != "echo a" || f.ran[1] != "pip install flask" {
		t.Errorf("commands ran in wrong order or count: %v", f.ran)
	}
}

func TestRunInitCommands_NonzeroExitFails(t *testing.T) {
	f := &fakeExec{response: map[string]*vsock.ExecResponse{
		"pip install nope": {ExitCode: 1, Stderr: "No matching distribution found"},
	}}
	cmds := []string{"echo a", "pip install nope", "echo never"}
	err := runInitCommands(f.exec, cmds)
	if err == nil {
		t.Fatal("expected error when an init command exits nonzero, got nil")
	}
	// The error must name the failing command and carry its stderr so the
	// operator can see why the template build was aborted.
	if !strings.Contains(err.Error(), "pip install nope") {
		t.Errorf("error missing failing command: %v", err)
	}
	if !strings.Contains(err.Error(), "No matching distribution found") {
		t.Errorf("error missing stderr: %v", err)
	}
	// Execution must stop at the first failure: "echo never" must not run.
	if len(f.ran) != 2 {
		t.Errorf("expected exactly 2 commands run before abort, got %v", f.ran)
	}
}

func TestRunInitCommands_TransportErrorFails(t *testing.T) {
	f := &fakeExec{err: strErr("connection closed")}
	err := runInitCommands(f.exec, []string{"echo a"})
	if err == nil {
		t.Fatal("expected error on transport failure")
	}
	if !strings.Contains(err.Error(), "connection closed") {
		t.Errorf("error missing transport cause: %v", err)
	}
}

type strErr string

func (e strErr) Error() string { return string(e) }
