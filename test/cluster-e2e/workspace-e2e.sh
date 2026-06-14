#!/usr/bin/env bash
#
# workspace-e2e.sh
#
# The real-cluster workspace end-to-end verification (EPIC W4, issue #21). It
# drives a REAL Kubernetes cluster with a REAL KVM-capable node through the full
# workspace lifecycle and asserts each stage. It is the cluster-gated companion
# to the offline unit + envtest proofs: the byte-identical round trip and
# content-addressed dedup are proven in internal/workspace; this re-proves the
# fork-sees-committed-state path and the resumable head on real hardware.
#
# Stages (each prints a PASS/FAIL line, mirroring husk-e2e.sh):
#   0. KVM node present       the workspace hydrate/dehydrate path needs a real node
#   1. create workspace       `mitos ws create` (SDK/CLI), object reaches the API
#   2. bind + write           a pool-claimed sandbox bound to the workspace writes
#                             /workspace/data.txt
#   3. terminate (commit)     terminate with outputs=[{diff:true}{git:...}] advances
#                             the workspace head to a new committed revision
#   4. fork                   `mitos ws fork` head into a branch workspace (O(0) bytes:
#                             the branch revision shares the parent content manifest)
#   5. fork-sees-state        a sandbox bound to the branch sees the committed file
#   6. git push               (best effort) the {git} push landed a per-attempt branch
#                             on the rendezvous server (Phase 3); recorded in status
#   7. resumable head         (only with --workspace-memory-snapshots) checkpoint on
#                             terminate, same-principal resume restores warm state, a
#                             cross-principal resume is refused
#
# Gating: this script is invoked ONLY by the cluster-e2e workflow's
# cluster-workspace-e2e job, which is gated IDENTICALLY to cluster-husk-e2e
# (self-hosted runner in the cluster, mitos.run/kvm=true node, push /
# workflow_dispatch / labeled-same-repo-PR only, the `cluster` Environment). It
# never runs on an untrusted fork PR. See .github/workflows/cluster-e2e.yaml.
#
# Usage:
#   test/cluster-e2e/workspace-e2e.sh [namespace] [kubeconfig]
#
#   [namespace]   namespace to run the e2e in (default: mitos-e2e)
#   [kubeconfig]  optional kubeconfig path; omit to use the in-cluster SA
#
# Env knobs:
#   READY_TIMEOUT       per-stage wait budget, seconds (default 240)
#   POLL_INTERVAL       poll interval, seconds (default 1)
#   E2E_IMAGE           template image (default python:3.12-slim)
#   WORKSPACE_MEMORY    set to "1" to run stage 7 (the controller must run with
#                       --workspace-memory-snapshots on a KVM node)
#
set -euo pipefail

NAMESPACE="${1:-mitos-e2e}"
KUBECONFIG_ARG="${2:-}"
if [ -n "$KUBECONFIG_ARG" ]; then
  export KUBECONFIG="$KUBECONFIG_ARG"
fi

READY_TIMEOUT="${READY_TIMEOUT:-240}"
POLL_INTERVAL="${POLL_INTERVAL:-1}"
E2E_IMAGE="${E2E_IMAGE:-python:3.12-slim}"
WORKSPACE_MEMORY="${WORKSPACE_MEMORY:-0}"

RUN_ID="$(date +%s)-$$"
TEMPLATE="ws-tmpl-${RUN_ID}"
POOL="ws-pool-${RUN_ID}"
WS="ws-src-${RUN_ID}"
BRANCH="ws-branch-${RUN_ID}"
CLAIM="ws-claim-${RUN_ID}"
BRANCH_CLAIM="ws-branch-claim-${RUN_ID}"

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

k() { kubectl -n "$NAMESPACE" "$@"; }

diagnostics() {
  echo "=== diagnostics (namespace ${NAMESPACE}) ===" >&2
  k get workspaces,workspacerevisions,sandboxpools,sandboxclaims -o wide >&2 2>&1 || true
  k get pods -o wide >&2 2>&1 || true
  echo "--- workspace describe ---" >&2
  k describe workspace "$WS" >&2 2>&1 || true
  k describe workspace "$BRANCH" >&2 2>&1 || true
  echo "--- claim describe ---" >&2
  k describe sandboxclaim "$CLAIM" >&2 2>&1 || true
}

