# Security operations: CVE tracking and patch pipeline

Mitos ships a guest Linux kernel (`vmlinux`) and runs on Firecracker, so it has
a real CVE and patch surface beyond its own Go code. This page is the single
index for how we track and patch vulnerabilities across the three surfaces:

1. the GUEST KERNEL we ship,
2. FIRECRACKER (the VMM binary and its pinned version), and
3. our Go DEPENDENCIES (the module graph).

It links the disclosure policy (SECURITY.md), the kernel currency doc
(docs/kernel-cve.md), and the supply-chain verification doc
(docs/supply-chain.md). Tracked in issue #35.

## What already exists

These controls are in place today; this doc records them and adds the watch
automation, it does not re-invent them.

| Control | Where | Surface it covers |
|---|---|---|
| govulncheck (call-graph reachable) | `.github/workflows/ci.yaml`, `govulncheck` job, BLOCKING on every PR | Go deps and our own code |
| Trivy image scan | `.github/workflows/ci.yaml`, docker-build job | OS packages inside the published images |
| CodeQL | `.github/workflows/codeql.yml` | Go code, static analysis |
| OpenSSF Scorecard | `.github/workflows/scorecard.yml` | Repo supply-chain posture |
| Dependabot | `.github/dependabot.yml` | gomod, github-actions, docker, pip, npm version bumps |
| cosign keyless sign + SPDX SBOM attest | `.github/workflows/publish.yaml` | Release artifact provenance (verify steps in docs/supply-chain.md) |
| Guest-kernel version pin and re-pin procedure | docs/kernel-cve.md | Guest kernel |
| Weekly CVE watch | `.github/workflows/cve-watch.yaml` | Re-runs govulncheck on a cron and surfaces the manual kernel/Firecracker watch step |

## Surface 1: the guest kernel

The guest kernel is a node-staged artifact, NOT a package inside the container
images, so the Trivy image scan does not see it. Guest-kernel currency is
tracked by version in docs/kernel-cve.md: the pinned `6.1.x` longterm version,
where the pin lives (`ci.yaml` and `kvm-test.yaml`), the subscribe-to-CVE-feed
watch step, and the re-pin procedure (bump the two `KERNEL_VERSION=` lines, run
the KVM end-to-end suite, rebuild pool template snapshots). See docs/kernel-cve.md
for the full process; this is the surface the image scanners cannot cover, so
it is deliberately a documented manual watch plus the weekly reminder issued by
cve-watch.yaml.

## Surface 2: Firecracker

- The Firecracker line is pinned by `FC_VERSION` in `.github/workflows/ci.yaml`
  and `.github/workflows/kvm-test.yaml`. A bump is a reviewed diff to those
  lines, gated by a green KVM run.
- Firecracker publishes security advisories as GitHub Security Advisories on
  `firecracker-microvm/firecracker` and release notes on each tag. There is no
  package-manager feed for the pinned VMM binary, so this is a manual watch:
  the weekly cve-watch.yaml job prints the pinned `FC_VERSION` and the advisory
  URL so a maintainer can compare against the latest advisory.
- When a Firecracker advisory affects the pinned line, open a tracking issue,
  bump `FC_VERSION` in both workflows, and require a green KVM run before merge.

## Surface 3: Go dependencies

- govulncheck is the BLOCKING gate (ci.yaml). It reports only advisories
  reachable from this module's call graph, so it is low-noise; a new finding
  fails CI and the fix is a dependency bump.
- Dependabot opens the routine bumps (`.github/dependabot.yml`, weekly,
  grouped, `chore` prefix).
- cve-watch.yaml re-runs govulncheck weekly so a newly published advisory
  against an already-merged dependency is caught even when no PR is open.

## Triage and patch flow

1. A finding arrives from one of: a govulncheck failure (PR or weekly), a
   Dependabot PR, the kernel CVE feed, a Firecracker advisory, or an inbound
   report under SECURITY.md.
2. Open or annotate a tracking issue. For an inbound vulnerability report,
   handle it privately per SECURITY.md first; do not open a public issue that
   describes an unfixed guest-to-host escape.
3. Assess severity against the threat model (docs/threat-model.md). A
   guest-to-host escape or cross-sandbox leak is prioritized ahead of feature
   work per SECURITY.md.
4. Patch: a dependency bump, a kernel re-pin (docs/kernel-cve.md), or a
   Firecracker bump. Security-sensitive code paths get named-human review
   (docs/security-review-policy.md, CODEOWNERS).
5. Release: tag, then the publish workflow signs and SBOM-attests the images;
   consumers verify with the commands in docs/supply-chain.md.

## Honest limits

- The kernel and Firecracker watch steps are partly MANUAL. There is no
  free, authoritative API that maps a pinned `vmlinux` build or a pinned
  Firecracker binary to a live CVE list, so cve-watch.yaml issues a weekly
  reminder and prints the pinned versions rather than faking a scan result.
- We consume the Firecracker CI kernel build; we do not build a hardened guest
  kernel from source today (docs/kernel-cve.md). That is a future option, not a
  current claim.
- No external security audit has been performed yet (SECURITY.md, Current
  Status).
