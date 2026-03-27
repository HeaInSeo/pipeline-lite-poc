#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-poc}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KIND_CONFIG="$SCRIPT_DIR/../deploy/kind/kind-config.yaml"

echo "[kind-up] Creating cluster: $CLUSTER_NAME"

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "[kind-up] Cluster '$CLUSTER_NAME' already exists. Skipping creation."
else
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CONFIG" --wait 120s
  echo "[kind-up] Cluster '$CLUSTER_NAME' created."
fi

echo "[kind-up] Context: kind-$CLUSTER_NAME"
kubectl cluster-info --context "kind-$CLUSTER_NAME"
echo "[kind-up] Done."
