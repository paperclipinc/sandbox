#!/usr/bin/env bash
set -euo pipefail

# Creates a kind cluster for local development and testing.
# Runs the controller and a mock forkd (no KVM required).

CLUSTER_NAME="${CLUSTER_NAME:-agent-run-dev}"

echo "==> Creating kind cluster: $CLUSTER_NAME"

cat <<EOF | kind create cluster --name "$CLUSTER_NAME" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels:
      agentrun.dev/kvm: "true"
  - role: worker
    labels:
      agentrun.dev/kvm: "true"
EOF

echo ""
echo "==> Installing CRDs"
kubectl apply -f deploy/crds/ 2>/dev/null || echo "    (CRDs not generated yet; run 'make manifests' first)"

echo ""
echo "==> Cluster ready: $CLUSTER_NAME"
echo ""
echo "    Nodes:"
kubectl get nodes -L agentrun.dev/kvm
echo ""
echo "    Next steps:"
echo "      1. make manifests              # generate CRD yamls"
echo "      2. make build                  # build controller + forkd"
echo "      3. make docker-build           # build container images"
echo "      4. kind load docker-image ghcr.io/agent-run/controller:latest --name $CLUSTER_NAME"
echo "      5. kind load docker-image ghcr.io/agent-run/forkd:latest --name $CLUSTER_NAME"
echo "      6. make install                # deploy to kind"
echo ""
echo "    Or run the controller locally against the kind cluster:"
echo "      go run ./cmd/controller/ --mock"
