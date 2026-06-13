# paperclipinc/mitos: API Specification v2

Supersedes v1. Same engine, same load-bearing rule, re-weighted for the three personas in priority order: **the application developer** (adoption is won here), **the agent itself** (the genuinely new user of 2026), **the platform operator** (governance lives here). The Kubernetes layer remains the implementation substrate and the operator's interface; it is no longer the cover page.

**The one rule, unchanged:** anything that creates, multiplies, or destroys infrastructure materializes as a declarative object through the API server; the imperative runtime surface only talks to live sandboxes. v2 adds one carefully shaped exception-that-isn't: capability-budgeted self-service (§3), where agent-initiated forks are still materialized as objects by the controller; the agent gets agency, the ledger stays complete.

**Vocabulary rule (new):** Kubernetes jargon never crosses into the developer or agent surfaces. `claim`, `husk`, `pool reconciliation` exist in chapter 5 and nowhere above it. Workspace verbs are deliberately git-shaped (`log`, `diff`, `revert`, `branch`) because every developer arrives with that mental model pre-installed and we are selling "git for computers."

---

## 1. The developer surface (primary)

### 1.1 Time-to-first-exec: under five minutes, no cluster required

```bash
# Local mode: kind + controller + a default pool, one command.
curl -fsSL https://get.mitos.run | sh
mitos dev up                      # laptop needs /dev/kvm; falls back to slow-mode with a warning
mitos run python -c "print('hello')"   # first exec. Done.
```

Against a real cluster, the same CLI targets it via kubeconfig. There is no step where a new user writes YAML, names a pool, or learns what a husk is. Defaults are lazy: the first `sandbox("python")` against a cluster with no matching pool creates a sensible default pool (admin-disableable) rather than erroring.

### 1.2 SDK (Python / TypeScript, conformance-tested parity)

The common path is three lines. Fork and lineage are the upgrade path, not a separate paradigm. Every SDK call maps 1:1 to a declarative object operation or a runtime RPC; no hidden magic; debugging the SDK is debugging the system.

```python
from mitos import AgentRun

c = AgentRun()                                   # local dev, kubeconfig, or in-cluster; autodetected

# ── Common path ────────────────────────────────────────────────
sb = c.sandbox("python")                         # ~ms from a warm pool
r = sb.exec("pytest -x", timeout=300)
print(r.stdout, r.exit_code)
sb.files.write("/workspace/notes.md", "# findings")
sb.terminate()

# ── Workspaces: the durable home, git verbs ────────────────────
sb = c.sandbox("python", workspace="proj-x")     # hydrates proj-x; dehydrates on terminate
ws = c.workspace("proj-x")
ws.log()                                         # revision DAG, newest first
ws.diff("rev-41", "rev-42")                      # changed paths + content hashes
ws.revert("rev-41")                              # bad memory consolidation? one call.

# ── Fork: try three approaches against shared warmed state ─────
attempts = sb.fork(3)                            # alias: sb.branch(3)
results = [f.exec(f"python fix.py --strategy {s}") for f, s in zip(attempts, "abc")]
best = max(zip(results, attempts), key=lambda p: score(p[0]))[1]
rev = best.terminate(outputs=["/workspace/dist", {"diff": True}])

# ── Lineage: resume the computer, not just the files ───────────
later = c.sandbox(from_revision=rev, resume="memory")    # files AND warm process state, ms

# ── Streaming / PTY ────────────────────────────────────────────
async for chunk in sb.exec_stream("npm run build"):
    print(chunk.stdout, end="")
term = sb.pty(); term.send("vim notes.md\n")
```

```typescript
import { AgentRun } from 'mitos';
const c = new AgentRun();
const sb = await c.sandbox('node', { workspace: 'proj-x' });
const { stdout } = await sb.exec('node build.js');
const forks = await sb.fork(2);
const rev = await sb.terminate({ outputs: ['/workspace/dist'] });
```

**Compatibility shims** (separate packages, same engine): `mitos-e2b` (drop-in `Sandbox` class) and an OpenAI code-interpreter-shaped HTTP endpoint, so LangChain/LlamaIndex/Vercel-AI users swap one import.

