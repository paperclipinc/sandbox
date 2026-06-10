# agent-run Python SDK

Python client for [paperclipinc/sandbox](https://github.com/paperclipinc/sandbox):
snapshot-fork sandboxes for AI agents on Kubernetes.

Two modes:

- `agent_run.direct.SandboxServer`: talks to a standalone `sandbox-server`
  (no Kubernetes required). Works today.
- `agent_run.AgentRun` / `Sandbox`: drives the Kubernetes CRDs
  (`SandboxClaim`, `SandboxFork`) and execs through the forkd sandbox API.

```python
from agent_run.direct import SandboxServer

server = SandboxServer("http://localhost:8080")
server.create_template("python")
sandbox = server.fork("python")
print(sandbox.exec("print(1 + 1)").stdout)
sandbox.terminate()
```

Development:

```bash
pip install -e ".[dev]"
pytest tests/ -v
```

See the [repository README](https://github.com/paperclipinc/sandbox#readme)
for project status; this SDK is pre-alpha and its API may change.
