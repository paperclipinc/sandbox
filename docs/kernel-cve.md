# Guest kernel currency and CVE watch

Mitos boots guest workloads on a Linux kernel we ship as `vmlinux`. A kernel
CVE in the guest is contained by the Firecracker/KVM boundary (a guest-kernel
bug is not automatically a host escape), but a guest-to-host escape chain often
starts in the guest kernel, so we track guest-kernel currency deliberately.

## What we ship

- Source: the Firecracker CI kernel bucket,
  `https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/<arch>/vmlinux-<version>`.
- Firecracker line: `v1.15` (the `FC_VERSION` set in the CI and KVM workflows).
- Pinned kernel version: `6.1.155` (x86_64 and aarch64).
- Where the pin lives: `.github/workflows/ci.yaml` and
  `.github/workflows/kvm-test.yaml`, the `KERNEL_VERSION=` line in the kernel
  staging step. The pin is an exact version, NOT a floating "latest in the
  v1.15 prefix": a bump is a reviewed diff to those two lines.

The kernel is a 6.1.x longterm (LTS) series; security fixes land on the 6.1.y
stable line. The Firecracker CI bucket republishes 6.1.y builds for the v1.15
line as upstream stable releases appear.

## CVE watch process

1. Subscribe to the linux-cve-announce list
   (https://lore.kernel.org/linux-cve-announce/) and to the kernel.org
   releases feed for the 6.1.y stable series
   (https://www.kernel.org/category/releases.html).
2. The Trivy image scan (`.github/workflows/ci.yaml`, the docker-build job)
   does NOT scan the guest kernel: the kernel is a node-staged artifact, not a
   package in the container images. Guest-kernel currency is tracked HERE, by
   version, not by an image scanner.
3. When a high-severity 6.1.y CVE relevant to the guest attack surface is
   announced, open a tracking issue referencing this doc.

## Re-pin procedure

1. Confirm a newer 6.1.y build exists in the Firecracker CI bucket:
   ```bash
   ARCH=x86_64
   curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/?prefix=firecracker-ci/v1.15/${ARCH}/vmlinux-" \
     | grep -oE "firecracker-ci/v1.15/${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]+" \
     | sort -V | tail -1
   ```
2. Update the `KERNEL_VERSION=` line in BOTH `.github/workflows/ci.yaml` and
   `.github/workflows/kvm-test.yaml`, and the "Pinned kernel version" line in
   this doc, to the new version.
3. The KVM test workflow (`.github/workflows/kvm-test.yaml`) boots, snapshots,
   and restores a VM on the new kernel and execs through the guest agent over
   vsock; a green KVM run is the gate that the new kernel works end to end.
4. The snapshot manifest records the producing kernel (see
   docs/snapshot-format.md, the snapshot version-compatibility contract,
   issue #32); kernel is informational in the compatibility check, so a kernel
   bump does not invalidate existing snapshots by itself, but a rebuild of
   pool template snapshots is the clean path after a kernel bump.

## Scope and limits

- This tracks the GUEST kernel only. The HOST kernel currency on bare-metal
  nodes is the operator's responsibility (Talos/OS update channel); see
  docs/platforms.
- We do not build the kernel from source today; we consume the Firecracker CI
  build. Building a custom hardened guest kernel is a future option, not a
  current claim.
