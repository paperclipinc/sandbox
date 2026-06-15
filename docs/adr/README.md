# Architecture Decision Records

This directory holds the Architecture Decision Records (ADRs) for mitos. An ADR
captures a significant, hard-to-reverse, or honesty-sensitive design decision:
the context that forced it, the decision itself, and the consequences that
follow. ADRs are how a residual or a design choice graduates from prose in
docs/threat-model.md, CLAUDE.md, or ROADMAP.md into a recorded, citable decision.

The decision to use ADRs at all is itself recorded as
[ADR 0000](0000-record-architecture-decisions.md).

## Format

Every ADR is a Markdown file matching the structure of the existing records
(0001 and 0002 are the reference shape):

- A title line: `# ADR NNNN: <short imperative title>`.
- A `Status:` line with the status and a date, e.g. `Status: accepted
  (2026-06-15)`.
- An `Issue:` line (and an optional `Related:`) tracing the decision to a GitHub
  issue and the grounding code, threat-model row, ROADMAP line, spec section, or
  plan under docs/superpowers/plans/.
- `## Context`: the forces and the problem that made a decision necessary.
- `## Decision`: what we decided, stated as a single load-bearing sentence in the
  heading where possible, then the detail.
- `## Consequences`: what becomes true, easier, harder, or constrained because of
  the decision, including the residuals it leaves open.
- Longer ADRs MAY add `## Why not <alternative>` and `## Status of the slice`
  sections (0001 and 0002 both do).

Every ADR decision must TRACE to something real: a line in the code, a row in
docs/threat-model.md, a rule in CLAUDE.md, a line in ROADMAP.md, or a plan under
docs/superpowers/plans/. This is the no-unverified-claims operating principle
(CLAUDE.md). An ADR that records a decision the code does not YET implement must
say so explicitly and cite the plan, the same way a threat-model row distinguishes
`mitigated` from `open`.

## Statuses

- `proposed`: under discussion, not yet adopted.
- `accepted`: adopted and in force.
- `superseded by ADR NNNN`: replaced by a later decision; the superseding ADR
  carries the new decision and this one is kept for history.
- `deprecated`: no longer in force, not directly replaced.

ADRs are append-only as a log. A reversal is a NEW ADR that supersedes the old
one; the old ADR's status line is updated to point at the superseding ADR, but
its body is never rewritten to hide the original decision.

## Numbering

ADRs are numbered with a zero-padded four-digit sequence in filename and title
(`NNNN-kebab-title.md`, `# ADR NNNN: ...`). The sequence is monotonic; a new ADR
takes the next number after the highest existing one. The meta-ADR that adopts
ADRs is 0000 here (not the conventional 0001) because numbers 0001 and 0002 were
already spent on substantive decisions before the framework was formalized, and
renumbering an accepted, cross-referenced ADR would break links in ROADMAP.md and
docs/facade-conformance.md. See ADR 0000 for the full numbering note.

## Index

| ADR | Title | Status | Records |
|---|---|---|---|
| [0000](0000-record-architecture-decisions.md) | Record architecture decisions | accepted | Adopting ADRs; the format, numbering, and the "residuals ship as ADRs" rule. |
| [0001](0001-facade-and-naming.md) | The agents.x-k8s.io facade and the our-vs-their naming collision | accepted | Implementing the upstream API via a separate vendored-types facade (`cmd/facade`), and deferring the `mitos.run` noun rename to the API v2 migration. |
| [0002](0002-workspace-not-csi.md) | The Workspace is a content-addressed artifact, not a CSI PersistentVolume | accepted | Modeling the Workspace as a content-addressed, versioned revision DAG over `internal/cas`, not a CSI PV. |
| [0003](0003-kvm-device-plugin-psa-exception.md) | The /dev/kvm device-plugin PSA exception | accepted | Why the unprivileged husk pod takes exactly two documented PSA-restricted exceptions to reach `/dev/kvm` via the device plugin. |
| [0004](0004-node-flat-snapshot-trust-domain.md) | Node-flat snapshot trust domain | accepted | Snapshots are content-addressed and node-shared; treat the whole cluster as one trust domain until per-tenant snapshot isolation lands. |
| [0005](0005-raw-forkd-not-multitenant.md) | raw-forkd is not for untrusted multi-tenant | accepted | The husk pod is the default tenant runner; raw-forkd is the opt-in privileged fallback, gated off by default. |
| [0006](0006-husk-netadmin-egress-firewall.md) | The husk-pod NET_ADMIN capability for in-pod egress firewalling | proposed | One scoped `NET_ADMIN` capability, in the pod's own netns, as the minimal control for default-deny husk egress plus a metadata block. |

## Residual ADRs and the compliance claim-language rule

The compliance & observability addendum (ROADMAP.md) requires that residuals ship
as ADRs in this directory. ADRs 0003 through 0006 are the first application: each
records a RESIDUAL design decision grounded in docs/threat-model.md. The
companion guardrail in [docs/compliance-claims.md](../compliance-claims.md)
codifies the honest-claim rule that these residuals back: mitos never claims to be
"fully Kubernetes conformant", permitted claim language is bounded to what a CI
job proves, and anything beyond that ships as a residual ADR here.
