#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-poc}"

echo "[kind-down] Deleting cluster: $CLUSTER_NAME"
kind delete cluster --name "$CLUSTER_NAME"
echo "[kind-down] Done."
