# @mitos/sdk

TypeScript SDK for the mitos sandbox runtime. Snapshot-fork microVMs for
AI agents: fork from a pool in milliseconds, exec commands, read and write
files, then terminate. Targets Node 18+ with native fetch.

Two modes:

- **Direct mode** (`SandboxServer`): talks to a standalone `sandbox-server`
  (no Kubernetes required). No bearer token; the standalone server is tokenless
  by design.
- **Cluster mode** (`AgentRun`): drives the Kubernetes CRDs
  (`SandboxClaim`, `SandboxFork`) in the `mitos.run/v1alpha1` API group.
  Each sandbox gets a per-sandbox bearer token read from a Secret and sent
  as `Authorization: Bearer <token>`. The token value is never logged and is
  redacted from any error message surfaced to callers.

See the [Python SDK README](../python/README.md) for the Python-language
equivalent and the parity mapping below.

## Install

```
npm install @mitos/sdk
```

## Direct mode: SandboxServer

```typescript
import { SandboxServer } from "@mitos/sdk";

const server = new SandboxServer("http://localhost:8080");

// List VM snapshot templates the server has built.
const templates = await server.listTemplates();
console.log(templates.map((t) => t.id));

// Fork a sandbox from a template. id is optional; a random one is generated.
const sandbox = await server.fork("python-3.12");

// Execute a command.
const result = await sandbox.exec("python3 -c 'print(1 + 1)'");
console.log(result.exitCode, result.stdout.trim()); // 0  2

// Write and read a file (content is a UTF-8 string).
await sandbox.files.write("/tmp/hello.txt", "hello\n");
const content = await sandbox.files.read("/tmp/hello.txt");
console.log(content.trim()); // hello

// List directory entries.
const entries = await sandbox.files.list("/tmp");
console.log(entries.map((e) => e.name));

// Terminate issues DELETE /v1/sandboxes/{id}.
await sandbox.terminate();
```

Full runnable example: [`examples/direct.ts`](examples/direct.ts).

## Cluster mode: AgentRun

```typescript
import { AgentRun, KubeConfigApi } from "@mitos/sdk";

// KubeConfigApi loads ~/.kube/config by default. Pass { inCluster: true }
// inside a pod that has a service account.
const k8s = new KubeConfigApi();

const client = new AgentRun({ k8s, namespace: "default" });

// Create a sandbox from a pool. Blocks until the SandboxClaim is Ready.
const sandbox = await client.create("my-pool", {
  env: { MY_VAR: "hello" },
});

// Execute with a per-sandbox bearer token (never logged).
const result = await sandbox.exec("echo $MY_VAR", { timeoutSeconds: 10 });
console.log(result.stdout.trim()); // hello

// File operations are identical to direct mode.
await sandbox.files.write("/tmp/data.txt", "cluster mode\n");

// List sandboxes, optionally filtered by pool.
const all = await client.list("my-pool");
console.log(all.map((s) => s.name));

// Terminate deletes the SandboxClaim; the controller tears the VM down.
await sandbox.terminate();
```

Full runnable example: [`examples/cluster.ts`](examples/cluster.ts).

## Bearer token model

In cluster mode the controller creates a per-sandbox `Secret` named
`<claim-name>-sandbox-token` containing a `token` key. `AgentRun.create`
reads this secret into memory immediately after the claim reaches Ready. The
value:

- is sent as `Authorization: Bearer <token>` on every exec and file request
- is never written to any log, span, metric label, or error message
- is redacted from any server-error body that reflects it back (via `redact`)
- is not stored on disk by the SDK

Direct mode (`SandboxServer`) has no token: the standalone server exposes its
sandbox API with `AllowTokenless`.

## PROVEN vs. OPEN

**PROVEN in CI** (see the `typescript-sdk` and `firecracker-test` jobs in
`.github/workflows/ci.yaml` and `kvm-test.yaml`):

- The SDK speaks the correct wire protocol. Every public method is driven
  against a mock HTTP server in the test suite (`test/`) that reproduces the
  exact JSON shapes the real `forkd` and `sandbox-server` emit. 31 tests
  cover exec, files read/write/list, fork, terminate, auth, error handling,
  and cluster polling.
- The package type-checks (`tsc --noEmit`) and builds (`tsc`) cleanly under
  TypeScript 5.7 strict mode.
- The examples type-check under `tsconfig.examples.json` (separate check so
  dist is not polluted).
- `npm pack --dry-run` confirms the package assembles and ships only `dist/`.

**OPEN** (proven by the KVM CI of the underlying API, not the TS SDK itself):

- Real in-VM exec over vsock: the KVM CI (`kvm-test.yaml`) exercises the
  `forkd` exec and file paths end to end against a live Firecracker VM; the TS
  SDK drives the same HTTP API that Python and the CLI use.
- Pool snapshot readiness on a real cluster: requires the controller and forkd
  running on KVM-capable hardware.

**Not yet shipped:**

- npm registry publication (`npm publish`): tracked as a release follow-up
  once the API stabilizes past 0.1. No `latest` badge until then.

## Parity with the Python SDK

See [`sdk/python/README.md`](../python/README.md) for the Python SDK.

| Python (`mitos`)            | TypeScript (`@mitos/sdk`)        | Notes                               |
|---------------------------------|-------------------------------------|-------------------------------------|
| `SandboxServer(url)`            | `new SandboxServer(url)`            | direct mode                         |
| `server.create_template(id)`    | `server.createTemplate(id)`         | builds a VM snapshot                |
| `server.fork(template)`         | `server.fork(template)`             | returns a `Sandbox`                 |
| `server.list_sandboxes()`       | `server.listSandboxes()`            | active sandboxes on the server      |
| `AgentRun(namespace=...)`       | `new AgentRun({ k8s, namespace })`  | cluster mode                        |
| `client.create(pool)`           | `client.create(pool)`               | creates a `SandboxClaim`            |
| `client.list(pool)`             | `client.list(pool)`                 | lists `SandboxClaim`s               |
| `sandbox.exec(cmd)`             | `sandbox.exec(cmd)`                 | returns `ExecResult`                |
| `sandbox.exec_result.stdout`    | `result.stdout`                     | camelCase in TS                     |
| `sandbox.files.read(path)`      | `sandbox.files.read(path)`          | UTF-8 string                        |
| `sandbox.files.write(path, c)`  | `sandbox.files.write(path, c)`      | UTF-8 string                        |
| `sandbox.files.list(path)`      | `sandbox.files.list(path)`          | returns `FileInfo[]`                |
| `sandbox.terminate()`           | `sandbox.terminate()`               | tears down the VM                   |

Naming: Python uses `snake_case`; TypeScript uses `camelCase`. The wire
protocol (HTTP JSON shapes) is identical; both SDKs talk to the same
`forkd`/`sandbox-server` endpoints.

## Errors

All errors are `AgentRunError` instances with a machine-readable `code`, a
human-readable `errorCause`, and an actionable `remediation`:

```typescript
import { AgentRunError } from "@mitos/sdk";

try {
  await sandbox.exec("...");
} catch (err) {
  if (err instanceof AgentRunError) {
    console.error(err.code);        // e.g. "unauthorized"
    console.error(err.remediation); // actionable hint
  }
}
```

## Development

```bash
npm ci
npm run build   # tsc -> dist/
npm test        # vitest: 31 conformance tests
npm run lint    # tsc --noEmit + tsc --project tsconfig.examples.json
npm pack --dry-run
```
