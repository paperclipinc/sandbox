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
