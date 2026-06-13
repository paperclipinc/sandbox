# TypeScript SDK Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `@mitos/sdk`, a TypeScript SDK at parity with the Python SDK, so the dominant agent-tooling ecosystem (TS/JS) is a first-class client. It exposes the same surface: a `Sandbox` (exec, fork, terminate, file read/write/list) over the forkd HTTP sandbox API with the per-sandbox bearer token; a `SandboxServer` direct client (the standalone sandbox-server: createTemplate, fork, list); and an `AgentRun` cluster client (create a SandboxClaim, wait Ready, return a Sandbox), mirroring `sdk/python/mitos`. Verified in CI by a TypeScript build, a unit/conformance test suite that drives the client against a mock HTTP server reproducing the sandbox-server and forkd API request/response shapes (no cluster, no KVM), and README examples that type-check. The conformance tests assert the same wire contract the Python SDK, MCP HTTPBackend, and mitos CLI already use, so the two SDKs stay in lockstep.

**Honesty constraint:** the TS SDK is a client over the existing CI-proven data path; this PR proves the SDK speaks the correct wire protocol (request shapes, bearer auth, response parsing, error mapping) against a mock server and type-checks its examples. The actual exec/fork against a real VM is proven by the existing KVM CI. The README states this split. No published-package claims (npm publish) until the package is reviewed; this PR ships the source + CI + a local pack check, with publishing as a release follow-up.

**Architecture:** `sdk/typescript/` is a standalone npm package (`@mitos/sdk`). `src/types.ts` mirrors the Python types (ForkPolicy, SandboxPhase, ExecResult, FileInfo, SandboxInfo, PoolStatus, ForkInfo). `src/http.ts` is a small fetch-based transport carrying the bearer token and mapping non-2xx to typed errors (LLM-legible: code/cause/remediation spirit), never logging the token. `src/sandbox.ts` is the `Sandbox` class (exec/fork/terminate + a `files` accessor) over the forkd HTTP sandbox API. `src/server.ts` is `SandboxServer` (direct sandbox-server mode). `src/client.ts` is `AgentRun` (cluster mode) using `@kubernetes/client-node` to create a SandboxClaim, poll to Ready, read the `<claim>-sandbox-token` Secret, and return a `Sandbox`; the k8s calls go through a thin interface so the claim-building, wait-Ready, and token-fetch logic are unit-tested with a fake k8s API (no live cluster). Build with `tsc` (ESM + types; CJS optional via a dual build), test with `vitest`, both pinned.

**Context for the implementer:**
- Python SDK parity reference: `sdk/python/mitos/{types.py,sandbox.py,direct.py,client.py}` (ForkPolicy/phases; Sandbox.exec(command,timeout)/fork(n,pause_source)/terminate/files.read|write|list; SandboxServer.create_template/fork/list_templates/list_sandboxes; AgentRun.create(pool...)/list(pool)). Match method names/semantics where idiomatic for TS (camelCase).
- The wire shapes: the forkd HTTP sandbox API (`internal/daemon/sandbox_api.go`): POST /v1/exec {sandbox, command, timeout} -> {exit_code, stdout, stderr, exec_time_ms}; POST /v1/files/read {sandbox, path} -> {content, size}; POST /v1/files/write {sandbox, path, content, mode} -> {status}; the bearer token in `Authorization: Bearer <token>`. The sandbox-server (`cmd/sandbox-server`): POST /v1/fork {template, id}, /v1/templates, /v1/sandboxes, DELETE /v1/sandboxes/{id}. The MCP `internal/mcp/httpbackend.go` and the mitos `internal/agentcli/clusterbackend.go` show the exact request/response JSON the SDK must match.
- Cluster claim path: `sdk/python/mitos/sandbox.py` + `client.py` show creating a SandboxClaim CRD (group mitos.run/v1alpha1), polling status to Ready, reading the token Secret, and the endpoint. The CRD shapes are in `api/v1alpha1/types.go`.
- Repo CI: `.github/workflows/ci.yaml` has a `python-test` job; add a parallel `typescript-sdk` job (Node setup, npm ci, build, test, lint).
- Conventions: CLAUDE.md authoritative. No em/en dashes anywhere (TS, JSON, Markdown). No secrets/tokens logged. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Explicit-path git add. The TS SDK is its own toolchain; the Go lint rules do not apply to it, but the no-dash rule and no-token-logging rule do.

