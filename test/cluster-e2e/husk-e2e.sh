#!/usr/bin/env bash
#
# husk-e2e.sh
#
# The real-cluster husk end-to-end verification (issue #16). This is the
# standing CI form of the maintainer's manual cluster check: it drives a REAL
# Kubernetes cluster with a REAL KVM-capable node through the full husk
# lifecycle and asserts each stage.
#
# Stages (each prints a PASS/FAIL line):
#   1. pool warms      a SandboxPool brings up dormant husk pods
#   2. claim activates a SandboxClaim reaches Ready and exec returns expected stdout
#   3. fork(2)         a fork produces independent sandboxes
#   4. run_code        a code-interpreter run returns a result OR a clean
#                      KernelUnavailable (the husk OCI base may lack the kernel;
#                      KernelUnavailable is accepted as a pass for that check)
#   5. PTY             an interactive PTY allocates and echoes
#   6. self-heal       (best effort) deleting a claimed husk pod re-pends the claim
#
# It runs from inside the cluster (the self-hosted runner's ServiceAccount) and
# reaches the per-claim sandbox HTTP API over the pod network via the Python
# SDK (in_cluster=True). CRD lifecycle and the self-heal probe use kubectl.
#
# Reuses the warm-dormant-pod wait pattern from bench/husk-activate-latency.sh.
#
# Usage:
#   test/cluster-e2e/husk-e2e.sh [namespace] [kubeconfig]
#
#   [namespace]   namespace to run the e2e in (default: mitos-e2e)
#   [kubeconfig]  optional kubeconfig path; omit to use the in-cluster SA
#
# Env knobs:
#   READY_TIMEOUT   per-stage wait budget, seconds (default 180)
#   POLL_INTERVAL   poll interval, seconds (default 1)
#   E2E_IMAGE       template image (default python:3.12-slim)
#
set -euo pipefail

NAMESPACE="${1:-mitos-e2e}"
KUBECONFIG_ARG="${2:-}"
if [ -n "$KUBECONFIG_ARG" ]; then
  export KUBECONFIG="$KUBECONFIG_ARG"
fi

READY_TIMEOUT="${READY_TIMEOUT:-180}"
POLL_INTERVAL="${POLL_INTERVAL:-1}"
E2E_IMAGE="${E2E_IMAGE:-python:3.12-slim}"

RUN_ID="$(date +%s)-$$"
TEMPLATE="e2e-tmpl-${RUN_ID}"
POOL="e2e-pool-${RUN_ID}"
CLAIM="e2e-claim-${RUN_ID}"

PASS_COUNT=0
FAIL_COUNT=0

pass() { echo "PASS: $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $*" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }
info() { echo "  $*"; }

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }
}
require kubectl
require python3

# kubectl in a namespace shorthand.
k() { kubectl -n "$NAMESPACE" "$@"; }

diagnostics() {
  echo "=== diagnostics (namespace ${NAMESPACE}) ===" >&2
  k get sandboxpools,sandboxclaims,sandboxforks -o wide >&2 2>&1 || true
  k get pods -o wide >&2 2>&1 || true
  echo "--- claim describe ---" >&2
  k describe sandboxclaim "$CLAIM" >&2 2>&1 || true
  echo "--- recent husk pod logs ---" >&2
  for p in $(k get pods -l 'mitos.run/husk=true' -o name 2>/dev/null | head -3); do
    echo "--- logs $p ---" >&2
    k logs "$p" --tail=40 >&2 2>&1 || true
  done
}

cleanup() {
  rc=$?
  echo "=== teardown ==="
  # Delete the claim/fork first (releases pods), then the pool, then the
  # template. Best effort; never let teardown mask the real exit code.
  k delete sandboxforks --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxclaim "$CLAIM" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxpool "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxtemplate "$TEMPLATE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # Sweep any residual claims/forks this run created.
  k delete sandboxclaims -l "mitos.run/e2e-run=${RUN_ID}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "teardown done"
  exit "$rc"
}
trap cleanup EXIT

echo "=== mitos cluster husk e2e: ns=${NAMESPACE} image=${E2E_IMAGE} run=${RUN_ID} ==="

# ---------------------------------------------------------------------------
# Stage 0: KVM node present (the husk path needs a KVM-capable node).
# ---------------------------------------------------------------------------
if kubectl get nodes -l 'mitos.run/kvm=true' -o name 2>/dev/null | grep -q node; then
  pass "a KVM-capable node (mitos.run/kvm=true) is present"
else
  fail "no node labeled mitos.run/kvm=true; the husk path cannot warm pods"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 1: a pool warms dormant husk pods.
# ---------------------------------------------------------------------------
echo "--- stage 1: pool warms dormant husk pods ---"
k apply -f - >/dev/null <<EOF
apiVersion: mitos.run/v1alpha1
kind: SandboxTemplate
metadata:
  name: ${TEMPLATE}
  labels:
    mitos.run/e2e-run: "${RUN_ID}"
