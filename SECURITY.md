# Security Policy

## Reporting a Vulnerability

Please do not file public issues for vulnerabilities.

- **Preferred**: GitHub private vulnerability reporting. Go to the Security tab of this repository and click "Report a vulnerability".
- **Fallback**: email jannes@paperclip.inc.

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

Published images are signed with cosign (keyless, GitHub OIDC) and carry an
SPDX SBOM attestation. The exact `cosign verify` and `cosign verify-attestation`
commands are in [docs/supply-chain.md](docs/supply-chain.md).

## Code review policy for security-sensitive paths

Changes to the security-sensitive paths (`internal/fork`,
`internal/firecracker`, `internal/daemon`, `internal/vsock`, `guest/`) require
a named human reviewer before merge, enforced by `.github/CODEOWNERS`. The full
policy is in [docs/security-review-policy.md](docs/security-review-policy.md).

## Current Status

This project has not yet had an external security review. The known threat surface and its per-row mitigation status are documented in [docs/threat-model.md](docs/threat-model.md). Read it before deploying to anything that matters.

## AI-Assisted Development Policy

Substantial portions of this codebase are AI-assisted. Security-sensitive paths
receive named-human review before merge: `internal/fork`,
`internal/firecracker`, `internal/daemon`, `internal/vsock`, `guest/`, and
future token/attenuation code. The policy is documented in
[docs/security-review-policy.md](docs/security-review-policy.md) and tracked in
issue #35.
