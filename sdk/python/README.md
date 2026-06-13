# mitos Python SDK

Python client for [paperclipinc/mitos](https://github.com/paperclipinc/mitos):
snapshot-fork sandboxes for AI agents on Kubernetes.

Two modes:

- `mitos.direct.SandboxServer`: talks to a standalone `sandbox-server`
  (no Kubernetes required). Works today.
- `mitos.AgentRun` / `Sandbox`: drives the Kubernetes CRDs
  (`SandboxClaim`, `SandboxFork`) and execs through the forkd sandbox API.

```python
from mitos.direct import SandboxServer

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

See the [repository README](https://github.com/paperclipinc/mitos#readme)
for project status; this SDK is pre-alpha and its API may change.