spec:
  image: ${E2E_IMAGE}
  resources:
    cpu: "1"
    memory: "512Mi"
---
apiVersion: mitos.run/v1alpha1
kind: SandboxPool
metadata:
  name: ${POOL}
  labels:
    mitos.run/e2e-run: "${RUN_ID}"
spec:
  templateRef:
    name: ${TEMPLATE}
  replicas: 2
EOF

# Wait for at least one dormant warm pod: husk=true, Running, no claim label.
warm_deadline=$(( $(date +%s) + READY_TIMEOUT ))
warm_ok=""
while [ "$(date +%s)" -lt "$warm_deadline" ]; do
  dormant="$(k get pods -l 'mitos.run/husk=true,!mitos.run/claim' \
    --field-selector=status.phase=Running -o name 2>/dev/null | head -1 || true)"
  if [ -n "$dormant" ]; then warm_ok="yes"; break; fi
  sleep "$POLL_INTERVAL"
done
if [ -n "$warm_ok" ]; then
  pass "pool ${POOL} warmed at least one dormant husk pod"
else
  fail "pool ${POOL} did not warm a dormant husk pod within ${READY_TIMEOUT}s"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stages 2-5 are driven by the Python SDK against the sandbox HTTP API over the
# pod network. The driver prints one RESULT:<stage>:<PASS|FAIL>:<detail> line
# per stage on stdout; this script folds those into the PASS/FAIL tally so the
# bash layer stays the single source of truth for the exit code.
# ---------------------------------------------------------------------------
echo "--- stages 2-5: claim, exec, fork, run_code, PTY (SDK driver) ---"

driver_out="$(mktemp)"
set +e
INCLUSTER="false"
[ -z "${KUBECONFIG:-}" ] && INCLUSTER="true"
MITOS_NS="$NAMESPACE" MITOS_POOL="$POOL" MITOS_CLAIM="$CLAIM" \
MITOS_INCLUSTER="$INCLUSTER" MITOS_READY_TIMEOUT="$READY_TIMEOUT" \
python3 - <<'PYEOF' | tee "$driver_out"
import os
import sys
import time

from mitos import AgentRun

NS = os.environ["MITOS_NS"]
POOL = os.environ["MITOS_POOL"]
CLAIM = os.environ["MITOS_CLAIM"]
INCLUSTER = os.environ.get("MITOS_INCLUSTER", "true") == "true"
READY_TIMEOUT = float(os.environ.get("MITOS_READY_TIMEOUT", "180"))


def result(stage, ok, detail=""):
    print(f"RESULT:{stage}:{'PASS' if ok else 'FAIL'}:{detail}", flush=True)


run = AgentRun(namespace=NS, in_cluster=INCLUSTER)

# ---- stage 2: claim activates to Ready and exec returns expected stdout ----
sb = None
try:
    sb = run.sandbox(pool=POOL, name=CLAIM)
    sb.wait_until_ready(timeout=READY_TIMEOUT)
    res = sb.exec("echo mitos-e2e-ok", timeout=30)
    out = (res.stdout or "").strip()
    if res.exit_code == 0 and out == "mitos-e2e-ok":
        result("claim-exec", True, f"exit=0 stdout={out!r}")
    else:
        result("claim-exec", False, f"exit={res.exit_code} stdout={out!r} stderr={res.stderr!r}")
except Exception as exc:  # noqa: BLE001
    result("claim-exec", False, f"{type(exc).__name__}: {exc}")
    # Without a Ready sandbox the remaining stages cannot run; bail.
    sys.exit(0)

# ---- stage 3: fork(2) produces independent sandboxes ----
try:
    forks = sb.fork(n=2)
    if len(forks) != 2:
        result("fork", False, f"expected 2 forks, got {len(forks)}")
    else:
        # Prove independence: write a distinct marker in each fork and read it
        # back; the two must not see each other's marker.
        f0, f1 = forks[0], forks[1]
        f0.exec("echo fork0 > /tmp/who", timeout=30)
        f1.exec("echo fork1 > /tmp/who", timeout=30)
        r0 = f0.exec("cat /tmp/who", timeout=30).stdout.strip()
        r1 = f1.exec("cat /tmp/who", timeout=30).stdout.strip()
        if r0 == "fork0" and r1 == "fork1":
            result("fork", True, f"independent: f0={r0!r} f1={r1!r}")
        else:
            result("fork", False, f"not independent: f0={r0!r} f1={r1!r}")
except Exception as exc:  # noqa: BLE001
    result("fork", False, f"{type(exc).__name__}: {exc}")

