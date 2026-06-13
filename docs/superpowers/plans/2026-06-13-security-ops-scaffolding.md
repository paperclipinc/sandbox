# Security Operations Scaffolding Implementation Plan (issue #35)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the engineering-tractable half of a real vulnerability-response posture for a project that ships Firecracker plus a guest kernel plus a guest agent: keyless cosign signing and SBOM attestation of the published container images, a CVE/dependency pipeline (govulncheck as a required job, Trivy image scan, a dependabot audit), a kernel-CVE tracking doc that pins the vmlinux we ship, a SECURITY.md gap-fill, and a documented human-review-required-paths policy backed by CODEOWNERS.

**Architecture:** All the moving parts are CI jobs and docs, so the "test" for each is a concrete verification command run locally (or via `act` / a CI dry run) rather than a Go unit test: `cosign verify`, `cosign verify-attestation`, `syft scan`, `govulncheck ./...`, `trivy image`, `kubeconform`/`actionlint` for YAML, and `grep` for the dash rule. The docker-build job currently builds `mitos-controller`, `mitos-forkd`, and `mitos-husk-stub` only with local `:ci` tags and NEVER pushes; signing and attestation need real published images, so we add a separate `publish` workflow that builds, pushes to `ghcr.io/paperclipinc/mitos-*` by digest, signs keyless via OIDC, and attaches an SBOM attestation, while the existing `docker-build` PR job stays a fast local-build gate and gains govulncheck + a Trivy scan of the locally-built images. The human-policy pieces (the actual disclosure mailbox, the real reviewer org membership, the published response SLA commitment) are flagged in a Human-gated section, not implemented as tasks.

**Tech Stack:** GitHub Actions, `sigstore/cosign-installer` + `cosign` (keyless, GitHub OIDC), `anchore/sbom-action` (syft), `aquasecurity/trivy-action`, `golang/govulncheck-action` (or `go run golang.org/x/vuln/cmd/govulncheck`), `docker/build-push-action` + `docker/metadata-action`, `actionlint` for workflow linting. Go 1.26.2. Repo: `paperclipinc/mitos`.

**Context for the implementer:**

- The repo already has: `.github/workflows/codeql.yml` (Go + Python CodeQL, weekly + on PR), `.github/workflows/scorecard.yml` (OpenSSF Scorecard, weekly), `.github/dependabot.yml` (gomod /, github-actions /, pip /sdk/python, npm /plugins/paperclip, all weekly, grouped minor+patch), `.github/CODEOWNERS` (maps `/internal/fork/`, `/internal/firecracker/`, `/internal/daemon/`, `/guest/`, `/docs/threat-model.md` to `@stubbi`), and `SECURITY.md`.
- The `docker-build` job in `.github/workflows/ci.yaml` (lines 154-164) builds three images with local `:ci` tags via plain `docker build` and does NOT push. There is no ghcr publish/release-image workflow anywhere (`grep -rln "login-action|build-push-action|cosign" .github/` is empty).
- The guest kernel is NOT pinned to a fixed version today. Both `.github/workflows/ci.yaml` (around line 572) and `.github/workflows/kvm-test.yaml` (around line 47) set `FC_VERSION=v1.15.0` and then resolve the LATEST `firecracker-ci/v1.15/<arch>/vmlinux-<x.y.z>` key off the S3 listing at run time. The husk pod mounts a node-staged `vmlinux` (`internal/controller/huskpod.go`, `huskKernelMountPath = "/var/lib/mitos/kernel/vmlinux"`). So "we pin a vmlinux" is currently a floating pin; the kernel-CVE doc must record the exact version we ship and turn the floating resolution into a recorded, reviewable version.
- Conventions (CLAUDE.md is authoritative): NEVER use em or en dashes anywhere (source, YAML, Markdown, commit messages, PR bodies). Use only `.` `,` `;` `:` and the ASCII hyphen for ranges/identifiers. Conventional commits (feat, fix, docs, ci, chore). Branch is `feat/rename-to-mitos`. Stage explicit paths only, never `git add -A`. End commit messages with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. Threat-model delta in the same PR when the security surface moves. No-unverified-claims: every doc claim reproducible from a command.
- Secrets rule: signing uses keyless OIDC (no private key material in the repo or secrets). Never log secret values; log keys and counts only.

---

## File Structure

New and modified files, each with one clear responsibility:

- `.github/workflows/publish.yaml` (CREATE): on tag push (and `workflow_dispatch`), build + push the three images to `ghcr.io/paperclipinc/mitos-*` by digest, cosign keyless sign, generate + attest an SBOM. This is the only place that needs registry write + OIDC `id-token: write`.
- `.github/workflows/ci.yaml` (MODIFY): add a `govulncheck` job (required), and add a Trivy filesystem-and-image scan step to the existing `docker-build` job (scans the locally built `:ci` images, no registry needed).
- `.github/workflows/actionlint.yaml` (CREATE): lint all workflow YAML so a malformed signing/scan job fails fast.
- `SECURITY.md` (MODIFY): fix the disclosure address ownership, add a supported-versions statement tied to releases, add explicit response SLAs, and add the verification-of-signed-images section plus a pointer to the review policy and kernel-CVE doc.
- `docs/security-review-policy.md` (CREATE): the human-review-required-paths policy (which dirs, why, what a reviewer checks, the merge gate), referencing CODEOWNERS.
- `docs/kernel-cve.md` (CREATE): the pinned vmlinux version + source URL, the CVE-watch process, and the update/rebuild/re-pin procedure.
- `docs/supply-chain.md` (CREATE): how images are signed and how a consumer verifies signature + SBOM attestation; the exact `cosign verify` / `cosign verify-attestation` commands.
- `.github/CODEOWNERS` (MODIFY): add `/internal/vsock/`, `/docs/security-review-policy.md`, `/docs/kernel-cve.md`, and the signing/publish workflow to the security-sensitive set.
- `docs/threat-model.md` (MODIFY): add supply-chain (image provenance) and kernel-currency rows; mark prior "unsigned images / no SBOM" gaps as mitigated where the new jobs land.
- `ROADMAP.md` (MODIFY): flip the engineering-doable parts of #35 to done; leave the human-gated parts open with notes.

