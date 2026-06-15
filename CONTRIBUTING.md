# Contributing

Thanks for your interest in contributing.

## Build and test

See the Commands section of [CLAUDE.md](CLAUDE.md) for the full list. The short version:

```bash
make build
make test-unit
make test-controller   # needs setup-envtest
make test-python
```

Lint with both `golangci-lint run --timeout=5m` and `GOOS=linux golangci-lint run --timeout=5m`; the guest agent is linux-only.

## Commits

- Use conventional commits: feat, fix, docs, ci, chore, refactor, test.

## Developer Certificate of Origin (DCO)

Every commit must be signed off under the
[Developer Certificate of Origin](https://developercertificate.org/). Sign off
with:

```bash
git commit -s
```

This appends a `Signed-off-by: Your Name <you@example.com>` trailer. By signing
off you certify that you wrote the change or otherwise have the right to submit
it under the project's open-source license (Apache 2.0). The sign-off identity
must match the commit author.

A lightweight check (`.github/workflows/dco.yaml`) verifies that every commit in
a pull request carries a sign-off; the standard DCO GitHub App may additionally
be enabled by the maintainer. If you forgot to sign off, add it to existing
commits with:

```bash
git rebase --signoff origin/main
```

We use the DCO, NOT a Contributor License Agreement. This is the
open-core-friendly choice: it certifies provenance without assigning copyright
or relicensing rights. See [docs/open-core.md](docs/open-core.md). Tracked in
issue #34.

## Named-human review for security-sensitive paths

Substantial portions of this codebase are AI-assisted. Changes to the
security-sensitive paths require a named human reviewer before merge, in
addition to CI. This rule lives in [CLAUDE.md](CLAUDE.md) and is enforced
mechanically by [`.github/CODEOWNERS`](.github/CODEOWNERS) (the listed owner is
requested as a required reviewer on any PR touching a matched path). The paths
in scope and what a reviewer checks are documented in
[docs/security-review-policy.md](docs/security-review-policy.md); they include
`internal/fork`, `internal/firecracker`, `internal/daemon`, `internal/vsock`,
`guest/`, the networking and egress paths (`internal/netconf`,
`internal/dnsproxy`), the content store and husk path (`internal/cas`,
`internal/husk`), and the key-material paths (`internal/pki`, `internal/kms`,
`internal/storecrypt`). Tracked in issue #35.

If you find a security vulnerability, do NOT open a public issue or PR; follow
the disclosure process in [SECURITY.md](SECURITY.md).

## Pull requests

- Tests for every behavior change, in the same commit.
- Docs updated in the same PR.
- If the security surface moved, include the threat-model delta (docs/threat-model.md) in the same PR.
- All six CI checks must be green: go-test, go-lint, python-test, docker-build, kind-e2e, firecracker-test.

## Where to start

- Issues labeled "good first issue".
- ROADMAP.md is the priority order; pick something near the top.

## Style

No em or en dashes anywhere; see the Coding Conventions section of [CLAUDE.md](CLAUDE.md).

## Licensing, open-core, and trademark

This repository is open source under the Apache License 2.0 (see `LICENSE`).
Contributions are accepted under the DCO and stay under that license. There is
no hosted or commercial offering today; the open-core boundary is described in
[docs/open-core.md](docs/open-core.md). The "mitos" name and marks are reserved;
see [TRADEMARKS.md](TRADEMARKS.md). The code license does not grant trademark
rights.
