#!/usr/bin/env bash
# Registration entrypoint for the in-cluster self-hosted runner.
#
# The base is the OFFICIAL ghcr.io/actions/actions-runner image, which ships
# config.sh + run.sh but does NOT auto-configure from environment variables. So
# this entrypoint does the registration the official image leaves to the
# operator: it exchanges the fine-grained PAT (ACCESS_TOKEN, scoped
# Administration:write on the repo) for a short-lived runner REGISTRATION token,
# configures an EPHEMERAL runner, and runs it for exactly one job.
#
# Ephemeral means the runner exits after one job; the Deployment then restarts
# this entrypoint, which registers a fresh runner. No job state survives across
# jobs. The PAT is never logged (set +x is implicit; we never echo ACCESS_TOKEN
# or the registration token).
set -euo pipefail

: "${REPO_URL:?REPO_URL is required (https://github.com/OWNER/REPO)}"
: "${ACCESS_TOKEN:?ACCESS_TOKEN (fine-grained PAT, Administration:write) is required}"
LABELS="${LABELS:-self-hosted}"
RUNNER_NAME_PREFIX="${RUNNER_NAME_PREFIX:-mitos-cluster}"
RUNNER_WORKDIR="${RUNNER_WORKDIR:-/home/runner/_work}"

# Repo slug (OWNER/REPO) from the URL, for the GitHub API.
slug="${REPO_URL#https://github.com/}"
slug="${slug%.git}"

# Exchange the PAT for a registration token (administration:write). The token
# value is captured into a variable and never printed.
reg_token="$(curl -fsSL -X POST \
  -H "Authorization: Bearer ${ACCESS_TOKEN}" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "https://api.github.com/repos/${slug}/actions/runners/registration-token" | jq -r .token)"

if [ -z "${reg_token}" ] || [ "${reg_token}" = "null" ]; then
  echo "FATAL: could not obtain a runner registration token for ${slug}." >&2
  echo "Check the PAT has Administration:Read-and-write on this repo and the repo is in its scope." >&2
  exit 1
fi

cd /home/runner

# Configure the ephemeral runner. --replace takes over a stale same-name runner
# left by a pod that died before de-registering.
./config.sh --unattended --replace \
  --url "${REPO_URL}" \
  --token "${reg_token}" \
  --name "${RUNNER_NAME_PREFIX}-$(hostname)" \
  --labels "${LABELS}" \
  --work "${RUNNER_WORKDIR}" \
  --ephemeral

# Run one job, then exit; the Deployment restarts us to re-register.
exec ./run.sh