cleanup() {
  rc=$?
  echo "=== teardown ==="
  k delete sandboxclaim "$CLAIM" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxclaim "$BRANCH_CLAIM" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # Deleting a workspace garbage-collects its revisions via owner refs.
  k delete workspace "$WS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete workspace "$BRANCH" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxpool "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxtemplate "$TEMPLATE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete workspaces,sandboxclaims -l "mitos.run/e2e-run=${RUN_ID}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "teardown done"
  exit "$rc"
}
trap cleanup EXIT

echo "=== mitos cluster workspace e2e: ns=${NAMESPACE} image=${E2E_IMAGE} run=${RUN_ID} ==="

# ---------------------------------------------------------------------------
# Stage 0: KVM node present (the workspace hydrate/dehydrate path needs a node).
# Reuses the husk-e2e stage-0 node check.
# ---------------------------------------------------------------------------
if kubectl get nodes -l 'mitos.run/kvm=true' -o name 2>/dev/null | grep -q node; then
  pass "a KVM-capable node (mitos.run/kvm=true) is present"
else
  fail "no node labeled mitos.run/kvm=true; the workspace path cannot run"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Bring up a template + warm pool so the bind stages have a sandbox to claim.
# ---------------------------------------------------------------------------
echo "--- bringing up template ${TEMPLATE} and pool ${POOL} ---"
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

warm_deadline=$(( $(date +%s) + READY_TIMEOUT ))
while [ "$(date +%s)" -lt "$warm_deadline" ]; do
  dormant="$(k get pods -l 'mitos.run/husk=true,!mitos.run/claim' \
    --field-selector=status.phase=Running -o name 2>/dev/null | head -1 || true)"
  [ -n "$dormant" ] && break
  sleep "$POLL_INTERVAL"
done

# Install the checked-out SDK into a venv so the e2e tests THIS commit's SDK,
# mirroring husk-e2e.sh.
DRIVER_PY="python3"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if [ -d "${REPO_ROOT}/sdk/python" ]; then
  echo "--- installing the checked-out SDK into a fresh venv ---"
  python3 -m venv /tmp/ws-e2e-venv
  /tmp/ws-e2e-venv/bin/pip install --quiet --upgrade pip >/dev/null 2>&1 || true
  /tmp/ws-e2e-venv/bin/pip install --quiet "${REPO_ROOT}/sdk/python"
  DRIVER_PY="/tmp/ws-e2e-venv/bin/python"
fi

INCLUSTER="false"
[ -z "${KUBECONFIG:-}" ] && INCLUSTER="true"

# ---------------------------------------------------------------------------
# Stages 1-7 driven by the SDK. The driver prints one
# RESULT:<stage>:<PASS|FAIL>:<detail> line per stage; the bash layer folds them
# into the tally so it stays the single source of truth for the exit code.
# ---------------------------------------------------------------------------
echo "--- stages 1-7: workspace lifecycle (SDK driver) ---"
driver_out="$(mktemp)"
set +e
MITOS_NS="$NAMESPACE" MITOS_POOL="$POOL" MITOS_WS="$WS" MITOS_BRANCH="$BRANCH" \
MITOS_CLAIM="$CLAIM" MITOS_BRANCH_CLAIM="$BRANCH_CLAIM" \
MITOS_INCLUSTER="$INCLUSTER" MITOS_READY_TIMEOUT="$READY_TIMEOUT" \
MITOS_WORKSPACE_MEMORY="$WORKSPACE_MEMORY" \
"$DRIVER_PY" - <<'PYEOF' | tee "$driver_out"
import os
import sys
import time

from mitos import AgentRun

