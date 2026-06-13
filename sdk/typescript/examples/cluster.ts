/**
 * Cluster-mode example: Kubernetes CRDs via AgentRun.
 *
 * Requires a running cluster with the mitos controller and forkd, and a
 * kubeconfig that can reach it. Execute with ts-node or compile and run with
 * node. It is kept runnable-shaped (top-level async main) and type-checks
 * under npm run check:examples.
 *
 * The cluster client polls a SandboxClaim until Ready, reads the per-sandbox
 * bearer token from a Secret (never logged, redacted from errors), and hands
 * back a Sandbox bound to the claim endpoint. The claim/fork path is proven
 * in the kind CI smoke; real in-VM exec is proven by the KVM CI of the API.
 */

import { AgentRun, KubeConfigApi } from "@mitos/sdk";

async function main(): Promise<void> {
  // KubeConfigApi loads ~/.kube/config (or the in-cluster service account when
  // opts.inCluster is true). @kubernetes/client-node is lazy-loaded so direct
  // mode never pulls it in.
  const k8s = new KubeConfigApi();

  // AgentRun requires a K8sApi implementation. For tests pass a fake; for
  // production pass KubeConfigApi.
  const client = new AgentRun({ k8s, namespace: "default" });

  // Create a sandbox from a pool. The pool must be Ready in the cluster.
  // opts.name is optional; a random name is generated when omitted.
  const sandbox = await client.create("my-pool", {
    env: { MY_VAR: "hello" },
  });
  console.log("sandbox id:", sandbox.id);
  console.log("endpoint:", sandbox.endpoint);

  // Execute a command. The bearer token is read from the per-sandbox Secret
  // and sent as Authorization: Bearer <token>. It is never logged.
  const result = await sandbox.exec("echo $MY_VAR", { timeoutSeconds: 10 });
  console.log("exit_code:", result.exitCode);
  console.log("stdout:", result.stdout.trim());

  // File operations work the same as in direct mode.
  await sandbox.files.write("/tmp/data.txt", "written from cluster mode\n");
  const content = await sandbox.files.read("/tmp/data.txt");
  console.log("file content:", content.trim());

  // List all sandboxes in the namespace, optionally filtered by pool.
  const all = await client.list("my-pool");
  console.log("active sandboxes:", all.map((s) => s.name));

  // Terminate deletes the SandboxClaim; the controller tears the VM down.
  await sandbox.terminate();
  console.log("sandbox terminated");
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
