# Secrets Into Guest + Live-Fork Inheritance Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close issues #8 (claim-time env/secrets delivered into the guest over vsock) and #7 (live forks of secret-holding sandboxes are rejected without explicit opt-in), with the threat-model/fork-correctness docs updated in the same PR.

**Architecture:** A new `configure` message on the existing JSON-over-vsock protocol carries env+secrets from forkd to the guest agent immediately after restore; the agent merges them into every subsequent exec's environment. Delivery policy: strict when the engine is real (secrets must land or the fork fails and the VM is reaped), lenient in mock mode (no guest exists). The fork controller gains a spec-level default-deny: source claims holding secrets cannot be live-forked without `allowSecretInheritance: true`, recorded as typed conditions.

**Tech Stack:** Go 1.26, JSON-over-vsock protocol (`internal/vsock`), controller-runtime + envtest, controller-gen for CRD regen, KVM CI via `cmd/test-agent`.

**Context for the implementer:**
- Protocol: `internal/vsock/protocol.go` (Request/Response unions, newline-delimited JSON), client: `internal/vsock/client.go`, existing fake-server test pattern: `internal/vsock/client_test.go`.
- Guest agent: `guest/agent/main.go` (`//go:build linux`, PID 1). Exec env today: `cmd.Env = append(os.Environ(), perRequestEnv...)`.
- forkd: `internal/daemon/server.go` (`Server.Fork` gets env+secrets, calls `registerAgent`, currently drops the values), `internal/daemon/sandbox_api.go` (`SandboxAPI` holds `agents map[string]*vsock.Client`; `RegisterSandbox` falls back to unix socket `/tmp/sandbox-agent-52.sock` when the vsock UDS fails).
- Engines: `internal/fork/engine.go` (real; `GetCapacity().KVMAvailable=true`), `internal/fork/mock.go` (mock; `KVMAvailable=false`).
- Fork CRD: `api/v1alpha1/types.go` `SandboxForkSpec`; controller: `internal/controller/sandboxfork_controller.go` (fetches source claim, waits for Ready, calls forkd). `setCondition` helper lives in `sandboxpool_controller.go`.
- Regen: `controller-gen` and `setup-envtest` are in `~/go/bin`. CRD regen: `make generate manifests`. Controller tests: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -count=1 -race`.
- Lint must stay clean: `golangci-lint run --timeout=5m` (and with `GOOS=linux` for the agent). NEVER touch README.md (intentional uncommitted local modification). `git add` explicit paths only; stay on the branch.
- SECURITY RULE for every task: secret VALUES are never logged, never written to host paths, never put in error messages or condition messages; log/report keys-counts only.

---

### Task 1: `configure` message in the vsock protocol + client

**Files:**
- Modify: `internal/vsock/protocol.go`
- Modify: `internal/vsock/client.go`
- Test: `internal/vsock/client_test.go` (append; follow the existing fake-server pattern in that file; read it first)

- [ ] **Step 1: Write the failing test** (append; adapt the fake-server scaffolding names to what the file already uses)

```go
func TestConfigure(t *testing.T) {
	var got *ConfigureRequest
	// fake agent server on a unix socket, same pattern as the other tests in
	// this file: accept, scan lines, unmarshal Request, respond.
	sockPath := startFakeAgent(t, func(req *Request) Response {
		if req.Type == TypeConfigure {
			got = req.Configure
			return Response{OK: true}
		}
		return Response{OK: false, Error: "unexpected type"}
	})

	client, err := ConnectUnix(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	err = client.Configure(
		map[string]string{"SESSION": "abc"},
		map[string]string{"API_KEY": "v"},
	)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got == nil || got.Env["SESSION"] != "abc" || got.Secrets["API_KEY"] != "v" {
		t.Fatalf("agent saw %+v", got)
	}
}
```

If `client_test.go` has no reusable `startFakeAgent` helper, extract one from the inline fake-server code while you are there (refactor the existing tests to use it; behavior-preserving, keeps the file DRY).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/vsock/ -run TestConfigure -count=1`
Expected: compile FAIL (`TypeConfigure`, `ConfigureRequest`, `client.Configure` undefined).

- [ ] **Step 3: Implement**

`protocol.go`: add to the consts block:

```go
	TypeConfigure RequestType = "configure"
```

add to `Request`:

```go
	Configure *ConfigureRequest `json:"configure,omitempty"`
```

add the message type (with the security contract in the comment):

```go
// ConfigureRequest delivers claim-time environment and secrets to the guest
// after restore. Values must never be logged or echoed by either side; they
// exist only in the request payload and the guest process environment.
type ConfigureRequest struct {
	Env     map[string]string `json:"env,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
}
```

`client.go`: add:

```go
// Configure delivers claim-time env and secrets to the guest agent.
func (c *Client) Configure(env, secrets map[string]string) error {
	_, err := c.send(&Request{
		Type:      TypeConfigure,
		Configure: &ConfigureRequest{Env: env, Secrets: secrets},
	})
	return err
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/vsock/ -count=1 && go build ./...`
Expected: PASS (all vsock tests, including the pre-existing ones if you refactored the fake server).

- [ ] **Step 5: Commit**

```bash
git add internal/vsock/protocol.go internal/vsock/client.go internal/vsock/client_test.go
git commit -m "feat: configure message on the vsock protocol

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: env-merge helper package

The guest agent is `//go:build linux`; the merge logic must be unit-testable on any platform, so it lives in a small shared package.

**Files:**
- Create: `internal/guestenv/merge.go`
- Create: `internal/guestenv/merge_test.go`

- [ ] **Step 1: Write the failing test**

```go
package guestenv

import (
	"slices"
	"testing"
)

func TestMergePrecedence(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/root", "LANG=C"}
	configured := map[string]string{"HOME": "/workspace", "API_KEY": "k1"}
	request := map[string]string{"API_KEY": "k2", "EXTRA": "e"}

	got := Merge(base, configured, request)

	want := map[string]string{
		"PATH":    "/bin",       // base survives
		"LANG":    "C",          // base survives
		"HOME":    "/workspace", // configured overrides base
		"API_KEY": "k2",         // request overrides configured
		"EXTRA":   "e",          // request adds
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vars %v, want %d", len(got), got, len(want))
	}
	for k, v := range want {
		if !slices.Contains(got, k+"="+v) {
			t.Errorf("missing %s=%s in %v", k, v, got)
		}
	}
}

func TestMergeNilMaps(t *testing.T) {
	got := Merge([]string{"A=1"}, nil, nil)
	if len(got) != 1 || got[0] != "A=1" {
		t.Fatalf("got %v", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/guestenv/ -count=1`
Expected: FAIL; package does not exist.

- [ ] **Step 3: Implement `internal/guestenv/merge.go`**

```go
// Package guestenv builds the environment for guest exec sessions.
// Kept separate from the linux-only guest agent so the precedence rules are
// unit-testable on any platform.
package guestenv

import "strings"

// Merge combines a base environment (os.Environ format) with configured
// (claim-time env+secrets) and per-request variables. Precedence, lowest to
// highest: base < configured < request. Later duplicates win.
func Merge(base []string, configured, request map[string]string) []string {
	merged := make(map[string]string, len(base)+len(configured)+len(request))
	order := make([]string, 0, len(base)+len(configured)+len(request))

	set := func(k, v string) {
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		merged[k] = v
	}

	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	for k, v := range configured {
		set(k, v)
	}
	for k, v := range request {
		set(k, v)
	}

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/guestenv/ -count=1`
Expected: PASS. (Map-iteration order across `configured`/`request` is fine; `set` keys the order map, duplicates resolve by precedence, and tests assert membership not order.)

- [ ] **Step 5: Commit**

```bash
git add internal/guestenv/
git commit -m "feat: guestenv.Merge with base<configured<request precedence

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: guest agent handles configure; test-agent exercises it in KVM CI

**Files:**
- Modify: `guest/agent/main.go`
- Modify: `cmd/test-agent/main.go`

There is no host-side unit test for the linux-only agent; the contract is pinned by Task 1's protocol test, Task 2's merge test, and the KVM CI run of `test-agent` (`.github/workflows/kvm-test.yaml` already builds the agent into a rootfs and runs `cmd/test-agent` against the booted VM; no workflow change needed).

- [ ] **Step 1: Implement configure in the agent (`guest/agent/main.go`)**

Add near the top (after `var startTime`):

```go
// configuredEnv holds claim-time env+secrets delivered via the configure
// message. Values are never logged. Guarded by configuredMu.
var (
	configuredMu  sync.Mutex
	configuredEnv = map[string]string{}
)
```

(add `"sync"` to imports.)

In `handleRequest`, add a case (mirror the style of the others):

```go
	case vsock.TypeConfigure:
		if req.Configure == nil {
			return vsock.Response{OK: false, Error: "configure request is nil"}
		}
		return handleConfigure(req.Configure)
```

Add the handler; note it reports counts, never values:

```go
func handleConfigure(req *vsock.ConfigureRequest) vsock.Response {
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
```

In `handleExec`, replace the env construction:

```go
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	// Inherit base environment
	cmd.Env = append(os.Environ(), cmd.Env...)
```

with:

```go
	configuredMu.Lock()
	configured := make(map[string]string, len(configuredEnv))
	for k, v := range configuredEnv {
		configured[k] = v
	}
	configuredMu.Unlock()
	cmd.Env = guestenv.Merge(os.Environ(), configured, req.Env)
```

(import `"github.com/paperclipinc/sandbox/internal/guestenv"`.)

- [ ] **Step 2: Cross-compile check**

Run: `GOOS=linux GOARCH=amd64 go build ./guest/agent/ && GOOS=linux golangci-lint run --timeout=5m ./guest/agent/`
Expected: clean build and lint.

- [ ] **Step 3: Extend `cmd/test-agent/main.go`**

After the existing list-dir test block, append:

```go
	// Test configure: claim-time env+secrets must reach exec sessions.
	if err := client.Configure(
		map[string]string{"CFG_VAR": "cfg"},
		map[string]string{"TEST_SECRET": "s3cr3t-canary"},
	); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL configure: %v\n", err)
		os.Exit(1)
	}
	result, err = client.Exec(`echo -n "$CFG_VAR:$TEST_SECRET"`, "/workspace", nil, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL exec after configure: %v\n", err)
		os.Exit(1)
	}
	if result.Stdout != "cfg:s3cr3t-canary" {
		fmt.Fprintf(os.Stderr, "FAIL configure: stdout=%q, want cfg:s3cr3t-canary\n", result.Stdout)
		os.Exit(1)
	}
	// Per-request env must override configured values.
	result, err = client.Exec(`echo -n "$CFG_VAR"`, "/workspace", map[string]string{"CFG_VAR": "override"}, 10)
	if err != nil || result.Stdout != "override" {
		fmt.Fprintf(os.Stderr, "FAIL configure precedence: err=%v stdout=%q\n", err, result.Stdout)
		os.Exit(1)
	}
	fmt.Println("PASS configure: env+secrets visible to exec, request overrides configured")
```

(reuse the existing `result`/`err` variables; check how they are declared above and adapt `:=` vs `=`.)

- [ ] **Step 4: Build everything**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add guest/agent/main.go cmd/test-agent/main.go
git commit -m "feat: guest agent applies configured env+secrets to exec sessions

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: forkd delivers env+secrets after restore (strict when real, lenient when mock)

**Files:**
- Modify: `internal/daemon/sandbox_api.go` (add `Configure`)
- Modify: `internal/daemon/server.go` (delivery policy in `Fork`)
- Test: `internal/daemon/delivery_test.go` (new)

Policy (capture in code comments): if the engine is real (`GetCapacity().KVMAvailable`), a fork carrying secrets MUST deliver them; on any registration/configure failure the sandbox is terminated and the fork fails. Env-only payloads are best-effort (log). The mock engine has no guest at all, so delivery is skipped entirely (the envtest/kind paths keep working).

- [ ] **Step 1: Write the failing test** (`internal/daemon/delivery_test.go`)

```go
package daemon

import (
	"context"
	"testing"

	"github.com/paperclipinc/sandbox/internal/fork"
)

// kvmReportingEngine wraps MockEngine but claims to be a real KVM engine,
// so Server.Fork applies the strict delivery policy.
type kvmReportingEngine struct {
	*fork.MockEngine
	terminated []string
}

func (e *kvmReportingEngine) GetCapacity() fork.Capacity {
	c := e.MockEngine.GetCapacity()
	c.KVMAvailable = true
	return c
}

func (e *kvmReportingEngine) Terminate(id string) error {
	e.terminated = append(e.terminated, id)
	return e.MockEngine.Terminate(id)
}

func TestForkWithSecretsFailsWhenAgentUnreachable(t *testing.T) {
	engine := &kvmReportingEngine{MockEngine: fork.NewMockEngine()}
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	_, err := srv.Fork(context.Background(), "py", "sb-secret", nil,
		map[string]string{"API_KEY": "v"})
	if err == nil {
		t.Fatal("fork with undeliverable secrets must fail")
	}
	if len(engine.terminated) != 1 || engine.terminated[0] != "sb-secret" {
		t.Fatalf("sandbox not reaped after failed delivery: %v", engine.terminated)
	}
	if got := engine.GetCapacity().ActiveSandboxes; got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}
}

