import type { PaperclipPluginManifestV1 } from "@paperclipai/plugin-sdk";

const manifest: PaperclipPluginManifestV1 = {
  id: "paperclip.sandbox-provider",
  apiVersion: 1,
  version: "0.1.0",
  displayName: "Sandbox Provider (Firecracker)",
  description:
    "Self-hosted sandbox provider using paperclipinc/mitos. Sub-millisecond Firecracker microVM forking with per-volume fork policies.",
  author: "Paperclip",
  categories: ["automation"],
  capabilities: ["environment.drivers.register"],
  entrypoints: {
    worker: "./dist/worker.js",
  },
  environmentDrivers: [
    {
      driverKey: "sandbox",
      kind: "sandbox_provider",
      displayName: "Sandbox (Firecracker)",
      description:
        "Self-hosted sandboxes via paperclipinc/mitos. Firecracker microVMs with CoW forking (~0.8ms), volume fork policies, and k8s-native management.",
      configSchema: {
        type: "object",
        properties: {
          serverUrl: {
            type: "string",
            description:
              "URL of the sandbox-server or forkd HTTP API. Example: http://sandbox-server:8080",
          },
          template: {
            type: "string",
            description:
              "Template ID to fork sandboxes from. Must be pre-created on the server.",
            default: "default",
          },
          timeoutMs: {
            type: "number",
            description: "Timeout for sandbox operations in milliseconds.",
            default: 30000,
          },
          reuseLease: {
            type: "boolean",
            description:
              "Whether to keep the sandbox alive across runs instead of terminating on release.",
            default: false,
          },
        },
        required: ["serverUrl"],
      },
    },
  ],
};

export default manifest;
