#!/usr/bin/env bash
#
# husk-activate-latency.sh
#
# Reproducible source for the warm-claim activate latency number mitos
# publishes (the "~27 ms P50" bare-metal reference figure, issue #16 / #18).
#
# What it measures: the time the controller reports for a warm-claim activation
# against an already-prepared (dormant) husk pod. For each of N sequential
# claims it creates a SandboxClaim against a running pool, waits for the claim
# to reach the Ready condition, and parses the activate latency out of the
# Ready condition message the controller writes:
#
#     activated husk pod <pod> on node <node> in <X>ms
#       (internal/controller/sandboxclaim_controller.go, reason "HuskActivated")
#
# That X is the husk-stub-reported time to load the snapshot in place, run the
# fork-correctness handshake, and reach guest-ready. It is NOT the end-to-end
# claim->Ready wall clock (that includes the Kubernetes reconcile round-trip and
# warm-pool refill, which are control-loop bound, not engine bound). This script
# isolates the engine activate the controller records.
#
# It prints min / P50 / P95 / max (nearest-rank) over the N samples and the raw
# sample list, so the published number is reproducible per the repo's
# no-unverified-claims rule.
#
# Requirements: a running mitos cluster with the husk-pods path enabled and a
# warm pool with dormant pods available, plus kubectl and a KUBECONFIG that can
# create SandboxClaims in the target namespace.
#
# Usage:
#   bench/husk-activate-latency.sh <kubeconfig> <pool> [namespace] [iterations]
#
#   <kubeconfig>   path to a kubeconfig for the target cluster
#   <pool>         name of an existing, warm SandboxPool to claim from
#   [namespace]    namespace to create claims in (default: default)
#   [iterations]   number of sequential claims to measure (default: 11)
#
# Example (the reference-node run that produced the published P50):
#   bench/husk-activate-latency.sh ~/.kube/talos-ref python-agent-pool default 11
#
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <kubeconfig> <pool> [namespace] [iterations]" >&2
  exit 2
fi

KUBECONFIG_PATH="$1"
POOL="$2"
NAMESPACE="${3:-default}"
ITERATIONS="${4:-11}"

export KUBECONFIG="$KUBECONFIG_PATH"

# claim readiness poll: total timeout and interval, in seconds.
READY_TIMEOUT="${READY_TIMEOUT:-60}"
POLL_INTERVAL="${POLL_INTERVAL:-0.2}"

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl not found on PATH" >&2
  exit 1
fi

# unique run id so repeated runs do not collide on claim names.
RUN_ID="$(date +%s)-$$"

samples=()

cleanup() {
  # best-effort: remove the claims we created so the run leaves no residue.
  for i in $(seq 1 "$ITERATIONS"); do
    kubectl delete sandboxclaim "bench-activate-${RUN_ID}-${i}" \
      -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# parse the activate latency (milliseconds) out of the Ready condition message
# of a claim. Emits the bare number on stdout, or nothing if not found.
parse_latency() {
  local claim="$1"
  local msg
  msg="$(kubectl get sandboxclaim "$claim" -n "$NAMESPACE" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || true)"
  # message: "activated husk pod <pod> on node <node> in <X>ms"
  printf '%s\n' "$msg" | sed -n 's/.* in \([0-9][0-9.]*\)ms$/\1/p'
}

echo "measuring warm-claim activate latency: pool=$POOL ns=$NAMESPACE iterations=$ITERATIONS"

for i in $(seq 1 "$ITERATIONS"); do
  claim="bench-activate-${RUN_ID}-${i}"

  # Wait for a warm dormant pod (a Running husk pod that no claim has taken yet)
  # before claiming, so each sample is a real warm activation and not blocked on
  # warm-pool refill after the previous claim released its pod. A dormant pod is
  # husk=true, Running, and does NOT carry the mitos.run/claim label.
  warmdeadline=$(( $(date +%s) + READY_TIMEOUT ))
  while [ "$(date +%s)" -lt "$warmdeadline" ]; do
    dormant="$(kubectl get pods -n "$NAMESPACE" \
      -l 'mitos.run/husk=true,!mitos.run/claim' \
      --field-selector=status.phase=Running -o name 2>/dev/null | head -1 || true)"
    [ -n "$dormant" ] && break
    sleep "$POLL_INTERVAL"
  done

  kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: mitos.run/v1alpha1
kind: SandboxClaim
metadata:
  name: ${claim}
spec:
  poolRef:
    name: ${POOL}
EOF

  # wait for the Ready condition to flip true, then read the latency.
  deadline=$(( $(date +%s) + READY_TIMEOUT ))
  latency=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ready="$(kubectl get sandboxclaim "$claim" -n "$NAMESPACE" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    if [ "$ready" = "True" ]; then
      latency="$(parse_latency "$claim")"
      break
    fi
    sleep "$POLL_INTERVAL"
  done

  if [ -z "$latency" ]; then
    echo "  claim $i: timed out or no activate latency in Ready message (skipped)" >&2
  else
    echo "  claim $i: ${latency} ms"
    samples+=("$latency")
  fi

  # release the claim before the next iteration so the warm pool can refill and
  # each measured claim is an independent warm activation.
  kubectl delete sandboxclaim "$claim" -n "$NAMESPACE" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
done

n="${#samples[@]}"
if [ "$n" -eq 0 ]; then
  echo "no successful samples collected" >&2
  exit 1
fi

# sort numerically for nearest-rank percentiles.
sorted=$(printf '%s\n' "${samples[@]}" | sort -n)

nth() {
  # nearest-rank: rank = ceil(p/100 * n), 1-indexed.
  local p="$1"
  local rank
  rank=$(awk -v p="$p" -v n="$n" 'BEGIN { r = (p/100.0)*n; ri = int(r); if (r > ri) ri = ri + 1; if (ri < 1) ri = 1; print ri }')
  printf '%s\n' "$sorted" | sed -n "${rank}p"
}

min=$(printf '%s\n' "$sorted" | head -1)
max=$(printf '%s\n' "$sorted" | tail -1)
p50=$(nth 50)
p95=$(nth 95)

echo
echo "warm-claim activate latency (ms), N=$n:"
echo "  min  $min"
echo "  P50  $p50"
echo "  P95  $p95"
echo "  max  $max"
echo
echo "raw samples (sorted): $(printf '%s ' $sorted)"