func TestForkEnvOnlyIsBestEffortWhenAgentUnreachable(t *testing.T) {
	engine := &kvmReportingEngine{MockEngine: fork.NewMockEngine()}
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	result, err := srv.Fork(context.Background(), "py", "sb-env",
		map[string]string{"SESSION": "abc"}, nil)
	if err != nil {
		t.Fatalf("env-only fork should succeed best-effort: %v", err)
	}
	if result.SandboxID != "sb-env" {
		t.Fatalf("got %q", result.SandboxID)
	}
}

func TestForkMockEngineSkipsDelivery(t *testing.T) {
	engine := fork.NewMockEngine() // KVMAvailable=false
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))

	if _, err := srv.Fork(context.Background(), "py", "sb-mock", nil,
		map[string]string{"API_KEY": "v"}); err != nil {
		t.Fatalf("mock-mode fork must not require delivery: %v", err)
	}
}
```

NOTE: `RegisterSandbox` falls back to the fixed unix socket `/tmp/sandbox-agent-52.sock`. If something on the machine is listening there, the "unreachable" tests could accidentally connect. Make the tests robust: the strict test asserts failure OR, if you find the fallback connecting, remove that ambiguity by scoping the fallback (preferred): change `RegisterSandbox` to attempt the unix fallback ONLY when the vsock UDS path is empty (mock paths), not on every failure. Inspect the call paths and pick the cleanest; document what you chose in the test file.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/daemon/ -run 'TestFork(WithSecrets|EnvOnly|MockEngine)' -count=1`
Expected: FAIL; strict test fails because today's `Server.Fork` ignores secrets and succeeds.