---

### Task 1: package scaffold, types, HTTP transport, Sandbox surface

**Files:** `sdk/typescript/{package.json,tsconfig.json,vitest.config.ts,.gitignore}`, `sdk/typescript/src/{index.ts,types.ts,http.ts,sandbox.ts,errors.ts}`, `sdk/typescript/test/{http.test.ts,sandbox.test.ts}`.

- [ ] Scaffold: `package.json` name `@mitos/sdk`, type module, a `build` (tsc), `test` (vitest), `lint` (tsc --noEmit or a light eslint) script, pinned devDeps (typescript, vitest, @types/node), Node engines >=18 (native fetch). `tsconfig.json` strict, ES2022, declaration output. `.gitignore` node_modules/dist.
- [ ] `types.ts`: ForkPolicy (enum/union Fresh|Share|Snapshot|Clone), SandboxPhase, ExecResult{exitCode, stdout, stderr, execTimeMs?}, FileInfo, SandboxInfo, PoolStatus, ForkInfo, matching the Python semantics.
- [ ] `errors.ts`: an `AgentRunError` with a machine `code`, `cause`, `remediation` (LLM-legible spirit), constructed from a non-2xx response (status + body), and a token-redaction helper so a token echoed in an error body is never surfaced.
- [ ] `http.ts`: a `HttpClient` wrapping fetch with a baseUrl + optional bearer token; `post(path, body)` / `del(path)` that set `Authorization: Bearer <token>` when present, parse JSON, and throw `AgentRunError` on non-2xx (redacting any token). Never log the token. A sandbox-id validation helper mirroring the server allowlist (reject `/`, `..`).
- [ ] `sandbox.ts`: `Sandbox` with `exec(command, opts?: {timeoutSeconds?})` -> ExecResult (POST /v1/exec); a `files` accessor with `read(path)`/`write(path, content, opts?)`/`list(path?)` (POST /v1/files/*); `fork(n?)` -> Sandbox[] (POST /v1/fork or the cluster fork, per the constructed mode); `terminate()`. The Sandbox holds {id, endpoint, token, http}.
- [ ] Tests (vitest, a mock HTTP server via a fetch mock or `http.createServer` on :0): exec sends {sandbox, command, timeout} with the bearer header and parses {exit_code,...} to ExecResult; files read/write/list send the right bodies and parse responses; a non-2xx yields an AgentRunError with code/cause/remediation; a token echoed in an error body is redacted; an unsafe sandbox id is rejected client-side. No cluster.
- [ ] Commit `feat: TypeScript SDK package, types, HTTP transport, Sandbox surface`.

### Task 2: SandboxServer (direct) and AgentRun (cluster) clients

**Files:** `sdk/typescript/src/{server.ts,client.ts,k8s.ts}`, `sdk/typescript/test/{server.test.ts,client.test.ts}`.

- [ ] `server.ts`: `SandboxServer(url)` direct client: `listTemplates()`, `createTemplate(id, opts?)`, `fork(template, id?)` -> Sandbox (POST /v1/fork, returns a Sandbox bound to the returned endpoint/id; direct mode may be tokenless or carry a token per the server config), `listSandboxes()`. Tests against a mock HTTP server reproducing the sandbox-server shapes.
- [ ] `k8s.ts`: a thin `K8sApi` interface (createClaim, getClaim, deleteClaim, readSecret) so the cluster logic is testable with a fake; the real impl uses `@kubernetes/client-node` (add as a dependency, pinned; it is the standard k8s client and is the one heavy dep the cluster mode needs). Document that direct mode needs no k8s dep (tree-shakeable / separate entry).
- [ ] `client.ts`: `AgentRun(opts?)` cluster client: `create(pool, opts?)` creates a SandboxClaim (mitos.run/v1alpha1) in the namespace, polls getClaim until Ready (bounded timeout), reads the `<claim>-sandbox-token` Secret + the status endpoint, returns a `Sandbox`; `list(pool?)` lists claims -> SandboxInfo[]. The k8s calls go through the K8sApi interface.
- [ ] Tests: SandboxServer against the mock HTTP server (fork returns a usable Sandbox; the exec then round-trips). AgentRun.create against a FAKE K8sApi (claim created with the right spec; poll returns Ready after N calls; the token Secret is read and the returned Sandbox carries the endpoint+token; a never-Ready claim times out with a clear error; the token never appears in logs). AgentRun.list maps claims to SandboxInfo.
- [ ] Commit `feat: SandboxServer and cluster AgentRun TypeScript clients`.

### Task 3: CI job, README with runnable examples, parity

**Files:** `.github/workflows/ci.yaml` (a `typescript-sdk` job), `sdk/typescript/README.md`, `sdk/typescript/examples/` (a couple of typed examples).

- [ ] CI `typescript-sdk` job: Node 20 setup, `cd sdk/typescript && npm ci && npm run build && npm test && npm run lint` (tsc --noEmit for type-check). Also `npm pack --dry-run` to confirm the package assembles. Gate on all passing. Keep it fast.
- [ ] `sdk/typescript/README.md`: install, the two modes (direct SandboxServer; cluster AgentRun), exec/files/fork examples (TypeScript), the bearer-token model, and a clear PROVEN (the SDK speaks the correct wire protocol against a mock server + type-checks, in CI) vs the real-exec-proven-by-KVM-CI split. Cross-link the Python SDK README so the parity is visible. No unverified claims; no npm-published badge until published.
- [ ] `sdk/typescript/examples/*.ts`: a direct-mode example (fork from a template, exec, read a file) and a cluster-mode example (create from a pool, exec), type-checked by the build. Examples use the public API only.
- [ ] A short parity note (in the README or a PARITY.md): the method-by-method mapping to the Python SDK so they do not drift.
- [ ] Commit `ci: build and test the TypeScript SDK; README and examples`.

### Task 4: verification + ROADMAP + PR

- [ ] Full verification:
```bash
cd sdk/typescript && npm ci && npm run build && npm test && npm run lint && npm pack --dry-run && cd ../..
# repo-wide gates unaffected by TS, but run the dash + go checks to be safe:
grep -rlP '\x{2014}|\x{2013}' sdk/typescript --include='*.ts' --include='*.json' --include='*.md' | wc -l   # 0
go build ./... && echo "go unaffected"
python3 -c "import yaml; list(yaml.safe_load_all(open('.github/workflows/ci.yaml'))); print('ci yaml ok')"
git status --short
```
- [ ] ROADMAP: add/flip the TypeScript SDK line under ergonomics/SDKs to done (parity surface, conformance-tested, CI-built), with npm publishing noted as a release follow-up.
- [ ] Push `feat/typescript-sdk`, PR `TypeScript SDK at parity with the Python SDK` body `## Summary` bullets (@mitos/sdk: Sandbox exec/fork/terminate/files over the forkd HTTP API with bearer auth; SandboxServer direct mode; AgentRun cluster mode over a mockable k8s interface; token never logged, redacted from errors; conformance tests drive the client against a mock server reproducing the same wire shapes the Python SDK/MCP/CLI use; CI builds + tests + type-checks the package; real exec proven by the KVM CI; npm publish is a release follow-up), `## Test plan` checklist, `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. No em/en dashes. Do NOT merge (controller watches CI and merges).

**Out of scope (follow-ups):** npm publishing + a release workflow (provenance, the @mitos org); streaming exec / PTY (the Connect protocol #23); the MCP-over-TS client; browser/edge runtime support (the SDK targets Node 18+ with native fetch); a generated-from-OpenAPI client (this is a hand-written idiomatic SDK); deno/bun matrices.