Each task below produces a self-contained change with its own verification command and commit.

---

### Task 1: Pin the guest kernel to an exact version (shared CI variable)

The kernel is currently a floating "latest in firecracker-ci/v1.15" pin. Record an exact version so the kernel-CVE doc can name what we ship and a re-pin is a reviewable diff. This is a prerequisite for Task 6 (the doc must reference a real pinned value).

**Files:**
- Modify: `.github/workflows/ci.yaml` (the kernel staging step, around lines 570-578)
- Modify: `.github/workflows/kvm-test.yaml` (the kernel staging steps, around lines 47 and 67-76)

- [ ] **Step 1: Discover the exact version both jobs currently resolve to**

Run:
```bash
ARCH=x86_64
curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/?prefix=firecracker-ci/v1.15/${ARCH}/vmlinux-" \
  | grep -oE "firecracker-ci/v1.15/${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]+" \
  | sort -V | tail -1
```
Expected: a single key like `firecracker-ci/v1.15/x86_64/vmlinux-6.1.102` (record the exact `x.y.z` it prints; that is the value to pin below). Run the same with `ARCH=aarch64` and record that key too.

- [ ] **Step 2: Replace the floating resolve with an exact pinned key in `ci.yaml`**

In the kernel staging step of `.github/workflows/ci.yaml` (the block that sets `FC_VERSION=v1.15.0`, computes `CI_VERSION`, lists S3 with `grep ... | sort -V | tail -1`, then curls the key into `/tmp/vmlinux`), replace the dynamic `KERNEL_KEY=$(curl ... sort -V | tail -1)` resolution with the pinned version recorded in Step 1. Use the version you recorded; the literal below shows the shape (substitute the real `x.y.z`):

```yaml
      - name: Provide a guest kernel on the node for forkd and husk pods
        run: |
          set -euo pipefail
          ARCH=$(uname -m)
          # Pinned guest kernel. The exact version we ship is recorded in
          # docs/kernel-cve.md; a bump is a reviewed diff to this line and the
          # matching line in kvm-test.yaml, never a floating "latest" resolve.
          KERNEL_VERSION=6.1.102
          KERNEL_KEY="firecracker-ci/v1.15/${ARCH}/vmlinux-${KERNEL_VERSION}"
          echo "Staging pinned kernel: $KERNEL_KEY"
          curl -fsSL -o /tmp/vmlinux "https://s3.amazonaws.com/spec.ccfc.min/${KERNEL_KEY}"
          worker=$(docker ps --format '{{.Names}}' | grep -m1 'mitos-husk-worker')
          docker exec "$worker" mkdir -p /var/lib/mitos
          docker cp /tmp/vmlinux "$worker:/var/lib/mitos/vmlinux"
          docker exec "$worker" ls -lh /var/lib/mitos/vmlinux
```

- [ ] **Step 3: Apply the same pin in `kvm-test.yaml`**

In `.github/workflows/kvm-test.yaml`, the kernel staging step (around lines 67-76) resolves the kernel the same floating way. Replace its `KERNEL_KEY=$(curl ... sort -V | tail -1)` with the identical pinned `KERNEL_VERSION=6.1.102` + `KERNEL_KEY="firecracker-ci/v1.15/${ARCH}/vmlinux-${KERNEL_VERSION}"` form, keeping the surrounding `curl ... -o` target unchanged. Leave the rootfs resolution (the `ubuntu-` key) as is: this task pins only the kernel.

- [ ] **Step 4: Verify the pinned key resolves and the YAML is valid**

Run:
```bash
ARCH=x86_64; KERNEL_VERSION=6.1.102
curl -fsSI "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/${ARCH}/vmlinux-${KERNEL_VERSION}" | head -1
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/ci.yaml")); yaml.safe_load(open(".github/workflows/kvm-test.yaml")); print("yaml ok")'
```
Expected: `HTTP/1.1 200 OK` (the pinned key exists) and `yaml ok`.

- [ ] **Step 5: Confirm no floating resolve remains**

Run:
```bash
grep -n "vmlinux-\[0-9\]" .github/workflows/ci.yaml .github/workflows/kvm-test.yaml || echo "no floating kernel resolve remains"
```
Expected: `no floating kernel resolve remains` (the `sort -V | tail -1` kernel resolution is gone; the rootfs `ubuntu-` resolve may still match a different pattern, that is fine).

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/ci.yaml .github/workflows/kvm-test.yaml
git commit -m "ci: pin the guest kernel to an exact firecracker-ci version

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Add govulncheck as a required CI job

`govulncheck` is the Go-native vulnerability scanner: it reports only vulnerabilities reachable from the call graph, so it is low-noise and suitable as a required gate.

**Files:**
- Modify: `.github/workflows/ci.yaml` (add a new top-level job under `jobs:`, alongside `go-test` and `go-lint`)

- [ ] **Step 1: Verify govulncheck runs clean locally first (so the gate is green on day one)**

Run:
```bash
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```
Expected: either `No vulnerabilities found.` or a list of findings. If there are findings, they must be triaged BEFORE adding the gate (bump the offending dependency via go.mod, or, only if it is provably unreachable, record an explicit justification in the PR). Do not add a passing-by-luck gate over a known finding.

- [ ] **Step 2: Add the `govulncheck` job**

Insert this job into `.github/workflows/ci.yaml` immediately after the `go-lint` job (before `manifests`):