- [ ] **Step 3: Implement**

`sandbox_api.go`: add:

```go
// Configure delivers claim-time env and secrets to a sandbox's guest agent.
// Values are never logged.
func (api *SandboxAPI) Configure(sandboxID string, env, secrets map[string]string) error {
	agent, err := api.getAgent(sandboxID)
	if err != nil {
		return err
	}
	return agent.Configure(env, secrets)
}
```

`server.go`: replace the `registerAgent` call inside `Fork` with a delivery step:

```go
	if err := s.deliverConfig(result.SandboxID, result.VsockPath, env, secrets); err != nil {
		// A sandbox that reports Ready without its secrets is a lie; reap it.
		_ = s.engine.Terminate(result.SandboxID)
		activeSandboxes.Dec()
		s.sandboxAPI.UnregisterSandbox(result.SandboxID)
		return nil, fmt.Errorf("sandbox %s: secret delivery failed: %w", result.SandboxID, err)
	}
```

and add:

```go
// deliverConfig connects the guest agent and delivers claim-time env+secrets.
// Strict when the engine is real and secrets are present: failure is returned
// so the caller can reap the sandbox. Env-only failures are logged
// (best-effort), and the mock engine is skipped entirely; no guest exists.
// Secret values are never logged.
func (s *Server) deliverConfig(sandboxID, vsockPath string, env, secrets map[string]string) error {
	if !s.engine.GetCapacity().KVMAvailable {
		return nil // mock engine: no guest to deliver to
	}

	strict := len(secrets) > 0

	if err := s.sandboxAPI.RegisterSandbox(sandboxID, vsockPath); err != nil {
		if strict {
			return fmt.Errorf("guest agent not connected: %w", err)
		}
		log.Printf("forkd: sandbox %s: guest agent not connected: %v", sandboxID, err)
		return nil
	}

	if len(env) == 0 && len(secrets) == 0 {
		return nil
	}
	if err := s.sandboxAPI.Configure(sandboxID, env, secrets); err != nil {
		if strict {
			return fmt.Errorf("configure guest: %w", err)
		}
		log.Printf("forkd: sandbox %s: env delivery failed (best-effort): %v", sandboxID, err)
	}
	return nil
}
```

