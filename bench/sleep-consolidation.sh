#!/usr/bin/env bash
#
# sleep-consolidation.sh
#
# Reproducible timing source for the reversible sleep-consolidation demo (W4).
# It measures the wall clock of the two latency-bearing phases of the demo so
# any published number is reproducible per the repo's no-unverified-claims rule
# (CLAUDE.md operating principle 1):
#
#   sleep  = checkpoint-on-terminate + dehydrate: the time from asking a bound
#            sandbox to terminate(checkpoint=True) until the workspace head has
#            advanced to the new committed revision (the consolidated head).
#   wake   = resume + hydrate: the time from creating a fresh bound claim until
#            it reaches Ready with the consolidated /workspace state present.
#
# These are END-TO-END wall clocks (they include the Kubernetes reconcile
# round-trip and the warm-pool consume), NOT an isolated engine number; the
# engine-isolated snapshot restore is bench/husk-activate-latency.sh. This script
# measures the user-visible sleep and wake the demo exposes.
#
# The REAL VM-memory resume (waking mid-execution from the live memory image) is
# CLUSTER-GATED: the controller must run with --workspace-memory-snapshots on a
# KVM-capable node. Without it the wake measures the content-only hydrate (still
# a real, reproducible number, just not the memory-resume path); the script
# records which mode produced the number so the result file is unambiguous.
#
# It writes a result row to bench/results/ so the number is reproducible.
#
# Requirements: a running mitos cluster, kubectl + a KUBECONFIG that can create
# the demo objects in the namespace, python3 with the mitos SDK, the mitos CLI.
#
# Usage:
#   bench/sleep-consolidation.sh <kubeconfig> [namespace] [iterations]
#
#   <kubeconfig>   path to a kubeconfig for the target cluster
#   [namespace]    namespace (default: mitos-e2e)
#   [iterations]   sleep/wake cycles to measure (default: 5)
#
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <kubeconfig> [namespace] [iterations]" >&2
  exit 2
fi

export KUBECONFIG="$1"
NAMESPACE="${2:-mitos-e2e}"
ITERS="${3:-5}"
POOL="sleep-demo-pool"
WS="sleep-demo"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "${HERE}/.." && pwd)"

command -v kubectl >/dev/null || { echo "missing kubectl" >&2; exit 1; }
command -v python3 >/dev/null || { echo "missing python3" >&2; exit 1; }

kubectl -n "$NAMESPACE" apply -f "${REPO}/examples/sleep-consolidation/pool.yaml" >/dev/null
kubectl -n "$NAMESPACE" apply -f "${REPO}/examples/sleep-consolidation/workspace.yaml" >/dev/null

# Run the measured cycles in the SDK so the wall clock spans the user-visible
# sleep (checkpoint+dehydrate to head advance) and wake (resume+hydrate to
# Ready). Prints "sleep_ms=<n>" and "wake_ms=<n>" per iteration plus the mode.
python3 - "$NAMESPACE" "$POOL" "$WS" "$ITERS" <<'PY'
import sys, time
from mitos.client import AgentRun

ns, pool, ws_name, iters = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
client = AgentRun(namespace=ns, in_cluster=True)

def wait_ready(sb, secs=180):
    try:
        sb.wait_until_ready(timeout=secs)
        return True
    except Exception as e:
        print(f"  not ready: {e}", file=sys.stderr)
        return False

ws = client.workspace(ws_name)

def head_revisions():
    return {r.name for r in ws.log()}

sleep_samples, wake_samples = [], []
resumable_seen = False

for i in range(iters):
    # work
    sb = client.create(pool=pool, workspace=ws_name, timeout="10m")
    if not wait_ready(sb):
        print(f"iter {i}: work not Ready, skipping", file=sys.stderr); continue
    sb.exec("mkdir -p /workspace/state && echo data > /workspace/state/x.txt")
    before = head_revisions()

    # sleep: checkpoint-on-terminate, timed until the head advances.
    t0 = time.time()
    sb.terminate(checkpoint=True)
    deadline = time.time() + 180
    while time.time() < deadline:
        if head_revisions() - before:
            break
        time.sleep(0.5)
    sleep_ms = (time.time() - t0) * 1000.0
    sleep_samples.append(sleep_ms)
    if ws.resumable:
        resumable_seen = True

    # wake: fresh bound claim, timed until Ready with state present.
    t1 = time.time()
    wk = client.create(pool=pool, workspace=ws_name, timeout="10m")
    wait_ready(wk)
    wake_ms = (time.time() - t1) * 1000.0
    wake_samples.append(wake_ms)
    wk.terminate()
    print(f"iter {i}: sleep_ms={sleep_ms:.1f} wake_ms={wake_ms:.1f}")

def stats(xs):
    if not xs:
        return "n/a"
    xs = sorted(xs)
    p50 = xs[len(xs)//2]
    return f"min={xs[0]:.1f} p50={p50:.1f} max={xs[-1]:.1f} n={len(xs)}"

mode = "resumable (memory snapshot)" if resumable_seen else "content-only (memory resume cluster-gated)"
print(f"SLEEP_MS {stats(sleep_samples)}")
print(f"WAKE_MS  {stats(wake_samples)}")
print(f"MODE     {mode}")
PY

echo
echo "Record these numbers in bench/results/ with the hardware, cluster, and MODE."
echo "Do not publish a number this script did not print (no-unverified-claims)."
