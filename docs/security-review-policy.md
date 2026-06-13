# Security review policy

Substantial portions of this codebase are AI-assisted. Changes to the
security-sensitive paths below require a named human reviewer before merge, in
addition to CI. This policy is enforced mechanically by `.github/CODEOWNERS`
(GitHub requests the listed owner as a required reviewer on any PR touching a
matched path) and is a merge gate on `main` (branch protection: require review
from Code Owners).

## Paths in scope and why

| Path | Why it is security-sensitive |
|---|---|
| `internal/fork/` | Drives Firecracker snapshot/restore; a fork-correctness or restore bug is a cross-sandbox state leak or a host exposure. |
| `internal/firecracker/` | The Firecracker API client and VM lifecycle; the guest-to-host boundary lives here. |
| `internal/daemon/` | forkd: the privileged builder and the host sandbox API (exec and file traffic); the host-side trust boundary. |
| `internal/vsock/` | The host side of the guest channel; parses untrusted guest output as data. |
| `guest/` | The guest agent (PID 1 in the VM) and guest-side protocol; untrusted post-exec. |
| `docs/threat-model.md` | The authoritative isolation claims; a change here moves the stated security surface. |
| `docs/security-review-policy.md` | This policy itself. |
| `docs/kernel-cve.md` | The shipped-kernel currency posture. |
| `.github/workflows/publish.yaml` | Produces signed release artifacts; a change here changes provenance. |
| Future token/attenuation code | Capability minting and attenuation; add the path to CODEOWNERS when it lands. |

## What a reviewer checks

- The change does not widen the guest-to-host boundary (new host syscalls,
  new privileged operations, new trust placed in guest-controlled data).
- No secret value is logged, put in an error or condition message, or written
  to a host path (CLAUDE.md secrets rule); only keys and counts.
- Fork-correctness hazards are respected (RNG, clocks, secret inheritance);
  see docs/fork-correctness.md.
- The threat model is updated in the SAME PR when the surface moves
  (a new row, or a status change on an existing row).
- For a kernel or Firecracker version bump: the KVM workflow is green and
  docs/kernel-cve.md is updated to match.

## Relationship to CI

CI (govulncheck, Trivy, CodeQL, Scorecard, the KVM end-to-end suite) is
necessary but not sufficient for these paths: an automated scan does not catch
a logic-level boundary regression. The named-human review is the backstop.
This policy does not replace the disclosure process in SECURITY.md; it governs
inbound code changes, while SECURITY.md governs inbound vulnerability reports.
