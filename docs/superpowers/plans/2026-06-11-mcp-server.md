# MCP Server Implementation Plan (issue #27)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close issue #27. Ship `agentrun-mcp`, a Model Context Protocol server that exposes sandboxes as tools (`sandbox_create`, `sandbox_exec`, `sandbox_read_file`, `sandbox_write_file`, `sandbox_fork`, `sandbox_terminate`), so any MCP-speaking agent (Claude, and any MCP client) can drive sandboxes with zero SDK integration. Tools are scoped by the credentials the server is launched with, schemas are published and versioned, errors are LLM-legible (code, cause, remediation), and the protocol/dispatch is proven by a conformance test in standard CI (no KVM): a real MCP client drives the server over stdio against a fake backend.

**Honesty constraint:** the MCP server is a thin adapter over the existing sandbox data path (the forkd HTTP sandbox API and the CRD claim path), both already CI-proven. This PR proves the MCP layer (protocol handshake, tool schemas, dispatch, token scoping, error shape) against a fake backend; the underlying exec/fork against a real VM is already proven by the KVM CI. The docs say exactly that. Workspace tools (log/diff/revert) are deferred behind a flag because the Workspace primitive (#21) is not built; they are advertised only when explicitly enabled, and the README/docs say so.

**Architecture:** A new `internal/mcp` package implements the MCP server over stdio JSON-RPC 2.0 (initialize, tools/list, tools/call), defining the tool set and their JSON schemas, and dispatching each tool to a `SandboxBackend` interface (Create, Exec, ReadFile, WriteFile, Fork, Terminate). A real backend talks to the sandbox system (the standalone sandbox-server HTTP API for the simplest path, or the k8s CRD + forkd HTTP path), carrying the per-sandbox bearer token. A fake backend (in tests) returns canned results so the conformance test exercises the full protocol without a cluster. `cmd/agentrun-mcp` wires the real backend and runs the server on stdio.

**MCP library decision (Task 1, investigate):** prefer the official Go MCP SDK `github.com/modelcontextprotocol/go-sdk` IF its go.mod directive is <= 1.24 and it does not drag in a fragile dependency tree (we just pinned the toolchain to go 1.24; do not regress it). If the official SDK forces a higher Go version or heavy deps, implement the minimal MCP subset directly (JSON-RPC 2.0 over stdio: initialize, tools/list, tools/call, plus the standard error envelope). The subset is small and well-specified; implementing it avoids dependency risk and is fully testable. State the decision and confidence in the report.

**Context for the implementer:**
- The sandbox data path the backend wraps: the standalone `cmd/sandbox-server` HTTP API (POST /v1/exec, /v1/files/read|write, /v1/fork, etc., no k8s, supports --mock) is the simplest backend target; OR the k8s path (create a SandboxClaim, wait Ready, exec via the forkd HTTP API with the claim-owned bearer token, like the Python SDK `sdk/python/agent_run/sandbox.py`). Pick one for the real backend (sandbox-server HTTP is simpler; document the choice and note k8s backend as a follow-up if you pick HTTP).
- Per-sandbox bearer tokens: the HTTP sandbox API requires `Authorization: Bearer <token>` (PR #41); the backend must carry it.
- LLM-legible errors: issue #28 specifies code/cause/remediation; apply that shape to tool errors (an error result with a machine code, a one-sentence cause, and an actionable remediation).
- Conventions: CLAUDE.md authoritative. No em/en dashes. TDD. Explicit-path git add. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Lint darwin + GOOS=linux. Do not regress go 1.24. No secrets/tokens logged.

---

### Task 1: `internal/mcp` server core + tool definitions + SandboxBackend

**Files:** Create `internal/mcp/server.go`, `internal/mcp/tools.go`, `internal/mcp/backend.go`, `internal/mcp/jsonrpc.go` (if implementing the subset), and `_test.go`.

- [ ] Decide the MCP library (see the decision note above) and add the dep only if it passes the go-1.24 + clean-deps bar; else implement the JSON-RPC 2.0 stdio subset in `jsonrpc.go` (Request{jsonrpc,id,method,params}, Response{jsonrpc,id,result|error}, the initialize/tools-list/tools-call methods, stdio framing per the MCP spec which uses newline-delimited or Content-Length framing; implement the framing the spec mandates for stdio, which is newline-delimited JSON-RPC messages).
- [ ] `backend.go`: `type SandboxBackend interface { Create(ctx, pool string) (sandboxID string, err error); Exec(ctx, sandboxID, command string, timeoutSec int) (ExecResult, error); ReadFile(ctx, sandboxID, path string) (string, error); WriteFile(ctx, sandboxID, path, content string) error; Fork(ctx, sandboxID string, replicas int) (forkIDs []string, err error); Terminate(ctx, sandboxID string) error }` with `ExecResult{ExitCode int; Stdout, Stderr string}`. A `FakeBackend` recording calls and returning canned results (for tests).
- [ ] `tools.go`: define the six tools with names, descriptions, and JSON-Schema input schemas: `sandbox_create{pool}`, `sandbox_exec{sandbox, command, timeout_seconds?}`, `sandbox_read_file{sandbox, path}`, `sandbox_write_file{sandbox, path, content}`, `sandbox_fork{sandbox, replicas?}`, `sandbox_terminate{sandbox}`. A `ToolSchemaVersion` constant. Workspace tools defined but only registered when an `EnableWorkspaceTools` option is set (deferred, #21).
- [ ] `server.go`: `Server{backend SandboxBackend; opts}`. Handles initialize (returns serverInfo + capabilities advertising tools), tools/list (returns the tool schemas), tools/call (validates the arguments against the tool, dispatches to the backend, returns a tool result or an LLM-legible error: a structured error with code/cause/remediation in the tool result content). `Run(ctx, in io.Reader, out io.Writer)` drives the stdio loop. Errors from the backend map to tool-call error results (isError:true) with the code/cause/remediation tri(never a bare code); a malformed request maps to a JSON-RPC error.
- [ ] Tests: unit-test tool schema validity (each tool has a name, description, valid JSON schema), the tools/list output contains all six, and tools/call dispatches to the backend (FakeBackend records the call and the server returns its result). A bad tool name returns a tool error; a missing required arg returns an LLM-legible validation error.
- [ ] Commit `feat: internal/mcp server, tool definitions, SandboxBackend interface`.

### Task 2: conformance test driving the server as a real MCP client

**Files:** `internal/mcp/conformance_test.go`.

- [ ] A test that runs `Server.Run` against an in-process pipe (io.Pipe) with a FakeBackend, and on the other end speaks the MCP protocol as a CLIENT: send initialize, assert the server response (protocol version, capabilities, tools advertised); send tools/list, assert all six tools with their schemas; send tools/call for each tool (create, exec, read, write, fork, terminate) and assert the result matches the FakeBackend canned output and the backend recorded the right arguments; send a tools/call with an unknown tool and a tools/call with a missing required argument and assert LLM-legible errors (code/cause/remediation present). This proves the full protocol handshake + dispatch + error shape without a cluster or KVM, and runs in the standard go test suite (and thus go-test CI).
- [ ] Commit `test: MCP conformance, client drives the server end to end`.

### Task 3: real backend + `cmd/agentrun-mcp`

**Files:** Create `internal/mcp/httpbackend.go` (real backend over the sandbox-server HTTP API, or k8s), `cmd/agentrun-mcp/main.go`, tests.

- [ ] `httpbackend.go`: a `SandboxBackend` talking to the chosen real path. If the sandbox-server HTTP API: a client hitting POST /v1/fork (create a sandbox from a template), /v1/exec, /v1/files/read|write, /v1/sandboxes/{id} DELETE, carrying the bearer token from a launch-time credential. Token scoping: the server is launched with a token (env AGENTRUN_TOKEN or a flag) and a base URL (AGENTRUN_SERVER); all backend calls carry that token, so the MCP server can only do what the token authorizes. Document the scoping. (If k8s path chosen: create a SandboxClaim, read its `<claim>-sandbox-token` Secret, exec via forkd HTTP; more moving parts; note the choice.)
- [ ] Unit-test the HTTP backend against an httptest.Server asserting it sends the bearer token and the right request bodies and parses responses (no real cluster).
- [ ] `cmd/agentrun-mcp/main.go`: parse flags/env (server URL, token, --enable-workspace-tools off by default), construct the real backend, run `mcp.Server.Run(ctx, os.Stdin, os.Stdout)`. Log to stderr only (stdout is the MCP channel); never log the token.
- [ ] Commit `feat: agentrun-mcp binary with an HTTP sandbox backend`.

### Task 4: docs + README + ROADMAP + PR

**Files:** `docs/mcp.md` (new), `README.md` (MCP section pointer), `ROADMAP.md`, full verification.

- [ ] `docs/mcp.md`: what agentrun-mcp is, the tool set + schemas + the schema version, how to launch it (the credential/token scoping, the backend URL), an example MCP client config (Claude Desktop / a generic MCP client stanza pointing at the agentrun-mcp binary), what is PROVEN (protocol + dispatch + schemas + token scoping via the conformance test; underlying exec/fork via the KVM CI), and what is OPEN (workspace tools pending #21; the k8s-claim backend if HTTP was chosen; capability-budget advertisement pending #24; streaming exec pending the Connect protocol #23).
- [ ] README: add a short MCP section (or a bullet under SDKs) stating sandboxes are exposed as MCP tools via agentrun-mcp, pointing at docs/mcp.md. No unverified claims.
- [ ] ROADMAP section 7 (ergonomics): flip the MCP server line to done (tools + conformance test), noting workspace-tools and capability-budget advertisement as open.
- [ ] Full verification (build darwin + GOOS=linux, vet, lint both, all Go suites with envtest, Python suite, gofmt zero, dash grep zero, go 1.24 directive preserved, any new dep clean).
- [ ] Push `feat/mcp-server`, PR `MCP server: expose sandboxes as tools for any MCP agent` body Closes #27 (tools + conformance proven, workspace tools and capability budgets open), watch CI, dismiss guarded CodeQL alerts with justification if any, merge when green.

**Out of scope (follow-ups):** workspace tools (log/diff/revert) pending Workspace #21; capability-budget advertisement pending #24; streaming exec / PTY over MCP pending the Connect runtime protocol #23; the k8s-claim backend (if the HTTP backend was chosen for v1); resource/prompt MCP primitives (only tools in v1); SSE/HTTP MCP transport (stdio only in v1).