### 1.3 CLI verbs (flyctl-grade; kubectl never required)

```
mitos run <cmd>                  one-shot: sandbox → exec → terminate
mitos sandbox create|ls|exec|fork|terminate|top|ps|logs
mitos ws log|diff|revert|branch <workspace>
mitos pool ls|create|refresh     (operator-leaning, still available)
mitos dev up|down                local environment
```

Idempotency: every creating CLI/SDK call accepts/derives an idempotency key; agents and scripts retry aggressively and must never double-create.

---

## 2. The agent surface

Agents are first-class API consumers. Three commitments follow.

### 2.1 MCP server

`mitos-mcp` exposes the surface as tools, scoped by the token it is launched with: `sandbox_exec`, `sandbox_read_file`, `sandbox_write_file`, `sandbox_fork`, `sandbox_checkpoint`, `workspace_log`, `workspace_diff`, `workspace_revert`. Any MCP-speaking agent integrates with zero SDK work. Tool schemas are published and versioned; the server advertises its capability budget (§3) so orchestrators can reason about what their agents may do.

### 2.2 In-guest self-service endpoint

Inside every sandbox: `AGENTRUN_SOCKET=/run/mitos.sock` (vsock-backed), speaking the same runtime protocol with the sandbox's own attenuated token. The agent can checkpoint itself before a risky operation, fork itself for tree search, watch its own budget, and read its own vitals, without any network egress and without an external orchestrator round-trip.

```python
# from inside the sandbox: e.g. an agent doing best-of-N over its own state
import mitos.guest as me
ckpt = me.checkpoint(label="before-refactor")
forks = me.fork(3)                       # budget-gated; see §3
...
me.exit(outputs=["/workspace/result.json"])
```

### 2.3 LLM-legible errors (normative, enforced in review)

The primary reader of every runtime error is a language model. Every error carries: a stable machine `code`, a one-sentence `cause`, and a `remediation` the agent can act on, plus structured context. Never a bare code.

```json
{
  "code": "EGRESS_DENIED",
  "cause": "Connection to pypi.org:443 blocked: destination not in this sandbox's allowlist.",
  "remediation": "Install from the local toolchain cache (`pip install --index-url file:///cache/pip ...`) or ask your orchestrator to add pypi.org:443 to network.allow on the sandbox.",
  "context": { "destination": "pypi.org:443", "allowed": ["api.anthropic.com:443"], "cache": "/cache/pip" }
}
```

A CI lint rejects any error path lacking `remediation`. Docs ship `llms.txt`, the OpenAPI/proto schemas, and an examples corpus formatted for in-context learning; agents are a documentation audience, not an afterthought.

---

## 3. Capability budgets: agency with a ledger

Each sandbox carries a budget set by its creator (orchestrator, developer, or parent fork):

```yaml
budget:
  maxForks: 5                 # self-initiated forks (depth-aggregate)
  maxCheckpoints: 10
  maxCpuSeconds: 3600
  maxLifetimeExtension: 1h
  maxEgressBytes: 1Gi