Keep `registerAgent` if `ForkRunning` still uses it; `ForkRunning` deliberately does NOT deliver new config (forks inherit memory; fresh-credential reissue is issue #7's end state); add that as a comment on `ForkRunning`.

- [ ] **Step 4: Add a happy-path delivery test with a fake agent**

First, add a settable `VsockDir` field to `MockEngine` (`internal/fork/mock.go`): field `VsockDir string` on the struct; in both `Fork` and `ForkRunning`, compute the result's VsockPath as:

```go
	vsockDir := e.VsockDir
	if vsockDir == "" {
		vsockDir = "/tmp/agent-run-mock"
	}
	// ... VsockPath: filepath.Sprintf-style join as today, rooted at vsockDir
	VsockPath: fmt.Sprintf("%s/sandboxes/%s/vsock.sock", vsockDir, sandboxID),
```

Then append to `delivery_test.go` (the fake must speak the Firecracker vsock-UDS preamble: `vsock.Connect` sends `CONNECT 52\n` and expects an `OK`-prefixed line, then newline-delimited JSON):

```go
// startFakeVsockAgent listens on sockPath, speaks the Firecracker vsock UDS
// preamble, then the JSON agent protocol, recording configure payloads.
func startFakeVsockAgent(t *testing.T, sockPath string) *recordedConfig {
	t.Helper()
	rec := &recordedConfig{}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				if !sc.Scan() { // "CONNECT 52"
					return
				}
				if _, err := c.Write([]byte("OK 52\n")); err != nil {
					return
				}
				for sc.Scan() {
					var req vsock.Request
					if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
						return
					}
					if req.Type == vsock.TypeConfigure {
						rec.mu.Lock()
						rec.got = req.Configure
						rec.mu.Unlock()
					}
					resp, _ := json.Marshal(vsock.Response{OK: true})
					if _, err := c.Write(append(resp, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return rec
}

type recordedConfig struct {
	mu  sync.Mutex
	got *vsock.ConfigureRequest
}

func TestForkDeliversConfigureToAgent(t *testing.T) {
	dir := t.TempDir()
	mock := fork.NewMockEngine()
	mock.ForkDelay = 0
	mock.VsockDir = dir
	engine := &kvmReportingEngine{MockEngine: mock}
	if err := engine.CreateTemplate("py", "py", 0); err != nil {
		t.Fatal(err)
	}
	// The mock will report this exact path for sandbox "sb-ok".
	rec := startFakeVsockAgent(t, filepath.Join(dir, "sandboxes", "sb-ok", "vsock.sock"))

	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	if _, err := srv.Fork(context.Background(), "py", "sb-ok",
		map[string]string{"SESSION": "abc"},
		map[string]string{"API_KEY": "v"}); err != nil {
		t.Fatalf("fork with reachable agent: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.got == nil || rec.got.Env["SESSION"] != "abc" || rec.got.Secrets["API_KEY"] != "v" {
		t.Fatalf("agent saw %+v", rec.got)
	}
}
```

(imports for the test file: `bufio`, `encoding/json`, `net`, `os`, `path/filepath`, `sync`, plus `internal/vsock`.)

- [ ] **Step 5: Run the full daemon suite**

Run: `go test ./internal/daemon/ -count=1 -race`
Expected: PASS, including the pre-existing gRPC lifecycle tests (they fork with env via the now-lenient mock path).

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/ internal/fork/mock.go internal/vsock/
git commit -m "feat: forkd delivers claim env+secrets to the guest, strict on real engines

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: live-fork default-deny (`allowSecretInheritance`)

**Files:**
- Modify: `api/v1alpha1/types.go` (SandboxForkSpec)
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`, `deploy/crds/agentrun.dev_sandboxforks.yaml`
- Modify: `internal/controller/sandboxfork_controller.go`
- Test: `internal/controller/fork_secrets_test.go` (new, package controller_test)

- [ ] **Step 1: Write the failing envtest** (`internal/controller/fork_secrets_test.go`)

```go
package controller_test

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
)

