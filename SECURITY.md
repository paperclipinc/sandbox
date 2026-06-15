# Security Policy

## Reporting a Vulnerability

Please do not file public issues for vulnerabilities.

- **Preferred**: GitHub private vulnerability reporting. Go to the Security tab of this repository and click "Report a vulnerability". MAINTAINER TODO: enable private vulnerability reporting in the repository Security settings if it is not already on.
- **Fallback**: email the security contact. MAINTAINER TODO: confirm or replace this address: `security@paperclip.inc` (placeholder; currently routes to `jannes@paperclip.inc`).

We will acknowledge your report within 72 hours and keep you informed of progress toward a fix and disclosure.

### Response targets

These are the targets we aim for on a valid, in-scope report; they are targets
for a pre-1.0 project run by a small team, not a contractual SLA.

- Acknowledge receipt: within 72 hours.
- Initial severity assessment and triage: within 7 days.
- Fix or mitigation for a critical guest-to-host escape or cross-sandbox leak:
  prioritized ahead of feature work; coordinated-disclosure timeline agreed
  with the reporter, default 90 days.
- Public disclosure: coordinated with the reporter after a fix ships, with
  credit unless the reporter requests anonymity.

## Supported Versions

This project is pre-1.0. Only the latest tagged release
(`ghcr.io/paperclipinc/mitos-*`) receives security fixes. There is no
backport window for older pre-1.0 tags: upgrade to the latest release to pick
up a security fix. The guest kernel we ship has its own currency process; see
[docs/kernel-cve.md](docs/kernel-cve.md).

| Version | Supported |
|---|---|
| Latest tagged release | Yes |
| Any older pre-1.0 tag | No |

## Scope

This project executes untrusted code in microVMs. The following are explicitly in scope:

- Guest-to-host escapes (Firecracker, vsock, forkd).
- Cross-sandbox leaks of any kind, including state leaking through snapshot forks.
- Snapshot integrity (tampering, substitution, restore of unverified snapshots).
- Secret handling (secrets appearing in logs, error messages, condition messages, or host paths).

## Verifying releases

Published images (`ghcr.io/paperclipinc/mitos-controller`,
`ghcr.io/paperclipinc/mitos-forkd`, `ghcr.io/paperclipinc/mitos-husk-stub`) are
signed with cosign in keyless mode using the publish workflow's GitHub OIDC
identity, and each carries an SPDX SBOM attestation. There is no long-lived
signing key. Verify the signature (replace `VERSION` with the release tag, for
example `v0.1.0`):

```bash
COSIGN_EXPERIMENTAL=1 cosign verify \
  --certificate-identity-regexp "https://github.com/paperclipinc/mitos/.github/workflows/publish.yaml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/paperclipinc/mitos-controller:VERSION
```

Verify the SBOM attestation:

```bash
COSIGN_EXPERIMENTAL=1 cosign verify-attestation \
  --type spdxjson \
  --certificate-identity-regexp "https://github.com/paperclipinc/mitos/.github/workflows/publish.yaml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/paperclipinc/mitos-controller:VERSION
```

A successful verify pins the signer to OUR publish workflow on OUR repository
and exits 0; a signature from any other identity fails. The full procedure,
including how to extract and read the SBOM, is in
[docs/supply-chain.md](docs/supply-chain.md). How we track and patch CVEs across
the guest kernel, Firecracker, and Go dependencies is in
[docs/security-operations.md](docs/security-operations.md).

## Code review policy for security-sensitive paths

Changes to the security-sensitive paths (`internal/fork`,
`internal/firecracker`, `internal/daemon`, `internal/vsock`, `guest/`, the
networking and egress paths `internal/netconf` and `internal/dnsproxy`, the
content store and husk path `internal/cas` and `internal/husk`, and the
key-material paths `internal/pki`, `internal/kms`, and `internal/storecrypt`)
require a named human reviewer before merge, enforced by `.github/CODEOWNERS`.
The full policy is in
[docs/security-review-policy.md](docs/security-review-policy.md).

## Current Status

This project has not yet had an external security review. The known threat surface and its per-row mitigation status are documented in [docs/threat-model.md](docs/threat-model.md). Read it before deploying to anything that matters.

## AI-Assisted Development Policy

Substantial portions of this codebase are AI-assisted. Security-sensitive paths
receive named-human review before merge: `internal/fork`,
`internal/firecracker`, `internal/daemon`, `internal/vsock`, `guest/`, and
future token/attenuation code. The policy is documented in
[docs/security-review-policy.md](docs/security-review-policy.md) and tracked in
issue #35.