```

Runtime `Fork()`/`Checkpoint()`/`ExtendLifetime()` are gated by it. Mechanically, a self-initiated fork is the controller materializing a real `Sandbox` object (owner-referenced to the parent); RBAC, quotas, Events, and the audit log see it exactly as if an operator had created it. Tokens are attenuated macaroon-style: a fork's token is strictly narrower than its parent's (budget minus spend, same-or-smaller scopes), and `secretInheritance: reissue` remains the default: each fork gets fresh credentials, never copies of the parent's. Budget exhaustion returns an LLM-legible error naming the orchestrator escalation path.

---

## 4. The runtime protocol (Connect)

One protocol, three transports: vsock (in-guest), cluster-internal, and browser (Connect's HTTP semantics let Paperclip's UI stream exec output with no proxy tier). AIP-aligned: standard list/filter/pagination, long-running operations for hydration and large transfers, field masks on reads.

```proto
service Sandbox {
  // Execution: the hot path
  rpc Exec(stream ExecRequest) returns (stream ExecResponse);     // cmd, env, cwd, pty; stdin/out/err chunks; exit code
  // Filesystem
  rpc ReadFile(Path) returns (stream Chunk);
  rpc WriteFile(stream WriteRequest) returns (WriteResult);
  rpc List(ListRequest) returns (ListResponse);                   // AIP-158 pagination
  rpc Stat(Path) returns (FileInfo);
  rpc Archive(ArchiveRequest) returns (stream Chunk);             // tar up/down
  rpc Watch(WatchRequest) returns (stream FsEvent);
  // Processes & network
  rpc Processes(Empty) returns (ProcessList);
  rpc Signal(SignalRequest) returns (Empty);
  rpc PortForward(stream Frame) returns (stream Frame);
  // Budget-gated self-service (§3): materialized declaratively by the controller
  rpc Fork(ForkRequest) returns (Operation);                      // → N sibling sandboxes
  rpc Checkpoint(CheckpointRequest) returns (Revision);           // live state → workspace revision
  rpc ExtendLifetime(ExtendRequest) returns (Lease);
  rpc Budget(Empty) returns (BudgetStatus);                       // remaining allowances
  // Telemetry
  rpc Vitals(Empty) returns (stream GuestVitals);                 // cpu steal, mem vs balloon, procs
}
```

Still absent on purpose: `Delete` of *other* sandboxes, pool mutation, workspace administration; those are creator-scope operations, declarative only. `me.exit()` terminates only the caller.

---

## 5. The declarative layer (three nouns, `mitos.run/v1alpha1`)

The operator's interface and the substrate everything above compiles to. **Pools prepare, Sandboxes run, Workspaces persist.** (v1's `SandboxTemplate` is inlined into the pool with an optional `templateRef` for reuse, the Deployment-embeds-PodSpec pattern; v1's `SandboxFork` is folded into `Sandbox` via `source.fromSandbox` + `replicas`, making fork and lineage the same concept, which in the engine they are.)

### SandboxPool

```yaml
apiVersion: mitos.run/v1alpha1
kind: SandboxPool
metadata: { name: python-agent }
spec:
  template:                                # inline (templateRef: optional alternative)
    image: ghcr.io/paperclipinc/agent-python:3.12
    init: ["pip install numpy pandas", "claude-code --version"]   # pool-build only; never per-sandbox
    resources: { cpu: "1", memory: 512Mi, balloon: true }
    volumes:
      - { name: workspace, size: 5Gi, forkPolicy: Snapshot }
      - { name: toolchain-cache, source: { nodeCache: default }, readOnly: true, forkPolicy: Share }
    network: { egress: deny, allow: ["api.anthropic.com:443"] }
    defaultBudget: { maxForks: 5, maxCheckpoints: 10 }
  snapshots: { replicasPerNode: 1, prefetch: full, refresh: { schedule: "0 4 * * *" } }
  warm: { min: 4, max: 100, targetPending: 0 }       # husk autoscaling
status:
  conditions: [...]                                   # SnapshotsReady, WarmReady, DistributionLag
```

### Sandbox

```yaml
apiVersion: mitos.run/v1alpha1
kind: Sandbox
metadata: { name: heartbeat-7f3a }
spec:
  source:                                  # exactly one of:
    poolRef: { name: python-agent }
    # fromSandbox: { name: heartbeat-7f3a }            # fork of a live sandbox
    # fromRevision: { workspace: proj-x, revision: rev-41 }   # lineage resume
  replicas: 1                              # >1 with fromSandbox = fan-out (indexed children)
  resume: memory                           # memory | filesystem (cross-principal handoff forces filesystem)
  workspaceRef: { name: proj-x }
  env: [{ name: SESSION_ID, value: abc-123 }]
  secrets: [{ name: anthropic, secretRef: { name: agent-secrets, key: ANTHROPIC_API_KEY } }]
  secretInheritance: reissue               # reissue (default) | inherit (requires source opt-in)
  network: { extraAllow: ["paperclip-bridge.agents.svc:8443"] }
  budget: { maxForks: 5, maxCpuSeconds: 3600 }
  lifetime:
    ttl: 2h
    idleTimeout: 15m
    onTerminate:
      snapshot: retain-last-3
      outputs:
        - { path: /workspace/report.md }
        - { diff: true }
        - { git: { remote: rendezvous, branch: "attempt/{{.name}}" } }