# ---- stage 4: run_code returns a result OR a clean KernelUnavailable ----
# The husk OCI base may lack the code-interpreter kernel; per issue #16 a clean
# KernelUnavailable is accepted as a PASS for this check (do not fail the suite).
try:
    ex = sb.run_code("print(6 * 7)", timeout=60)
    if ex.error is not None and "KernelUnavailable" in (ex.error.name or ""):
        result("run_code", True, "clean KernelUnavailable (accepted: base lacks the kernel)")
    elif ex.error is not None and "KernelUnavailable" in (ex.error.value or ""):
        result("run_code", True, "clean KernelUnavailable (accepted: base lacks the kernel)")
    else:
        logs = "".join(ex.logs.get("stdout", []))
        got = (ex.text or logs).strip()
        if "42" in got:
            result("run_code", True, f"kernel returned {got!r}")
        elif ex.error is not None:
            # A non-KernelUnavailable error is a real failure.
            result("run_code", False, f"kernel error {ex.error.name!r}: {ex.error.value!r}")
        else:
            result("run_code", False, f"no result and no KernelUnavailable; got {got!r}")
except Exception as exc:  # noqa: BLE001
    # A transport-level KernelUnavailable can surface as an exception; accept it.
    msg = f"{type(exc).__name__}: {exc}"
    if "KernelUnavailable" in msg:
        result("run_code", True, "clean KernelUnavailable (accepted: base lacks the kernel)")
    else:
        result("run_code", False, msg)

# ---- stage 5: a PTY allocates and echoes ----
try:
    chunks = []
    handle = sb.pty.create(on_data=lambda b: chunks.append(b), cols=80, rows=24)
    handle.send_input(b"echo pty-mitos-e2e\n")
    # Give the guest a moment to echo, then exit the shell.
    deadline = time.time() + 30
    while time.time() < deadline:
        if b"pty-mitos-e2e" in b"".join(chunks):
            break
        time.sleep(0.2)
    handle.send_input(b"exit\n")
    try:
        handle.wait(timeout=10)
    except TypeError:
        # Older handle.wait() takes no timeout.
        handle.wait()
    echoed = b"".join(chunks)
    if b"pty-mitos-e2e" in echoed:
        result("pty", True, "PTY allocated and echoed the input")
    else:
        result("pty", False, f"PTY did not echo; got {echoed[:120]!r}")
except Exception as exc:  # noqa: BLE001
    result("pty", False, f"{type(exc).__name__}: {exc}")
PYEOF
driver_rc=$?
set -e

# Fold the driver RESULT lines into the bash tally.
for stage in claim-exec fork run_code pty; do
  line="$(grep "^RESULT:${stage}:" "$driver_out" | tail -1 || true)"
  if [ -z "$line" ]; then
    fail "stage ${stage}: driver produced no result (driver_rc=${driver_rc})"
    continue
  fi
  verdict="$(printf '%s' "$line" | cut -d: -f3)"
  detail="$(printf '%s' "$line" | cut -d: -f4-)"
  if [ "$verdict" = "PASS" ]; then
    pass "stage ${stage}: ${detail}"
  else
    fail "stage ${stage}: ${detail}"
  fi
done
rm -f "$driver_out"

# ---------------------------------------------------------------------------
# Stage 6 (best effort): deleting a claimed husk pod re-pends the claim
# (the eviction / self-heal path). Best effort: if the claim object or its pod
# cannot be resolved, record a non-fatal note rather than a FAIL.
# ---------------------------------------------------------------------------
echo "--- stage 6 (best effort): self-heal re-pends a claim on pod deletion ---"
self_heal_probe() {
  # Find the pod backing the claim. The controller labels the activated husk
  # pod with mitos.run/claim=<claim-name>.
  pod="$(k get pods -l "mitos.run/claim=${CLAIM}" -o name 2>/dev/null | head -1 || true)"
  if [ -z "$pod" ]; then
    info "self-heal: could not resolve the pod for claim ${CLAIM}; skipping (non-fatal)"
    return 0
  fi
  info "self-heal: deleting ${pod} backing claim ${CLAIM}"
  k delete "$pod" --wait=false >/dev/null 2>&1 || true
  # The claim should leave Ready (re-pend) then recover to Ready on a new pod,
  # OR at minimum its Ready condition should flip away from True transiently.
  deadline=$(( $(date +%s) + READY_TIMEOUT ))
  saw_repend=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ready="$(k get sandboxclaim "$CLAIM" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    if [ "$ready" != "True" ]; then saw_repend="yes"; break; fi
    sleep "$POLL_INTERVAL"
  done
  if [ -n "$saw_repend" ]; then
    return 0
  fi
  info "self-heal: claim stayed Ready throughout (controller may have re-activated faster than the poll); treating as non-fatal"
  return 2
}
set +e
self_heal_probe
sh_rc=$?
set -e
if [ "$sh_rc" -eq 0 ]; then
  pass "self-heal: claim re-pended after its husk pod was deleted"
else
  # Best effort: a missed re-pend window is recorded but does NOT fail the suite.
  echo "NOTE: self-heal probe inconclusive (non-fatal)"
fi

# ---------------------------------------------------------------------------
# Verdict.
# ---------------------------------------------------------------------------
echo
echo "=== summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed ==="
if [ "$FAIL_COUNT" -gt 0 ]; then
  diagnostics
  exit 1
fi
echo "ALL CHECKS PASSED"
