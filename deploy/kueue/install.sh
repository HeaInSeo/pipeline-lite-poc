#!/usr/bin/env bash
set -euo pipefail

# Kueue 설치 스크립트
# helm repo가 접근 불가한 환경에서는 GitHub releases에서 직접 설치
# 검증된 버전: v0.16.4 (Kubernetes v1.35.0 호환)

KUEUE_VERSION="${KUEUE_VERSION:-v0.16.4}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "[kueue-install] Installing Kueue $KUEUE_VERSION via kubectl apply"
kubectl apply --server-side \
  -f "https://github.com/kubernetes-sigs/kueue/releases/download/${KUEUE_VERSION}/manifests.yaml"

echo "[kueue-install] Waiting for controller to be ready..."
kubectl wait --for=condition=available \
  deployment/kueue-controller-manager \
  -n kueue-system \
  --timeout=120s

echo "[kueue-install] Kueue ready. Pods:"
kubectl get pods -n kueue-system

echo "[kueue-install] Applying queue manifests..."
kubectl apply -f "$SCRIPT_DIR/queues.yaml"

echo "[kueue-install] Queues:"
kubectl get resourceflavor,clusterqueue,localqueue -A
echo "[kueue-install] Done."