```yaml
  govulncheck:
    # Go-native vulnerability scan. govulncheck reports only vulnerabilities
    # reachable from this module's call graph, so it is low-noise and a
    # required gate. CVE/dependency pipeline, issue #35. A new finding fails
    # the job; the fix is a dependency bump (dependabot opens most of these)
    # or, only for a provably unreachable advisory, a documented exclusion.
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
      - name: govulncheck
        run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          "$(go env GOPATH)/bin/govulncheck" ./...
```

- [ ] **Step 3: Validate the workflow YAML parses**

Run:
```bash
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/ci.yaml")); print("yaml ok")'
```
Expected: `yaml ok`.

- [ ] **Step 4: Lint the workflow with actionlint (installed in Task 7; if not yet present, run via the published action image)**

Run:
```bash
docker run --rm -v "$PWD:/repo" --workdir /repo rhysd/actionlint:latest -color .github/workflows/ci.yaml || true
```
Expected: no errors for the new `govulncheck` job (the `|| true` tolerates pre-existing warnings in unrelated jobs; read the output and confirm the new job is clean).

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yaml
git commit -m "ci: add govulncheck as a required vulnerability gate

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Add a Trivy scan of the locally built images to docker-build

The PR `docker-build` job builds `mitos-controller:ci`, `mitos-forkd:ci`, and `mitos-husk-stub:ci` locally and does not push. Trivy can scan those local image references directly, so this gives image-CVE coverage on every PR with no registry. Gate on HIGH and CRITICAL with fixes available.

**Files:**
- Modify: `.github/workflows/ci.yaml` (the `docker-build` job, lines 154-164)

- [ ] **Step 1: Verify Trivy scans a locally built image (run once locally to sanity-check the gate level)**

Run:
```bash
docker build -f Dockerfile.controller -t mitos-controller:ci .
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  aquasec/trivy:latest image --scanners vuln --severity HIGH,CRITICAL \
  --ignore-unfixed --exit-code 1 mitos-controller:ci
```
Expected: exit 0 with `Total: 0` for HIGH/CRITICAL-fixable, or a list. If there are fixable HIGH/CRITICAL CVEs, address them (bump the base image in the Dockerfile) before gating, or document why a specific CVE is accepted in a `.trivyignore` with the CVE id and a one-line reason.

- [ ] **Step 2: Add Trivy steps to the existing `docker-build` job**

Replace the body of the `docker-build` job in `.github/workflows/ci.yaml` so it scans each built image (keep the three existing `docker build` lines; add the Trivy steps after them):

```yaml
  docker-build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: docker build -f Dockerfile.controller -t mitos-controller:ci .
      - run: docker build -f Dockerfile.forkd -t mitos-forkd:ci .
      # The husk-stub image runs the dormant VMM inside each husk pod (the
      # pod-native default, issue #18). It is referenced by the controller
      # Deployment's --husk-stub-image, so it must build cleanly here.
      - run: docker build -f Dockerfile.husk-stub -t mitos-husk-stub:ci .
      # Trivy scans each locally built image for known CVEs (CVE/dependency
      # pipeline, issue #35). HIGH and CRITICAL with a fix available fail the
      # job; unfixed advisories are reported but do not gate (no action to
      # take). Accepted CVEs live in .trivyignore with a reason.
      - name: Trivy scan controller image
        uses: aquasecurity/trivy-action@0.28.0
        with:
          image-ref: mitos-controller:ci
          scanners: vuln
          severity: HIGH,CRITICAL
          ignore-unfixed: true
          exit-code: "1"
          trivyignores: .trivyignore
      - name: Trivy scan forkd image
        uses: aquasecurity/trivy-action@0.28.0
        with:
          image-ref: mitos-forkd:ci
          scanners: vuln
          severity: HIGH,CRITICAL
          ignore-unfixed: true
          exit-code: "1"
          trivyignores: .trivyignore
      - name: Trivy scan husk-stub image
        uses: aquasecurity/trivy-action@0.28.0
        with:
          image-ref: mitos-husk-stub:ci
          scanners: vuln
          severity: HIGH,CRITICAL
          ignore-unfixed: true
          exit-code: "1"
          trivyignores: .trivyignore
```

- [ ] **Step 3: Create an empty (header-only) `.trivyignore`**

Create `.trivyignore` at the repo root:

```text
# Accepted CVEs for the mitos container images (issue #35).
# Each entry MUST carry a CVE id and a one-line reason it is accepted
# (unreachable in our usage, or no fix yet and mitigated elsewhere).
# Format: one CVE id per line, comments with #. Empty for now.
```

- [ ] **Step 4: Validate YAML and confirm the action ref is pinned**

Run:
```bash
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/ci.yaml")); print("yaml ok")'
grep -n "aquasecurity/trivy-action@" .github/workflows/ci.yaml
```
Expected: `yaml ok` and three lines each pinned to `@0.28.0` (a tag, not a floating major).

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yaml .trivyignore
git commit -m "ci: scan built images with Trivy on every PR

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Create the publish workflow that pushes, signs (cosign keyless), and attests an SBOM

Signing and SBOM attestation need real published images by digest. Add a dedicated `publish` workflow gated on tag pushes (release artifacts) plus manual dispatch. Keyless cosign uses the workflow's GitHub OIDC identity, so there is no key material to store.

**Files:**
- Create: `.github/workflows/publish.yaml`

- [ ] **Step 1: Create the publish workflow**

Create `.github/workflows/publish.yaml`:

```yaml
name: Publish signed images

on:
  push:
    tags:
      - "v*"
  workflow_dispatch:
    inputs:
      tag:
        description: "Image tag to build and publish (for example v0.1.0)"
        required: true

permissions:
  contents: read

jobs:
  publish:
    # Build, push, cosign keyless-sign, and SBOM-attest the three mitos images
    # under ghcr.io/paperclipinc (supply chain, issue #35). Keyless signing
    # uses the workflow GitHub OIDC identity (id-token: write); there is NO
    # private key in the repo or in secrets. Each image is pushed by digest and
    # the signature and SBOM attestation are bound to that digest.
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
      id-token: write
    strategy:
      fail-fast: false
      matrix:
        include:
          - name: mitos-controller
            dockerfile: Dockerfile.controller
          - name: mitos-forkd
            dockerfile: Dockerfile.forkd
          - name: mitos-husk-stub
            dockerfile: Dockerfile.husk-stub
    env:
      REGISTRY: ghcr.io
      IMAGE: ghcr.io/paperclipinc/${{ matrix.name }}
    steps:
      - uses: actions/checkout@v4
      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Image metadata (tags and labels)
        id: meta
        uses: docker/metadata-action@v5
        with:
          images: ${{ env.IMAGE }}
          tags: |
            type=ref,event=tag
            type=sha
      - name: Build and push by digest
        id: build
        uses: docker/build-push-action@v6
        with:
          context: .
          file: ${{ matrix.dockerfile }}
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
      - name: Install cosign
        uses: sigstore/cosign-installer@v3
      - name: Cosign keyless sign by digest
        env:
          DIGEST: ${{ steps.build.outputs.digest }}
        run: |
          set -euo pipefail
          cosign sign --yes "${IMAGE}@${DIGEST}"
      - name: Generate SBOM (SPDX JSON)
        uses: anchore/sbom-action@v0
        with:
          image: ${{ env.IMAGE }}@${{ steps.build.outputs.digest }}
          format: spdx-json
          output-file: sbom-${{ matrix.name }}.spdx.json
      - name: Cosign attest the SBOM by digest
        env:
          DIGEST: ${{ steps.build.outputs.digest }}
        run: |
          set -euo pipefail
          cosign attest --yes \
            --predicate "sbom-${{ matrix.name }}.spdx.json" \
            --type spdxjson \
            "${IMAGE}@${DIGEST}"
      - name: Upload SBOM artifact
        uses: actions/upload-artifact@v4
        with:
          name: sbom-${{ matrix.name }}
          path: sbom-${{ matrix.name }}.spdx.json
```

- [ ] **Step 2: Validate the workflow YAML parses**

Run:
```bash
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/publish.yaml")); print("yaml ok")'
```
Expected: `yaml ok`.

- [ ] **Step 3: Lint the workflow**

Run:
```bash
docker run --rm -v "$PWD:/repo" --workdir /repo rhysd/actionlint:latest -color .github/workflows/publish.yaml
```
Expected: no errors. actionlint validates the `id-token: write` permission, the matrix, and the action refs.

- [ ] **Step 4: Confirm the keyless and digest invariants by inspection**

Run:
```bash
grep -n "id-token: write" .github/workflows/publish.yaml
grep -n "cosign sign --yes" .github/workflows/publish.yaml
grep -n "@\${{ steps.build.outputs.digest }}\|@\${DIGEST}" .github/workflows/publish.yaml
grep -ci "cosign.key\|COSIGN_PRIVATE_KEY\|--key " .github/workflows/publish.yaml
```
Expected: the OIDC permission is present; sign and attest both target `IMAGE@DIGEST` (never a mutable tag); the last grep prints `0` (no private key material referenced, proving keyless).

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/publish.yaml
git commit -m "ci: publish images signed keyless with an attested SBOM

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Add an actionlint workflow so signing/scan YAML cannot rot

A malformed signing or scanning workflow silently disables the supply-chain controls. actionlint as its own job catches that.

**Files:**
- Create: `.github/workflows/actionlint.yaml`

- [ ] **Step 1: Create the actionlint workflow**

Create `.github/workflows/actionlint.yaml`:

```yaml
name: Lint workflows

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

permissions:
  contents: read

jobs:
  actionlint:
    # Lint every GitHub Actions workflow. A malformed supply-chain job (signing,
    # SBOM, Trivy, govulncheck) would otherwise silently disable a security
    # control; actionlint fails the PR instead. Issue #35.
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: actionlint
        run: |
          set -euo pipefail
          bash <(curl -fsSL https://raw.githubusercontent.com/rhysd/actionlint/main/scripts/download-actionlint.bash)
          ./actionlint -color
```

- [ ] **Step 2: Run actionlint over the whole tree locally**

Run:
```bash
docker run --rm -v "$PWD:/repo" --workdir /repo rhysd/actionlint:latest -color
```
Expected: no errors across `ci.yaml`, `kvm-test.yaml`, `codeql.yml`, `scorecard.yml`, `release-please.yml`, `publish.yaml`, and `actionlint.yaml`. Fix any real errors the new jobs introduced (warnings in long-standing jobs may be triaged separately, but the new files must be clean).

- [ ] **Step 3: Validate the YAML parses**

Run:
```bash
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/actionlint.yaml")); print("yaml ok")'
```
Expected: `yaml ok`.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/actionlint.yaml
git commit -m "ci: lint workflows with actionlint

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Write the supply-chain verification doc

A signature and an attestation are worthless if no consumer knows how to verify them. Document the exact `cosign verify` and `cosign verify-attestation` commands, bound to the workflow identity.

**Files:**
- Create: `docs/supply-chain.md`

- [ ] **Step 1: Create the doc**

Create `docs/supply-chain.md`:

