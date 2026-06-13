/**
 * Direct-mode example: standalone sandbox-server, no Kubernetes required.
 *
 * Run a sandbox-server locally (cmd/sandbox-server) then execute this file
 * with ts-node or compile and run with node. It is kept runnable-shaped
 * (top-level async main) and type-checks under npm run check:examples.
 *
 * Wire shapes and error semantics are conformance-tested against a mock server
 * in test/server.test.ts and test/sandbox.test.ts.
 */

import { SandboxServer } from "@mitos/sdk";

async function main(): Promise<void> {
  // Point at the running sandbox-server. The default is http://localhost:8080.
  const server = new SandboxServer("http://localhost:8080");

  // List available templates (VM snapshots built by the server at startup).
  const templates = await server.listTemplates();
  console.log("templates:", templates.map((t) => t.id));

  // Fork a sandbox from the "python-3.12" template. When id is omitted a
  // random name is generated.
  const sandbox = await server.fork("python-3.12");
  console.log("sandbox id:", sandbox.id);

  // Execute a command. The per-sandbox bearer token (cluster mode only) is
  // never logged; direct mode is tokenless.
  const result = await sandbox.exec("python3 -c 'print(1 + 1)'");
  console.log("exit_code:", result.exitCode);
  console.log("stdout:", result.stdout.trim());

  // Write a file and read it back.
  await sandbox.files.write("/tmp/hello.txt", "hello from the TypeScript SDK\n");
  const content = await sandbox.files.read("/tmp/hello.txt");
  console.log("file content:", content.trim());

  // List a directory.
  const entries = await sandbox.files.list("/tmp");
  console.log(
    "entries:",
    entries.map((e) => `${e.name}${e.isDir ? "/" : ""}`),
  );

  // Tear the sandbox down (direct mode issues DELETE /v1/sandboxes/{id}).
  await sandbox.terminate();
  console.log("sandbox terminated");
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