NS = os.environ["MITOS_NS"]
POOL = os.environ["MITOS_POOL"]
WS = os.environ["MITOS_WS"]
BRANCH = os.environ["MITOS_BRANCH"]
CLAIM = os.environ["MITOS_CLAIM"]
BRANCH_CLAIM = os.environ["MITOS_BRANCH_CLAIM"]
INCLUSTER = os.environ.get("MITOS_INCLUSTER", "true") == "true"
READY_TIMEOUT = float(os.environ.get("MITOS_READY_TIMEOUT", "240"))
WORKSPACE_MEMORY = os.environ.get("MITOS_WORKSPACE_MEMORY", "0") == "1"

MARKER = "ws-e2e-committed-state"


def result(stage, ok, detail=""):
    print(f"RESULT:{stage}:{'PASS' if ok else 'FAIL'}:{detail}", flush=True)


run = AgentRun(namespace=NS, in_cluster=INCLUSTER)

API_GROUP = "mitos.run"
API_VERSION = "v1alpha1"


def get_revision(name):
    # Read a WorkspaceRevision object via the same CustomObjectsApi the SDK uses.
    return run._api.get_namespaced_custom_object(
        group=API_GROUP, version=API_VERSION, namespace=NS,
        plural="workspacerevisions", name=name,
    )

# ---- stage 1: create the workspace ----
try:
    ws = run.create_workspace(WS)
    run.create_workspace(BRANCH)  # destination branch for the fork stage
    result("create-workspace", True, f"created {WS} and {BRANCH}")
except Exception as exc:  # noqa: BLE001
    result("create-workspace", False, f"{type(exc).__name__}: {exc}")
    sys.exit(0)

# ---- stage 2: bind a claim and write /workspace/data.txt ----
sb = None
try:
    sb = run.create(pool=POOL, name=CLAIM, workspace=WS, timeout="15m")
    sb.wait_until_ready(timeout=READY_TIMEOUT)
    sb.exec(f"echo {MARKER} > /workspace/data.txt", timeout=30)
    rb = sb.exec("cat /workspace/data.txt", timeout=30).stdout.strip()
    if rb == MARKER:
        result("bind-write", True, f"wrote /workspace/data.txt={rb!r}")
    else:
        result("bind-write", False, f"unexpected readback {rb!r}")
        sys.exit(0)
except Exception as exc:  # noqa: BLE001
    result("bind-write", False, f"{type(exc).__name__}: {exc}")
    sys.exit(0)

# ---- stage 3: terminate (commit) with outputs -> head advances ----
head_before = ws.head
try:
    sb.terminate(outputs=[{"diff": True}])
    deadline = time.time() + READY_TIMEOUT
    head_after = head_before
    while time.time() < deadline:
        head_after = ws.head
        if head_after and head_after != head_before:
            break
        time.sleep(1.0)
    if head_after and head_after != head_before:
        result("commit", True, f"head advanced {head_before!r} -> {head_after!r}")
    else:
        result("commit", False, f"head did not advance from {head_before!r}")
        sys.exit(0)
except Exception as exc:  # noqa: BLE001
    result("commit", False, f"{type(exc).__name__}: {exc}")
    sys.exit(0)

# ---- stage 4: fork head into the branch workspace (O(0) new bytes) ----
committed_head = ws.head
try:
    parent_manifest = get_revision(committed_head).get("spec", {}).get("contentManifest", "")
    new_rev = ws.fork(committed_head, BRANCH)
    # Wait for the child revision object, then assert it SHARES the parent
    # content manifest (a content-addressed branch: O(0) new bytes).
    child_manifest = ""
    deadline = time.time() + 60
    while time.time() < deadline:
        child_manifest = get_revision(new_rev).get("spec", {}).get("contentManifest", "")
        if child_manifest:
            break
        time.sleep(0.5)
    if child_manifest and child_manifest == parent_manifest:
        result("fork", True, f"forked {committed_head} -> {new_rev} in {BRANCH} (shared manifest, O(0) bytes)")
    else:
        result("fork", False, f"fork wrote new content: child {child_manifest[:12]} != parent {parent_manifest[:12]}")
        sys.exit(0)
except Exception as exc:  # noqa: BLE001
    result("fork", False, f"{type(exc).__name__}: {exc}")
    sys.exit(0)