```markdown
# Supply chain: image signing and SBOM verification

The published `ghcr.io/paperclipinc/mitos-*` images are signed with cosign in
keyless mode and carry an SPDX SBOM attestation. Both are produced by
`.github/workflows/publish.yaml` using the workflow GitHub OIDC identity, so
there is no long-lived signing key: the signing identity IS the workflow that
built the image. This page is the consumer-side verification procedure; every
command here is runnable against a published tag.

## Images

- `ghcr.io/paperclipinc/mitos-controller`
- `ghcr.io/paperclipinc/mitos-forkd`
- `ghcr.io/paperclipinc/mitos-husk-stub`

Each tag is pushed by digest; the signature and SBOM attestation are bound to
the digest, not to the mutable tag.

## Verify the signature

Install cosign (https://docs.sigstore.dev/cosign/installation/), then:

```bash
COSIGN_EXPERIMENTAL=1 cosign verify \
  --certificate-identity-regexp "https://github.com/paperclipinc/mitos/.github/workflows/publish.yaml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/paperclipinc/mitos-controller:VERSION
```

Replace `VERSION` with the release tag (for example `v0.1.0`). A successful
verify prints the verified signature payload and exits 0. The
`--certificate-identity-regexp` pins the signer to OUR publish workflow on OUR
repository; a signature from any other identity fails verification.

## Verify the SBOM attestation

```bash
COSIGN_EXPERIMENTAL=1 cosign verify-attestation \
  --type spdxjson \
  --certificate-identity-regexp "https://github.com/paperclipinc/mitos/.github/workflows/publish.yaml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/paperclipinc/mitos-controller:VERSION
```

A successful verify confirms the SBOM was produced and signed by our publish
workflow. To read the SBOM contents, extract the predicate:

```bash
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp "https://github.com/paperclipinc/mitos/.github/workflows/publish.yaml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/paperclipinc/mitos-controller:VERSION \
  | jq -r '.payload' | base64 -d | jq '.predicate'
```

## Optional: admission-time enforcement

A cluster that wants to refuse unsigned mitos images can enforce the same
identity with a policy controller (sigstore policy-controller or Kyverno
`verifyImages`) using the identity regexp and issuer above. This is a
deployment choice, not a default; the project ships the signatures, the
operator chooses to require them.

## What is NOT covered yet

- The guest kernel is a separate artifact with its own currency process; see
  docs/kernel-cve.md.
- Reproducible builds (bit-for-bit) are not claimed.
```

- [ ] **Step 2: Verify the doc carries no em or en dashes and references real files**

Run:
```bash
grep -nP "[\x{2013}\x{2014}]" docs/supply-chain.md && echo "DASH FOUND (fix it)" || echo "no en/em dashes"
grep -c "publish.yaml" docs/supply-chain.md
```
Expected: `no en/em dashes` and a non-zero count (the doc points at the real workflow file).

- [ ] **Step 3: Commit**

```bash
git add docs/supply-chain.md
git commit -m "docs: image signature and SBOM verification procedure

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Write the kernel-CVE tracking doc

We ship a guest kernel. Record the exact pinned version (from Task 1), where it comes from, how to watch for kernel CVEs, and the re-pin procedure.

**Files:**
- Create: `docs/kernel-cve.md`

- [ ] **Step 1: Create the doc using the pinned version from Task 1**

Create `docs/kernel-cve.md` (substitute the exact `KERNEL_VERSION` you pinned in Task 1 for `6.1.102` if it differs):

```markdown
# Guest kernel currency and CVE watch

Mitos boots guest workloads on a Linux kernel we ship as `vmlinux`. A kernel
CVE in the guest is contained by the Firecracker/KVM boundary (a guest-kernel
bug is not automatically a host escape), but a guest-to-host escape chain often
starts in the guest kernel, so we track guest-kernel currency deliberately.

## What we ship

- Source: the Firecracker CI kernel bucket,
  `https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/<arch>/vmlinux-<version>`.
- Firecracker line: `v1.15` (the `FC_VERSION` set in the CI and KVM workflows).
- Pinned kernel version: `6.1.102` (x86_64 and aarch64).
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
```

- [ ] **Step 2: Verify no dashes and that the pinned version matches the workflows**

Run:
```bash
grep -nP "[\x{2013}\x{2014}]" docs/kernel-cve.md && echo "DASH FOUND (fix it)" || echo "no en/em dashes"
grep -h "KERNEL_VERSION=" .github/workflows/ci.yaml .github/workflows/kvm-test.yaml
grep "Pinned kernel version" docs/kernel-cve.md
```
Expected: `no en/em dashes`; the `KERNEL_VERSION=` values in both workflows and the doc's "Pinned kernel version" line are the SAME version. If they differ, fix the doc to match Task 1.

- [ ] **Step 3: Commit**

```bash
git add docs/kernel-cve.md
git commit -m "docs: guest kernel currency and CVE watch process

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: Write the security-review-required-paths policy

CLAUDE.md and SECURITY.md both assert named-human review for the security-sensitive paths. CODEOWNERS enforces it mechanically; this doc states WHAT a reviewer checks and WHY each path is in scope.

**Files:**
- Create: `docs/security-review-policy.md`

- [ ] **Step 1: Create the doc**

Create `docs/security-review-policy.md`:

```markdown
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
```

- [ ] **Step 2: Verify no dashes**

Run:
```bash
grep -nP "[\x{2013}\x{2014}]" docs/security-review-policy.md && echo "DASH FOUND (fix it)" || echo "no en/em dashes"
```
Expected: `no en/em dashes`.

- [ ] **Step 3: Commit**

