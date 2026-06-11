# MCP server (`agentrun-mcp`)

`agentrun-mcp` exposes the agentrun sandbox lifecycle as [Model Context
Protocol](https://modelcontextprotocol.io) tools. Any MCP-speaking agent
(Claude Desktop, an MCP client library, an agent framework with MCP support)
can create sandboxes, run commands, read and write files, fork, and terminate,
with no SDK integration.

It speaks MCP over a stdio JSON-RPC 2.0 transport: stdin and stdout are the
protocol channel, and all logging goes to stderr. The protocol subset
(`initialize`, `tools/list`, `tools/call` over newline-delimited JSON-RPC) is
implemented directly and is dependency free, so the binary stays on go 1.24.

## Tools

The server advertises six always-on tools. The tool input-schema contract is
versioned by `ToolSchemaVersion` (currently `1.0.0`); it is bumped whenever a
tool name, required field, or property semantic changes in a way clients must
observe.

| Tool | Required arguments | Optional arguments | Returns |
| --- | --- | --- | --- |
| `sandbox_create` | `pool` (string) | | The new sandbox id. |
| `sandbox_exec` | `sandbox` (string), `command` (string) | `timeout_seconds` (integer) | JSON `{exit_code, stdout, stderr}`. |
| `sandbox_read_file` | `sandbox` (string), `path` (string) | | The file contents. |
| `sandbox_write_file` | `sandbox` (string), `path` (string), `content` (string) | | A confirmation of bytes written. |
| `sandbox_fork` | `sandbox` (string) | `replicas` (integer) | JSON array of new sandbox ids. |
| `sandbox_terminate` | `sandbox` (string) | | A termination confirmation. |

`pool` maps to a sandbox-server template id in the HTTP backend (see below).

Tool failures are returned as LLM-legible tool results, not bare codes: each
carries a `code`, a one-sentence `cause`, and an actionable `remediation`
(the API v2 error rule, issue #28), with `isError` set so the client can
distinguish a tool failure from a transport error.

Workspace tools (`workspace_create`, `workspace_list`, `workspace_attach`,
`workspace_delete`) are advertised only when `--enable-workspace-tools` is set
and are not yet dispatched (issue #21).

## Launching it

The HTTP backend talks to a running [sandbox-server](../cmd/sandbox-server)
over its REST API.

```bash
agentrun-mcp \
  --server https://sandbox.example.internal \
  --token "$AGENTRUN_TOKEN" \
  --enable-workspace-tools=false
```

Flags and environment:

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--server` | `AGENTRUN_SERVER` | `http://localhost:8080` | Base URL of the sandbox-server. |
| `--token` | `AGENTRUN_TOKEN` | empty | Bearer token sent on every backend request. |
| `--enable-workspace-tools` | | `false` | Advertise the deferred workspace tools. |

### Token scoping

The launch-time token is the server's only credential. Every backend request
carries it as `Authorization: Bearer <token>`, so `agentrun-mcp` can do exactly
what that token authorizes on the sandbox-server and nothing more. Scope the
token to scope the agent.

The token is never logged and never placed in an error message. The backend
does not log at all; on a non-2xx response it surfaces the response body as
error context, but redacts any echo of the token first, so the secret cannot
escape through an error string. The startup log line reports only whether a
token is `set` or `unset`.

### Backend mapping and caveats

`agentrun-mcp` ships with the HTTP backend, the simplest real backend: one
sandbox-server process, plain HTTP, one token.

- `sandbox_create` issues `POST /v1/fork {template: <pool>, id: <generated>}`.
  The sandbox-server has no pools; it forks from a named template, so the
  HTTP backend treats `pool` as the template id. On a real Kubernetes
  deployment a pool is a `SandboxPool`; that mapping belongs to the future
  k8s-claim backend.
- `sandbox_fork` issues one `POST /v1/fork` per replica. The sandbox-server
  fork endpoint has no replicas parameter and its `template` field is a
  template lookup, so the source id must resolve there. True
  fork-of-a-running-sandbox is the k8s `SandboxFork` path and is a follow-up.
- `sandbox_exec` and the file tools require the bearer token on the
  sandbox-server (its exec/files routes are token-guarded); `fork` does not.

Sandbox ids are generated client-side with `crypto/rand`.

## Example MCP client config

A generic MCP client that launches servers as subprocesses points a command at
the binary and passes the backend URL and token through the environment:

```json
{
  "mcpServers": {
    "agentrun": {
      "command": "/usr/local/bin/agentrun-mcp",
      "env": {
        "AGENTRUN_SERVER": "https://sandbox.example.internal",
        "AGENTRUN_TOKEN": "your-scoped-token"
      }
    }
  }
}
```

## What is proven vs open

Proven:

- The MCP protocol handshake (`initialize`), `tools/list` schema advertisement,
  and `tools/call` dispatch, driven by a conformance test that acts as a real
  MCP client over the same wire framing, in the standard `go-test` CI job (no
  KVM required).
- LLM-legible tool errors (code, cause, remediation) on bad arguments,
  unknown tools, and backend failures, asserted by the same test.
- The token-scoped HTTP backend: each method sends the bearer token and the
  correct method, path, and body, parses the response, turns a non-2xx into an
  error, and never leaks the token, asserted against an `httptest` server.
- The underlying exec/fork against a real microVM via the existing KVM CI job
  (`firecracker-test`), which exercises the sandbox-server data path the HTTP
  backend wraps.

Open:

- Workspace tools (log/diff/revert/attach) pending Workspace (issue #21);
  advertised but not dispatched.
- The Kubernetes claim backend (create a `SandboxClaim`, read its token
  Secret, exec via forkd); the HTTP backend is the v1 path.
- Capability-budget advertisement pending issue #24.
- Streaming exec and PTY over MCP pending the Connect runtime protocol
  (issue #23).
- Transport: stdio only in v1; no SSE or HTTP MCP transport yet.
- MCP primitives: tools only in v1; no resources or prompts yet.
