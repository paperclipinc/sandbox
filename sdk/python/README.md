# mitos Python SDK

Python client for [paperclipinc/mitos](https://github.com/paperclipinc/mitos):
snapshot-fork sandboxes for AI agents on Kubernetes.

Two modes:

- `mitos.AgentRun` / `Sandbox`: drives the Kubernetes CRDs
  (`SandboxClaim`, `SandboxFork`, `SandboxPool`, `SandboxTemplate`) and execs
  through the forkd sandbox API.
- `mitos.direct.SandboxServer`: talks to a standalone `sandbox-server`
  (no Kubernetes required).

## Cluster mode: the one-liner

```python
from mitos import AgentRun

c = AgentRun()                                   # kubeconfig or in-cluster; autodetected

sb = c.sandbox("python", ready=True)             # lazy default pool, waits Ready
print(sb.exec("python -c 'print(2 + 2)'").stdout)  # 4
sb.files.write("/workspace/notes.md", "# findings")
print(sb.files.read("/workspace/notes.md"))      # "# findings"
sb.terminate()
```

`c.sandbox("python")` ensures a deterministic default pool
`mitos-default-python` (a `SandboxTemplate` carrying the image plus a
`SandboxPool` that references it), creating both if absent. It is
admin-disableable with `AgentRun(allow_default_pool=False)`, which makes the
image path raise instead of creating anything.

### Explicit pool (never creates anything)

```python
sb = c.sandbox(pool="python-agent-pool")
```

### Fork a running sandbox

```python
forks = sb.fork(3)                               # 3 copies of the warmed state
for f in forks:
    print(f.exec("echo from-fork").stdout)
```

### Reconnect by name (durable handle across processes)

```python
sb = c.sandbox("python", name="agent-session-1", ready=True)
# ... later, in a different process:
sb = c.from_name("agent-session-1")
print(sb.exec("cat /workspace/notes.md").stdout)
```

### Readiness

```python
sb = c.sandbox("python").wait_until_ready()      # chainable; raises on Failed/timeout
```

### Streaming exec

```python
# Callbacks fire per chunk (bytes) as output arrives; the returned ExecResult
# still carries the full aggregate.
sb.exec("pytest -x", on_stdout=lambda b: print(b.decode(), end=""))

# A long-running background command with a handle.
bg = sb.exec_background("npm run dev")
# ... do other work ...
bg.kill()
```

### Structured errors

```python
from mitos import AgentRunError

try:
    sb.exec("false")
    sb.files.read("/does/not/exist")
except AgentRunError as e:
    print(e.code)         # e.g. file_failed, not_found
    print(e.remediation)  # an actionable next step
```

`AgentRunError` is parsed from the server envelope
`{error:{code, message, cause, remediation}}`. Any bearer token a misconfigured
server reflects into a body is redacted before it becomes the error cause.

### Async client (hot paths)

```python
import asyncio
from mitos import AsyncAgentRun

async def main():
    c = AsyncAgentRun()
    sb = await c.sandbox("python", ready=True)
    print((await sb.exec("echo async-hello")).stdout)
    await sb.files.write("/workspace/a.txt", "x")
    print(await sb.files.read("/workspace/a.txt"))
    forks = await sb.fork(2)
    for f in forks:
        await f.terminate()
    await sb.terminate()

asyncio.run(main())
```

`AsyncAgentRun` / `AsyncSandbox` cover the hot paths (exec blocking and
streaming, files, fork, terminate, wait_until_ready, from_name,
sandbox(image)). Pool and workspace administration are sync-only. If your build
includes the code interpreter (#102), `sb.run_code(...)` returns an `Execution`;
the async client does not yet wrap `run_code`.

## Direct mode (no Kubernetes)

```python
from mitos.direct import SandboxServer

server = SandboxServer("http://localhost:8080")
server.create_template("python")
sandbox = server.fork("python")
print(sandbox.exec("print(1 + 1)").stdout)
sandbox.terminate()
```

## What is proven where

The cluster examples (lazy default-pool creation, fork, from_name reconnect,
readiness, async hot paths) run against the real Firecracker engine in the KVM
CI job. The structured-error parsing and the wire shapes are unit-tested with
no cluster. No latency or throughput number is claimed in this README.

## Development

```bash
pip install -e ".[dev]"
pytest tests/ -v
```

See the [repository README](https://github.com/paperclipinc/mitos#readme)
for project status; this SDK is pre-alpha and its API may change.