```bash
git add docs/security-review-policy.md
git commit -m "docs: security-review-required-paths policy

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: Extend CODEOWNERS to the full security-sensitive set

CODEOWNERS already covers `internal/fork`, `internal/firecracker`, `internal/daemon`, `guest/`, and `docs/threat-model.md`. Add the paths the new policy names: `internal/vsock`, the new docs, and the publish workflow.

**Files:**
- Modify: `.github/CODEOWNERS`

- [ ] **Step 1: Read the current file and append the missing paths**

The current file ends with `/docs/threat-model.md @stubbi`. Append (keep the existing owner `@stubbi`; the Human-gated section flags that this login must be a real reviewer with org write access):

```text
/internal/vsock/ @stubbi
/docs/security-review-policy.md @stubbi
/docs/kernel-cve.md @stubbi
/docs/supply-chain.md @stubbi
/.github/workflows/publish.yaml @stubbi
```

- [ ] **Step 2: Verify CODEOWNERS syntax (no empty owners; every line has an owner)**

Run:
```bash
awk 'NF && $1 !~ /^#/ { if (NF < 2) { print "NO OWNER: " $0; bad=1 } } END { if (!bad) print "every pattern has an owner" }' .github/CODEOWNERS
```
Expected: `every pattern has an owner`.

- [ ] **Step 3: Confirm all six security-sensitive code paths and the new docs are covered**

Run:
```bash
for p in internal/fork internal/firecracker internal/daemon internal/vsock guest docs/security-review-policy.md docs/kernel-cve.md docs/supply-chain.md .github/workflows/publish.yaml; do
  grep -q "$p" .github/CODEOWNERS && echo "covered: $p" || echo "MISSING: $p"
done
```
Expected: every line prints `covered:` (no `MISSING:`).

- [ ] **Step 4: Commit**

```bash
git add .github/CODEOWNERS
git commit -m "chore: extend CODEOWNERS to the full security-sensitive set

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 10: Fill the SECURITY.md gaps

The current SECURITY.md has a disclosure path, a one-line supported-versions statement, a scope, and an AI-assisted-development note. Gaps to close: response SLAs beyond the 72h ack, a clearer supported-versions statement tied to releases, a verification-of-signed-images pointer, and pointers to the new policy and kernel docs. The disclosure mailbox ownership is flagged Human-gated.

**Files:**
- Modify: `SECURITY.md`

- [ ] **Step 1: Add response SLAs under the reporting section**

After the existing line `We will acknowledge your report within 72 hours and keep you informed of progress toward a fix and disclosure.`, add:

```markdown

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
```

- [ ] **Step 2: Replace the supported-versions section with a release-tied statement**

Replace the existing block:

```markdown
## Supported Versions

This project is pre-1.0. Only the latest release receives security fixes.
```

with:

```markdown
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
```

- [ ] **Step 3: Add a verifying-releases section and policy pointer before "Current Status"**

Insert before the `## Current Status` heading:

```markdown
## Verifying releases

Published images are signed with cosign (keyless, GitHub OIDC) and carry an
SPDX SBOM attestation. The exact `cosign verify` and `cosign verify-attestation`
commands are in [docs/supply-chain.md](docs/supply-chain.md).

## Code review policy for security-sensitive paths

Changes to the security-sensitive paths (`internal/fork`,
`internal/firecracker`, `internal/daemon`, `internal/vsock`, `guest/`) require
a named human reviewer before merge, enforced by `.github/CODEOWNERS`. The full
policy is in [docs/security-review-policy.md](docs/security-review-policy.md).
```

- [ ] **Step 4: Update the AI-Assisted Development Policy paragraph to add internal/vsock and point at the policy doc**

Replace the sentence listing the paths in the existing `## AI-Assisted Development Policy` section so it reads:

```markdown
Substantial portions of this codebase are AI-assisted. Security-sensitive paths
receive named-human review before merge: `internal/fork`,
`internal/firecracker`, `internal/daemon`, `internal/vsock`, `guest/`, and
future token/attenuation code. The policy is documented in
[docs/security-review-policy.md](docs/security-review-policy.md) and tracked in
issue #35.
```

- [ ] **Step 5: Verify no dashes and that all internal links resolve to real files**

Run:
```bash
grep -nP "[\x{2013}\x{2014}]" SECURITY.md && echo "DASH FOUND (fix it)" || echo "no en/em dashes"
for f in docs/kernel-cve.md docs/supply-chain.md docs/security-review-policy.md; do
  test -f "$f" && echo "link target exists: $f" || echo "MISSING TARGET: $f"
done
```
Expected: `no en/em dashes` and three `link target exists:` lines (Tasks 6, 7, 8 created them).

- [ ] **Step 6: Commit**

```bash
git add SECURITY.md
git commit -m "docs: fill SECURITY.md gaps (SLAs, supported versions, verification)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 11: Audit the dependabot config and close the gaps

dependabot.yml covers gomod /, github-actions /, pip /sdk/python, npm /plugins/paperclip. Audit for uncovered ecosystems/directories so dependency CVEs surface as PRs everywhere.

**Files:**
- Modify: `.github/dependabot.yml` (only if the audit finds an uncovered manifest)

- [ ] **Step 1: Enumerate every dependency manifest in the repo**

Run:
```bash
echo "=== go modules ==="; find . -name go.mod -not -path '*/vendor/*'
echo "=== package.json (npm) ==="; find . -name package.json -not -path '*/node_modules/*'
echo "=== python deps ==="; find . \( -name pyproject.toml -o -name requirements*.txt \) -not -path '*/.venv/*'
echo "=== dockerfiles ==="; ls Dockerfile*
```
Expected: a list. Compare each found manifest's directory against the `directory:` entries already in `.github/dependabot.yml`.

- [ ] **Step 2: Add an entry for each uncovered manifest**

For every manifest NOT already covered (for example a `sdk/typescript` package.json, a nested `third_party/.../go.mod` that we actually own, or a Docker ecosystem for base-image bumps), add a block matching the existing style. Example for the TypeScript SDK if `sdk/typescript/package.json` exists and is uncovered:

```yaml
  - package-ecosystem: npm
    directory: /sdk/typescript
    schedule:
      interval: weekly
    commit-message:
      prefix: chore
    groups:
      typescript-sdk-minor-patch:
        update-types:
          - minor
          - patch