func TestLiveForkOfSecretHolderIsRejectedByDefault(t *testing.T) {
	// Source claim that *declares* secrets. Readiness is irrelevant: the
	// policy gate is spec-level and must fire before any forkd call.
	source := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-holder", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "nonexistent-pool"},
			Secrets: []v1alpha1.SecretMount{{
				Name:      "k",
				SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "K"},
			}},
		},
	}
	if err := k8sClient.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	forkObj := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{Name: "denied-fork", Namespace: "default"},
		Spec: v1alpha1.SandboxForkSpec{
			SourceRef: v1alpha1.LocalObjectReference{Name: "secret-holder"},
			Replicas:  1,
		},
	}
	if err := k8sClient.Create(ctx, forkObj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, forkObj)
		_ = k8sClient.Delete(ctx, source)
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxFork
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "denied-fork", Namespace: "default"}, &got); err == nil {
			if c := meta.FindStatusCondition(got.Status.Conditions, "Rejected"); c != nil {
				if c.Reason != "SecretInheritanceDenied" {
					t.Fatalf("reason = %q", c.Reason)
				}
				if got.Status.ReadyForks != 0 {
					t.Fatalf("forks were created despite rejection")
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("fork was not rejected within 10s")
}

func TestLiveForkOptInProceedsPastTheGate(t *testing.T) {
	source := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-holder-2", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "nonexistent-pool"},
			Secrets: []v1alpha1.SecretMount{{
				Name:      "k",
				SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "K"},
			}},
		},
	}
	if err := k8sClient.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	forkObj := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{Name: "optin-fork", Namespace: "default"},
		Spec: v1alpha1.SandboxForkSpec{
			SourceRef:               v1alpha1.LocalObjectReference{Name: "secret-holder-2"},
			Replicas:                1,
			AllowSecretInheritance:  true,
		},
	}
	if err := k8sClient.Create(ctx, forkObj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, forkObj)
		_ = k8sClient.Delete(ctx, source)
	})

	// The gate must record the audit condition and NOT reject. (The fork then
	// waits on source readiness, which never comes in this test; that's fine,
	// we are testing the gate, not the fork path.)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxFork
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "optin-fork", Namespace: "default"}, &got); err == nil {
			if meta.FindStatusCondition(got.Status.Conditions, "Rejected") != nil {
				t.Fatal("opt-in fork must not be rejected")
			}
			if c := meta.FindStatusCondition(got.Status.Conditions, "SecretInheritance"); c != nil {
				if c.Reason != "ExplicitOptIn" {
					t.Fatalf("reason = %q", c.Reason)
				}
				return // audit condition recorded, gate passed
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("audit condition not recorded within 10s")
}
```

- [ ] **Step 2: Run to verify failure**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestLiveFork -count=1`
Expected: compile FAIL (`AllowSecretInheritance` undefined).

- [ ] **Step 3: API field + regen**

In `api/v1alpha1/types.go`, `SandboxForkSpec`, after `PauseSource`:

```go
	// AllowSecretInheritance permits forking a sandbox whose claim holds
	// secrets. A live fork duplicates guest memory, including any delivered
	// secret values, into every fork. Default is to reject such forks; see
	// docs/fork-correctness.md §3. The long-term default is per-fork
	// credential reissue.
	AllowSecretInheritance bool `json:"allowSecretInheritance,omitempty"`
```

Regenerate:

```bash
~/go/bin/controller-gen object paths=./api/...
~/go/bin/controller-gen crd paths=./api/... output:crd:artifacts:config=deploy/crds
```

(or `make generate manifests` if controller-gen resolves on PATH.)

- [ ] **Step 4: The gate in `sandboxfork_controller.go`**

Right after fetching the source claim (BEFORE the readiness wait), insert:

```go
	// Live-fork secret gate: duplicating guest memory duplicates any
	// delivered secrets into every fork. Default-deny without explicit
	// opt-in. Spec-level check: fires regardless of source readiness.
	if len(source.Spec.Secrets) > 0 {
		now := metav1.Now()
		if !fork.Spec.AllowSecretInheritance {
			setCondition(&fork.Status.Conditions, metav1.Condition{
				Type:               "Rejected",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "SecretInheritanceDenied",
				Message:            "source claim holds secrets; set spec.allowSecretInheritance=true to fork it (forks duplicate guest memory, including secret values)",
			})
			if err := r.Status().Update(ctx, &fork); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil // terminal: no requeue
		}
		// Audit trail for the explicit opt-in.
		setCondition(&fork.Status.Conditions, metav1.Condition{
			Type:               "SecretInheritance",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "ExplicitOptIn",
			Message:            "fork inherits the source's in-memory secrets by explicit opt-in",
		})
		if err := r.Status().Update(ctx, &fork); err != nil {
			return ctrl.Result{}, err
		}
	}
```

Also add an early return at the very top of Reconcile, right after the Get, so a rejected fork stays terminal:

```go
	if meta.IsStatusConditionTrue(fork.Status.Conditions, "Rejected") {
		return ctrl.Result{}, nil
	}
```

(import `"k8s.io/apimachinery/pkg/api/meta"`.) NOTE: the status update inside the gate triggers a re-reconcile; the audit-condition branch must be idempotent; `setCondition` replaces in place, and the early-return covers the rejected branch.

- [ ] **Step 5: Run the tests**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -count=1 -race`
Expected: both new tests PASS; the whole suite stays green (the e2e fork test in `suite` uses sources without secrets; verify; if any pre-existing fork test uses secrets, set the opt-in there and note it).

- [ ] **Step 6: Commit**

```bash
git add api/v1alpha1/ deploy/crds/agentrun.dev_sandboxforks.yaml internal/controller/sandboxfork_controller.go internal/controller/fork_secrets_test.go
git commit -m "feat: live forks of secret-holding sandboxes require explicit opt-in

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: docs truth pass + full verification

**Files:**
- Modify: `docs/threat-model.md` (§6)
- Modify: `docs/fork-correctness.md` (§3 + status table)
- Modify: `ROADMAP.md` (§0 secrets item)

- [ ] **Step 1: Full verification**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./guest/agent/ ./cmd/test-agent/
golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m
go test ./internal/... -count=1 -race   # controller pkg needs: eval $(~/go/bin/setup-envtest use 1.31 -p env)
cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -q && cd ../..
```

All green required. STOP and report if not.

- [ ] **Step 2: Docs**

`docs/threat-model.md` §6, "Claim-time injection" row: update the Detail: delivery into the guest is now implemented over vsock post-restore (strict on real engines: undeliverable secrets fail the fork and reap the VM); plaintext wire transit note stays until mTLS (#4); status stays **partial** until the wire is encrypted.

`docs/fork-correctness.md`: §3: the default-deny rejection + opt-in audit trail are implemented (`sandboxfork_controller.go`), reissue remains open; delivery note (§3 paragraph about ForkOpts being dropped) updated to reflect reality. Status table row 3 → **partial** with "rejection + delivery done (envtest + KVM CI); reissue open". Row 1/2 unchanged.

`ROADMAP.md` §0: flip "Secrets delivered into the guest over vsock" to ✅ (note: wire encryption pending #4).

- [ ] **Step 3: Commit**

```bash
git add docs/threat-model.md docs/fork-correctness.md ROADMAP.md
git commit -m "docs: secrets delivery + live-fork gate reflected in threat model

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Out of scope (this PR)

- mTLS / API auth: issue #4 (next PR; removes the plaintext-transit caveat)
- Per-fork credential reissue: end state recorded in #7, follows #4's token machinery
- `ForkRunning` env delivery: forks inherit memory by design; reissue covers freshening