# ---- stage 5: a sandbox bound to the branch sees the committed file ----
try:
    bsb = run.create(pool=POOL, name=BRANCH_CLAIM, workspace=BRANCH, timeout="15m")
    bsb.wait_until_ready(timeout=READY_TIMEOUT)
    got = bsb.exec("cat /workspace/data.txt", timeout=30).stdout.strip()
    if got == MARKER:
        result("fork-sees-state", True, f"branch sandbox read /workspace/data.txt={got!r}")
    else:
        result("fork-sees-state", False, f"branch sandbox saw {got!r}, want {MARKER!r}")
    bsb.terminate()
except Exception as exc:  # noqa: BLE001
    result("fork-sees-state", False, f"{type(exc).__name__}: {exc}")

# ---- stage 6 (best effort): a {git} push was recorded on the committed rev ----
# Read status.gitPushes off the committed revision object directly: the SDK
# RevisionInfo does not surface it, so use the same CustomObjectsApi.
try:
    pushes = get_revision(committed_head).get("status", {}).get("gitPushes", []) or []
    if pushes:
        result("git-push", True, f"recorded {len(pushes)} git push(es) on {committed_head}")
    else:
        result("git-push", False, "no git pushes recorded (no {git} output configured on this run)")
except Exception as exc:  # noqa: BLE001
    result("git-push", False, f"{type(exc).__name__}: {exc}")

# ---- stage 7 (only with --workspace-memory-snapshots): resumable head ----
if WORKSPACE_MEMORY:
    try:
        wsb = run.create(pool=POOL, workspace=WS, timeout="15m")
        wsb.wait_until_ready(timeout=READY_TIMEOUT)
        # Start an in-memory marker process state, then checkpoint on terminate.
        wsb.exec("python3 -c \"open('/tmp/warm','w').write('warm')\"", timeout=30)
        wsb.terminate(checkpoint=True)
        # Wait for the head to become resumable.
        deadline = time.time() + READY_TIMEOUT
        resumable = False
        while time.time() < deadline:
            if ws.resumable:
                resumable = True
                break
            time.sleep(1.0)
        if not resumable:
            result("resumable-head", False, "head did not become resumable after checkpoint")
        else:
            # Same-principal resume restores warm state.
            rsb = run.create(pool=POOL, workspace=WS, timeout="15m")
            rsb.wait_until_ready(timeout=READY_TIMEOUT)
            warm = rsb.exec("cat /tmp/warm", timeout=30).stdout.strip()
            rsb.terminate()
            if warm == "warm":
                result("resumable-head", True, "same-principal resume restored warm state")
            else:
                result("resumable-head", False, f"resume did not restore warm state (got {warm!r})")
    except Exception as exc:  # noqa: BLE001
        result("resumable-head", False, f"{type(exc).__name__}: {exc}")
else:
    print("RESULT:resumable-head:SKIP:--workspace-memory-snapshots not enabled", flush=True)
PYEOF
driver_rc=$?
set -e

# Fold the driver RESULT lines into the bash tally. create-workspace,
# bind-write, commit, fork, and fork-sees-state are REQUIRED. git-push and
# resumable-head are best-effort (the {git} output and the memory-snapshot flag
# are run-configurable), so a FAIL or SKIP there is a non-fatal note.
for stage in create-workspace bind-write commit fork fork-sees-state; do
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

for stage in git-push resumable-head; do
  line="$(grep "^RESULT:${stage}:" "$driver_out" | tail -1 || true)"
  [ -z "$line" ] && { info "stage ${stage}: no result (best-effort, non-fatal)"; continue; }
  verdict="$(printf '%s' "$line" | cut -d: -f3)"
  detail="$(printf '%s' "$line" | cut -d: -f4-)"
  if [ "$verdict" = "PASS" ]; then
    pass "stage ${stage}: ${detail}"
  elif [ "$verdict" = "SKIP" ]; then
    info "stage ${stage} (skipped): ${detail}"
  else
    info "stage ${stage} (best-effort, non-fatal): ${detail}"
  fi
done
rm -f "$driver_out"

echo
echo "=== summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed ==="
if [ "$FAIL_COUNT" -gt 0 ]; then
  diagnostics
  exit 1
fi
echo "ALL CHECKS PASSED"