```

Add a `docker` ecosystem to watch base-image updates in the Dockerfiles (one entry; dependabot scans all Dockerfiles in the directory):

```yaml
  - package-ecosystem: docker
    directory: /
    schedule:
      interval: weekly
    commit-message:
      prefix: chore
    groups:
      docker-minor-patch:
        update-types:
          - minor
          - patch
```

Do NOT add an entry for a `third_party/` module that is vendored upstream code we do not maintain (CodeQL already excludes `third_party`; dependabot bumps there would be noise we cannot merge).

- [ ] **Step 3: Validate the dependabot YAML**

Run:
```bash
python3 -c 'import yaml; d=yaml.safe_load(open(".github/dependabot.yml")); assert d["version"]==2; print("entries:", len(d["updates"])); [print(" ", u["package-ecosystem"], u["directory"]) for u in d["updates"]]'
```
Expected: `version` is 2 and every manifest directory found in Step 1 (excluding vendored `third_party`) is listed.

- [ ] **Step 4: Commit (only if changed)**

```bash
git add .github/dependabot.yml
git commit -m "chore: cover all dependency manifests in dependabot

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

If the audit found everything already covered, record that finding in the PR body instead of committing an empty change.

---

### Task 12: Update the threat model and ROADMAP

The security surface moved (image provenance, kernel currency, a vulnerability pipeline), so the threat model must reflect it in the same PR (CLAUDE.md rule 2).

**Files:**
- Modify: `docs/threat-model.md`
- Modify: `ROADMAP.md`

- [ ] **Step 1: Add a supply-chain section to the threat model**

Add a new section to `docs/threat-model.md` (after the existing component/boundary sections), stating the status honestly:

```markdown
## Supply chain and artifact provenance (issue #35)

| Boundary | Status | Mechanism |
|---|---|---|
| Image provenance (controller, forkd, husk-stub) | mitigated for published releases | cosign keyless signing + SPDX SBOM attestation, bound to the image digest, produced by `.github/workflows/publish.yaml`; consumer verification in docs/supply-chain.md. |
| Image CVEs | partial | Trivy scans the built images on every PR (HIGH/CRITICAL fixable gate); govulncheck gates Go call-graph-reachable vulnerabilities; base-image and dependency bumps arrive via dependabot. Runtime re-scan of long-lived published tags is not yet automated. |
| Guest kernel currency | partial | The shipped vmlinux is pinned to an exact version (docs/kernel-cve.md) and validated end to end by the KVM workflow; CVE watch is a documented manual process, not an automated feed. |
| Admission-time signature enforcement | open | The project ships signatures; requiring them at admission (policy-controller/Kyverno) is a documented operator choice, not a default. |
```

- [ ] **Step 2: Soften the honest-summary line only if warranted, otherwise leave it**

The top-of-file honest summary says defense-in-depth layers are largely open. The supply-chain layer is now partly mitigated, but do NOT overclaim: leave the "do not run untrusted code in production yet" summary as is (it remains true for the isolation boundary). This step is a deliberate no-op on the summary; the new section carries the delta.

- [ ] **Step 3: Verify no dashes in the threat-model edit**

Run:
```bash
grep -nP "[\x{2013}\x{2014}]" docs/threat-model.md && echo "DASH FOUND (fix it)" || echo "no en/em dashes"
```
Expected: `no en/em dashes`.

- [ ] **Step 4: Update ROADMAP #35**

In `ROADMAP.md`, find the `#35` foundations line ("security operations: we ship a kernel"). Add a short status note marking the engineering-doable pieces done and the human-gated pieces open:

```markdown
- Security operations (#35): cosign keyless signing + SBOM attestation of the
  published images, govulncheck + Trivy as CVE gates, a kernel-CVE tracking doc
  with an exact pinned vmlinux, a CODEOWNERS-backed security-review-required-
  paths policy, and a SECURITY.md gap-fill are DONE (engineering). OPEN
  (human-gated): the monitored disclosure mailbox, the real reviewer org
  membership behind CODEOWNERS, the published response-SLA commitment, an
  automated kernel-CVE feed, and admission-time signature enforcement defaults.
```

- [ ] **Step 5: Verify no dashes in the ROADMAP edit**

Run:
```bash
grep -nP "[\x{2013}\x{2014}]" ROADMAP.md && echo "DASH FOUND (fix it)" || echo "no en/em dashes"
```
Expected: `no en/em dashes`.

- [ ] **Step 6: Commit**

```bash
git add docs/threat-model.md ROADMAP.md
git commit -m "docs: threat-model supply-chain rows and roadmap #35 status

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 13: Full verification and PR

- [ ] **Step 1: Repo-wide dash and YAML sweep over everything this plan touched**

Run:
```bash
grep -rnP "[\x{2013}\x{2014}]" \
  SECURITY.md ROADMAP.md \
  docs/supply-chain.md docs/kernel-cve.md docs/security-review-policy.md docs/threat-model.md \
  .github/workflows/publish.yaml .github/workflows/actionlint.yaml .github/workflows/ci.yaml \
  .github/CODEOWNERS .github/dependabot.yml .trivyignore \
  && echo "DASH FOUND (fix it)" || echo "no en/em dashes across the change set"
for f in .github/workflows/ci.yaml .github/workflows/publish.yaml .github/workflows/actionlint.yaml .github/dependabot.yml; do
  python3 -c "import yaml,sys; yaml.safe_load(open('$f')); print('yaml ok: $f')"
done
```
Expected: `no en/em dashes across the change set` and a `yaml ok:` line per workflow.

- [ ] **Step 2: actionlint the whole workflow tree**

Run:
```bash
docker run --rm -v "$PWD:/repo" --workdir /repo rhysd/actionlint:latest -color
```
Expected: clean for all new/modified workflows.

- [ ] **Step 3: govulncheck and a local Trivy spot-check still pass**

Run:
```bash
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
docker build -f Dockerfile.forkd -t mitos-forkd:ci .
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  aquasec/trivy:latest image --scanners vuln --severity HIGH,CRITICAL \
  --ignore-unfixed --exit-code 1 mitos-forkd:ci
