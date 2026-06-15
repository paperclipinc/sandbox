# Compliance claim-language guardrails

This document codifies the honest-claim rule for Kubernetes conformance and
compliance language in mitos. It exists so a contributor can check a claim (in the
README, docs, a CRD description, release notes, a blog post, or a slide) against a
single reusable standard before it ships. It is the compliance counterpart to the
no-unverified-claims operating principle (CLAUDE.md, operating principle 1) and
the honest-Kubernetes-semantics principle (operating principle 3).

It implements the compliance & observability addendum in ROADMAP.md: "permitted
claim language is limited to what a CI job proves ...; never 'fully Kubernetes
conformant'. Residuals ship as ADRs in `docs/adr/`."

## The rule

> mitos NEVER claims to be "fully Kubernetes conformant". Permitted compliance
> claim language is bounded to exactly what a CI job proves. Anything beyond that
> ships as a residual ADR in `docs/adr/`, not as a claim.

A claim is permitted only if BOTH hold:

1. A CI job (a required check, or a gated KVM/cluster job named in the doc) proves
   the specific fact, OR the claim links to the ADR that records the residual.
2. The claim does not imply a pod-scoped Kubernetes mechanism governs sandbox VMs
   beyond what is actually wired (honest-Kubernetes-semantics).

If neither holds, the claim is rewritten or removed.

## Forbidden language

Do not write, in any artifact (source, comments, docs, YAML, CRD descriptions,
Markdown, commit messages, PR descriptions, the GitHub repo description, release
notes, marketing):

- "fully Kubernetes conformant" / "fully conformant" / "100% conformant".
- "production-ready" or "safe for untrusted code" as an unqualified claim. The
  threat model's honest summary is the opposite today (docs/threat-model.md: "do
  not run untrusted code with this project in production yet"), and a "1.0" or
  "production-ready" claim requires an EXTERNAL security review that has not
  happened (docs/threat-model.md review gate).
- "PSA restricted, no exceptions" for the husk pod. The husk pod takes EXACTLY
  the documented `/dev/kvm` device-plugin exceptions (ADR 0003); the count must be
  stated accurately, never as zero.
- "NetworkPolicy enforces sandbox egress" on the husk default path. The product
  creates no NetworkPolicy and ships no husk egress enforcement today
  (docs/threat-model.md section 0 surface 5, section 4; ADR 0006 records the
  planned control as `proposed`).
- "namespaces isolate tenants" around snapshots or VMs. A namespace buys RBAC on
  the CRDs only; snapshots are node-flat and the cluster is one trust domain today
  (ADR 0004, docs/threat-model.md section 7).
- "Sandboxes are pods" or any phrasing implying pod-scoped NetworkPolicy,
  ResourceQuota, or PodSecurity govern sandbox VMs in raw-forkd mode (sandboxes
  are not pods there; honest-Kubernetes-semantics).

## Permitted language (each bounded to its proof)

The compliance addendum (ROADMAP.md) bounds the permitted claim set to what CI
proves. Each permitted claim and its proof:

- "Runs on CNCF-conformant Kubernetes clusters." Bounded to the clusters CI
  actually exercises (kind in `kind-e2e` / `kind-e2e-husk`, the KVM runner in
  kvm-test.yaml). State the cluster, not "any Kubernetes".
- "The husk pod runs PSA `restricted` with exactly the documented `/dev/kvm`
  device-plugin exception." Bounded to the `kind-e2e-husk` PSA assertion: the pod
  is admitted with its two documented exceptions, the same pod minus the
  exceptions is admitted into a `restricted` namespace, and a privileged pod is
  rejected (PSA enforcing). The exception set is ADR 0003. State the exceptions;
  do not round them to zero.
- "Standard quota / policy / eviction semantics apply to husk pods." Bounded to
  what is object-level proven on `kind-e2e-husk` (the PodDisruptionBudget and
  drain/eviction behavior, docs/threat-model.md section 0 surface 8). Husk pods
  ARE ordinary pods, so ResourceQuota/LimitRange/PDB DO apply to THEM; this is
  honest because the claim is about the POD, not the sandbox VM's internal
  boundary.
- "The `agents.x-k8s.io` conformance facade implements the upstream API." Bounded
  to the `facade-conformance` job's per-row matrix in docs/facade-conformance.md:
  every upstream artifact is PROVEN-OBJECT-LEVEL-ON-KIND, NEEDS-BARE-METAL, or
  JUSTIFIED-EXCEPTION, with no silent divergence. The facade is the vendored
  conformance facade `cmd/facade` (ADR 0001). Claim only the rows the matrix
  marks proven.

When in doubt, phrase the claim as "CI proves X on cluster Y" rather than a
blanket capability.

## Residuals ship as ADRs

Anything true of the DESIGN but not yet proven by CI, or true today but weaker
than a reader would assume, is a RESIDUAL. A residual does not get a softened
claim; it gets an ADR in `docs/adr/` and a truthful status in
docs/threat-model.md. The current residual ADRs:

- ADR 0003: the `/dev/kvm` device-plugin PSA exception (the documented exception
  set that bounds the PSA-restricted claim).
- ADR 0004: node-flat snapshot trust domain (why namespaces are not a snapshot
  isolation boundary; treat the cluster as one trust domain).
- ADR 0005: raw-forkd is not for untrusted multi-tenant (the husk pod is the
  default tenant runner; raw-forkd is an opt-in privileged fallback).
- ADR 0006: the husk-pod `NET_ADMIN` egress firewall (the planned default-deny
  husk egress control; `proposed` until it ships, with the metadata-reachable gap
  stated open in the meantime).

The framework that governs ADRs is ADR 0000 and docs/adr/README.md.

## Contributor checklist

Before merging a doc, README change, CRD description, or release note that makes a
conformance/compliance/security claim:

1. Does the claim use any forbidden phrase above? If yes, rewrite or remove it.
2. Is the claim bounded to a named CI job that proves it (a required check, or a
   named gated job)? If not, either tie it to the proof or downgrade it to a
   linked residual ADR.
3. Does the claim imply a pod-scoped mechanism governs sandbox VMs that is not
   actually wired? If yes, fix the phrasing (honest-Kubernetes-semantics).
4. Does the claim state any exception COUNT (PSA, egress, capabilities)
   accurately, never rounded down to zero? Cross-check ADR 0003 and ADR 0006.
5. If the claim is about a residual, does it link the ADR rather than soften the
   claim?
6. If the claim moved the security surface, did the SAME PR update
   docs/threat-model.md (the change-discipline rule)?

A claim that cannot pass this checklist is not shipped.
