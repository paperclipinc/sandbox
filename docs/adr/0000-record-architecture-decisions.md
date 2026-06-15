# ADR 0000: record architecture decisions

Status: accepted (2026-06-15)
Issue: #30 (ADR framework and residual ADRs). Related: docs/adr/README.md, the
compliance & observability addendum in ROADMAP.md ("residuals ship as ADRs in
`docs/adr/`").

## Context

mitos already carried load-bearing design decisions in prose: the
`agents.x-k8s.io` facade and the our-vs-their naming collision (recorded ad hoc
as ADR 0001), the Workspace-not-CSI choice (ADR 0002), and a set of RESIDUAL
security decisions documented only inside docs/threat-model.md and CLAUDE.md
(the `/dev/kvm` device-plugin PSA exception, the node-flat snapshot trust domain,
raw-forkd not being a multi-tenant runner, the husk-pod egress control). The
compliance & observability addendum (ROADMAP.md) names "residuals ship as ADRs
in `docs/adr/`" as a hard rule, and lists specific residuals that must be
captured: the kvm device exception, the guest boundary, Workspace-not-CSI, and
the forkd control channel mirrored into Kubernetes Events.

Two problems followed from having no adopted framework:

1. There was a directory (`docs/adr/`) and a numbering scheme (0001, 0002) but no
   recorded decision to USE ADRs, no stated format, and no index. A contributor
   had to reverse-engineer the format from the two existing files.
2. The compliance addendum's "residuals ship as ADRs" rule had no home: nothing
   said what an ADR is, where the index lives, or how a residual graduates from a
   threat-model row into a recorded decision.

## Decision: adopt Architecture Decision Records

We adopt ADRs (in the Nygard sense) as the way mitos records significant,
hard-to-reverse, or honesty-sensitive design decisions. The mechanics:

- ADRs live in `docs/adr/` as Markdown, one file per decision, named
  `NNNN-kebab-title.md` with a zero-padded four-digit sequence.
- The format is fixed: a title line (`# ADR NNNN: <title>`), a `Status` line
  with a date, an `Issue`/`Related` line that traces the decision to a GitHub
  issue and the grounding code or doc, then the sections `## Context`,
  `## Decision`, and `## Consequences`. Longer ADRs may add `## Why not <X>` and
  `## Status of the slice` sections, as ADRs 0001 and 0002 already do. This
  matches the structure of the two pre-existing ADRs exactly, so no rewrite of
  0001 or 0002 is needed.
- The index of all ADRs lives in `docs/adr/README.md`, which also states this
  format and the numbering rule. The README is the entry point.
- Statuses are `proposed`, `accepted`, `superseded by ADR NNNN`, or
  `deprecated`. An ADR is never edited to reverse its decision; a reversal is a
  NEW ADR that supersedes the old one, and the old one's status line is updated
  to point at the superseding ADR. This preserves the decision history.
- Every ADR decision must TRACE to something real: a line in the code, a row in
  docs/threat-model.md, a rule in CLAUDE.md, or a plan in
  docs/superpowers/plans/. mitos has a no-unverified-claims operating principle
  (CLAUDE.md, operating principle 1); an ADR that asserts a behavior the code
  does not yet implement must say so explicitly and cite the plan, exactly as a
  threat-model row distinguishes mitigated from open.

### Numbering note

The sequence began at 0001 before this framework was adopted: ADR 0001
(`0001-facade-and-naming.md`) and ADR 0002 (`0002-workspace-not-csi.md`) already
existed as real decisions. This meta-ADR therefore takes number 0000 rather than
displacing a recorded decision. New ADRs continue the sequence from the highest
existing number. The standard "record architecture decisions" meta-ADR is
conventionally 0001 in a greenfield log; here it is 0000 because 0001 was already
spent on a substantive decision, and renumbering an accepted ADR would break the
cross-references in ROADMAP.md, docs/facade-conformance.md, and elsewhere.

## Consequences

- A contributor adding a significant decision adds an ADR under `docs/adr/`,
  appends it to the index in `docs/adr/README.md`, and links it from the
  grounding doc (the threat-model row, the ROADMAP line, the spec section).
- A residual named in the threat model or the compliance addendum graduates to a
  recorded decision by becoming an ADR, not by staying prose. The four residual
  ADRs 0003 through 0006 are the first application of this rule.
- The compliance claim-language guardrail (docs/compliance-claims.md) leans on
  this: a claim that exceeds what CI proves is permitted ONLY as a documented
  residual, and a documented residual means an ADR. ADRs are therefore the
  escape hatch for the honest-claim rule, not an exception to it.
- ADRs are append-only as a log: superseding rather than rewriting keeps the
  history of why a decision changed, which the security-sensitive paths
  (internal/fork, internal/firecracker, internal/daemon, guest/agent) need for
  review.