```
Expected: `No vulnerabilities found.` and Trivy exit 0 (or accepted entries in `.trivyignore` with reasons).

- [ ] **Step 4: Confirm the Go build, vet, and lint are unaffected (no code changed, but prove it)**

Run:
```bash
go build ./... && go vet ./... && echo "build+vet ok"
```
Expected: `build+vet ok` (this plan adds no Go code; this guards against an accidental edit).

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin feat/rename-to-mitos
gh pr create --title "Security operations scaffolding (#35)" --body "$(cat <<'BODY'
Engineering-tractable half of issue #35.

What landed:
- Pinned the guest kernel to an exact firecracker-ci version (was a floating latest resolve) in ci.yaml and kvm-test.yaml.
- govulncheck added as a required CI job.
- Trivy scan of the built images added to docker-build (HIGH/CRITICAL fixable gate, .trivyignore for accepted CVEs).
- New publish.yaml: builds and pushes ghcr.io/paperclipinc/mitos-* by digest, cosign keyless (OIDC) signing, SPDX SBOM generation + attestation.
- actionlint job so a malformed supply-chain workflow fails fast.
- docs/supply-chain.md (verify signature + SBOM), docs/kernel-cve.md (pinned version + CVE-watch + re-pin), docs/security-review-policy.md.
- SECURITY.md: response targets, release-tied supported-versions, verification pointer, review-policy pointer, internal/vsock added to the AI-review path list.
- CODEOWNERS extended (internal/vsock, the new docs, publish.yaml).
- dependabot audited and gaps closed.
- Threat-model supply-chain rows added; ROADMAP #35 status updated.

Human-gated (NOT in this PR, see the plan's Human-gated section): the monitored disclosure mailbox, the real reviewer org membership behind CODEOWNERS, the published response-SLA commitment, an automated kernel-CVE feed, and admission-time signature enforcement defaults.

Threat-model delta: docs/threat-model.md supply-chain section (CLAUDE.md rule 2).
Security review: this PR touches CODEOWNERS-gated paths (internal/vsock note, publish.yaml, threat-model); requires the named reviewer.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
BODY
)"
```

- [ ] **Step 6: Watch CI and confirm the new gates run**

Run:
```bash
gh pr checks --watch
```
Expected: `govulncheck`, `docker-build` (now with Trivy steps), and `actionlint` all run and pass. The `publish` workflow does NOT run on a PR (it is tag/dispatch only); confirm by its absence from the PR checks. Merge when green and after the named reviewer approves.

---

## Self-Review

- Spec coverage: SECURITY.md gaps (Task 10), cosign keyless signing in CI (Task 4) + verification doc (Task 6), SBOM syft + attestation (Task 4), govulncheck required job (Task 2) + Trivy image scan (Task 3) + dependabot audit (Task 11), kernel-CVE doc with the pinned vmlinux (Tasks 1 + 7), CODEOWNERS + security-review-policy.md (Tasks 8 + 9). Every numbered requirement maps to a task.
- The kernel "pin" finding (it was floating, not fixed) is handled as a real code change in Task 1 so Task 7's doc references a true value.
- The signing/SBOM pieces required a real published image, so a new `publish.yaml` was added rather than retrofitting the local-only `docker-build` job; `docker-build` stays a fast PR gate and gains the no-registry Trivy scan.
- Every CI/doc artifact has a concrete verification command (cosign verify, govulncheck, trivy image, syft via sbom-action, actionlint, kubeconform-style YAML parse, dash grep) in place of an awkward unit test.
- Type/name consistency: `KERNEL_VERSION` is used identically in Tasks 1, 7; the image names `mitos-controller`/`mitos-forkd`/`mitos-husk-stub` and the registry `ghcr.io/paperclipinc` are consistent across Tasks 3, 4, 6, 10, 12; the cosign identity regexp is identical in Task 6 and the SECURITY.md/threat-model references.

---

## Human-gated (NOT implemented as tasks; require a human decision)

These are deliberately excluded from the tasks above. They are policy/identity commitments, not code:

1. **The disclosure mailbox.** SECURITY.md lists `jannes@paperclip.inc` as the email fallback, but the repository is now `paperclipinc/mitos`. A human must confirm which address is actually monitored and update the fallback line. GitHub private vulnerability reporting (the preferred path) must be enabled in repo Settings by a maintainer.
2. **The CODEOWNERS reviewer identity.** CODEOWNERS uses `@stubbi`. For the review gate to be real, that login must be a member of the `paperclipinc` org with write access, and branch protection on `main` must require review from Code Owners. A human assigns the real reviewer(s) and enables the branch-protection setting; consider a small `@paperclipinc/security` team rather than a single login (bus factor).
3. **The published response-SLA commitment.** Task 10 adds aspirational response targets. Committing to them publicly is a team decision; a human signs off that the team can meet them or edits the numbers.
4. **OIDC/registry permissions for publish.yaml.** Pushing to `ghcr.io/paperclipinc/*` requires the package to allow the repo's `GITHUB_TOKEN` write access (or org package settings); a maintainer confirms package visibility and permissions before the first tag.
5. **Automated kernel-CVE feed.** Task 7 documents a manual CVE-watch process. Wiring an automated feed (a scheduled job that diffs the pinned 6.1.y against the latest stable and opens an issue) is a possible follow-up, flagged here, not built.
6. **Admission-time signature enforcement defaults.** Whether the shipped deploy manifests should REQUIRE signed images (policy-controller/Kyverno) by default is an operator-experience decision, not implemented here.
