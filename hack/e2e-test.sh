#!/usr/bin/env bash
set -euo pipefail

# End-to-end test using kind and mock mode.
# Tests the full CRD lifecycle: Template → Pool → Claim → Fork
#
# Prerequisites: kind, kubectl
# No KVM required; runs in mock mode.

CLUSTER_NAME="${CLUSTER_NAME:-sandbox-e2e}"
PASSED=0
FAILED=0

log() { echo "==> $*"; }
pass() { echo "  PASS: $*"; PASSED=$((PASSED + 1)); }
fail() { echo "  FAIL: $*"; FAILED=$((FAILED + 1)); }

cleanup() {
    log "Cleaning up..."
    kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

# --- Setup ---
log "Creating kind cluster: $CLUSTER_NAME"
cat <<EOF | kind create cluster --name "$CLUSTER_NAME" --config=- --wait 60s
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels:
      mitos.run/kvm: "true"
EOF

log "Installing CRDs"
kubectl apply -f deploy/crds/

# Wait for CRDs to be established
sleep 3

# --- Test 1: CRDs are installed ---
log "Test 1: CRDs installed"
for crd in sandboxtemplates sandboxpools sandboxclaims sandboxforks; do
    if kubectl get crd "${crd}.mitos.run" &>/dev/null; then
        pass "CRD ${crd}.mitos.run exists"
    else
        fail "CRD ${crd}.mitos.run not found"
    fi
done

# --- Test 2: Create SandboxTemplate ---
log "Test 2: Create SandboxTemplate"
cat <<EOF | kubectl apply -f -
apiVersion: mitos.run/v1alpha1
kind: SandboxTemplate
metadata:
  name: test-python
spec:
  image: python:3.12-slim
  init:
    - "echo ready"
  resources:
    cpu: "1"
    memory: "512Mi"
  volumes:
    - name: workspace
      mountPath: /workspace
      size: 1Gi
      forkPolicy: Snapshot
    - name: scratch
      mountPath: /tmp
      size: 512Mi
      forkPolicy: Fresh
EOF

if kubectl get sandboxtemplate test-python -o name &>/dev/null; then
    pass "SandboxTemplate created"
else
    fail "SandboxTemplate creation failed"
fi

# --- Test 3: Create SandboxPool ---
log "Test 3: Create SandboxPool"
cat <<EOF | kubectl apply -f -
apiVersion: mitos.run/v1alpha1
kind: SandboxPool
metadata:
  name: test-pool
spec:
  templateRef:
    name: test-python
  replicas: 3
  snapshotAfter: Ready
  scaleDownAfterSnapshot: true
EOF

if kubectl get sandboxpool test-pool -o name &>/dev/null; then
    pass "SandboxPool created"
else
    fail "SandboxPool creation failed"
fi

# Verify pool spec
REPLICAS=$(kubectl get sandboxpool test-pool -o jsonpath='{.spec.replicas}')
if [ "$REPLICAS" = "3" ]; then
    pass "Pool replicas = 3"
else
    fail "Pool replicas expected 3, got $REPLICAS"
fi

# --- Test 4: Create SandboxClaim ---
log "Test 4: Create SandboxClaim"
cat <<EOF | kubectl apply -f -
apiVersion: mitos.run/v1alpha1
kind: SandboxClaim
metadata:
  name: test-claim
spec:
  poolRef:
    name: test-pool
  env:
    - name: SESSION_ID
      value: "e2e-test"
  timeout: 10m
EOF

if kubectl get sandboxclaim test-claim -o name &>/dev/null; then
    pass "SandboxClaim created"
else
    fail "SandboxClaim creation failed"
fi

# Verify claim spec
POOL_REF=$(kubectl get sandboxclaim test-claim -o jsonpath='{.spec.poolRef.name}')
if [ "$POOL_REF" = "test-pool" ]; then
    pass "Claim references correct pool"
else
    fail "Claim pool ref expected test-pool, got $POOL_REF"
fi

# --- Test 5: Create SandboxFork ---
log "Test 5: Create SandboxFork"
cat <<EOF | kubectl apply -f -
apiVersion: mitos.run/v1alpha1
kind: SandboxFork
metadata:
  name: test-fork
spec:
  sourceRef:
    name: test-claim
  replicas: 2
EOF

if kubectl get sandboxfork test-fork -o name &>/dev/null; then
    pass "SandboxFork created"
else
    fail "SandboxFork creation failed"
fi

FORK_REPLICAS=$(kubectl get sandboxfork test-fork -o jsonpath='{.spec.replicas}')
if [ "$FORK_REPLICAS" = "2" ]; then
    pass "Fork replicas = 2"
else
    fail "Fork replicas expected 2, got $FORK_REPLICAS"
fi

# --- Test 6: Verify volume fork policies in template ---
log "Test 6: Volume fork policies"
WS_POLICY=$(kubectl get sandboxtemplate test-python -o jsonpath='{.spec.volumes[0].forkPolicy}')
SCRATCH_POLICY=$(kubectl get sandboxtemplate test-python -o jsonpath='{.spec.volumes[1].forkPolicy}')

if [ "$WS_POLICY" = "Snapshot" ]; then
    pass "Workspace volume forkPolicy = Snapshot"
else
    fail "Workspace forkPolicy expected Snapshot, got $WS_POLICY"
fi

if [ "$SCRATCH_POLICY" = "Fresh" ]; then
    pass "Scratch volume forkPolicy = Fresh"
else
    fail "Scratch forkPolicy expected Fresh, got $SCRATCH_POLICY"
fi

# --- Test 7: Cleanup ---
log "Test 7: Resource deletion"
kubectl delete sandboxfork test-fork
kubectl delete sandboxclaim test-claim
kubectl delete sandboxpool test-pool
kubectl delete sandboxtemplate test-python

for resource in sandboxfork/test-fork sandboxclaim/test-claim sandboxpool/test-pool sandboxtemplate/test-python; do
    if kubectl get "$resource" &>/dev/null 2>&1; then
        fail "Resource $resource still exists after deletion"
    else
        pass "Resource $resource deleted"
    fi
done

# --- Summary ---
echo ""
echo "================================"
echo "  Results: $PASSED passed, $FAILED failed"
echo "================================"

if [ "$FAILED" -gt 0 ]; then
    exit 1
fi