status:
  phase: Ready                             # Pending | Hydrating | Ready | Terminating | NodeLost | Failed
  endpoint: heartbeat-7f3a.agents.svc:8443
  pod: heartbeat-7f3a-husk                 # visible to kubectl, quotas, NetworkPolicy, OpenCost
  revision: rev-42                         # produced on terminate
  budgetSpend: { forks: 2, cpuSeconds: 1411 }
  startupLatencyMs: 4
  conditions: [...]
```

### Workspace

```yaml
apiVersion: mitos.run/v1alpha1
kind: Workspace
metadata: { name: proj-x }
spec:
  store: { objectStorageRef: default }     # S3-compatible, content-addressed chunks
  git: { paths: ["/workspace/repo"] }      # repo paths get history + the rendezvous remote
  retention: { revisions: 50, minAge: 7d }
  grants: [{ serviceAccount: agents/paperclip-instance, access: readwrite }]
status:
  head: rev-42
  revisions: 42
  resumable: true                          # head pairs with a memory snapshot
```

Conventions: typed conditions with `observedGeneration` and a published reason-code catalogue; owner references for GC (self-forked sandboxes owner-ref their parent); every data-plane action mirrored as a Kubernetes Event; finished sandboxes TTL'd from etcd.

---

## 6. Integration surfaces

- **agents.x-k8s.io facade**: the SIG kinds accepted verbatim, fulfilled by this engine (podTemplate → husk pods; pause/resume → memory snapshot/restore); vendored upstream e2e in CI; bridge annotation `mitos.run/pool` only.
- **Paperclip provider** (`@paperclipinc/plugin-sandbox`): provision→Sandbox, install-commands→pool init, lease→ttl/idle, teardown→terminate-with-outputs; honors `executionMode` enforcement.
- **kubectl plugin** (operator persona): `kubectl sandbox ps|top|logs|exec|tree <name>`; `tree` renders the fork/lineage DAG.
- **Eventing**: CloudEvents (`dev.mitos.workspace.revision.created`, `…sandbox.phase.changed`) over webhook/NATS for indexers (reference consumer: the turbovec-based CI indexer), billing, and dashboards; mirrored as Kubernetes Events on-cluster.

---

## 7. Cross-cutting

- **Auth**: declarative = K8s RBAC. Runtime = sandbox-scoped attenuated tokens (one sandbox per token; forks get strictly narrower tokens). Workspace grants are explicit, auditable objects.
- **Versioning**: CRDs follow the hermes-operator-grade written versioning + deprecation policy at v1; the Connect proto versions independently with a one-major-version compatibility window; SDKs pin proto versions; shims track their upstream's surface.
- **Docs**: every README/SDK example executed in CI; `llms.txt` + schemas published for agent consumption; the conditions/error-code catalogues are normative documents, not wiki pages.
- **Every number in this spec** (startup latency, budget accounting, feed lag) is benchmark-backed before it appears anywhere public.

## Appendix: what changed from v1 and why

1. **Presentation inverted**: SDK/CLI first, CRDs as chapter 5. The adoption persona never sees YAML; the operator persona lost nothing.
2. **Five nouns → three**: template inlined into pool; fork folded into `Sandbox.source.fromSandbox`+`replicas`. Fork and lineage are now visibly one concept.
3. **Budgeted self-service added**: runtime `Fork`/`Checkpoint`/`ExtendLifetime` behind per-sandbox budgets with attenuated tokens, materialized declaratively. The agent-as-user persona gains agency; the audit ledger stays complete.
4. **Git verbs on workspaces, K8s jargon quarantined, LLM-legible errors made normative, Connect over grpc-gateway, AIP conventions, idempotency keys, local dev mode, llms.txt.**
