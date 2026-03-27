# KUEUE_LAB

Day 2 (2026-03-27) 실습 기록.

## 환경

| 항목 | 값 |
|------|----|
| kind | v0.32.0-alpha (podman rootless) |
| Kubernetes | v1.35.0 |
| Kueue | v0.16.4 |
| 설치 방식 | kubectl apply --server-side (GitHub releases) |

## 설치 과정

### 1. kind 클러스터 생성

```bash
./hack/kind-up.sh
# → kind-poc 클러스터 생성 (17s)
# → kubectl context: kind-poc
```

**주의사항:**
- podman rootless 사용 (`/run/user/1001/podman/podman.sock`)
- sudo 없이 동작 (kind wrapper 수정: `sudo kind` → 직접 실행)
- rootful podman socket(`/run/podman/podman.sock`) 미존재 → rootless로 전환

### 2. Kueue 설치

```bash
kubectl apply --server-side \
  -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.16.4/manifests.yaml
```

**주의사항:**
- helm chart repo(`charts.kueue.sigs.k8s.io`) 접근 불가 → GitHub releases 직접 사용
- API 버전: v1beta1 deprecated → v1beta2 사용
- LocalQueue 필드: `clusterQueueName` → `clusterQueue` (v1beta2 변경)

### 3. Queue 구성

```bash
kubectl apply -f deploy/kueue/queues.yaml
```

생성된 리소스:
- ResourceFlavor: `default-flavor`
- ClusterQueue: `poc-standard-cq` (cpu=4, memory=4Gi)
- ClusterQueue: `poc-highmem-cq` (cpu=2, memory=8Gi)
- LocalQueue: `poc-standard-lq` → poc-standard-cq
- LocalQueue: `poc-highmem-lq` → poc-highmem-cq

## Queue 구조

```
executionClass: standard
  → kueue.x-k8s.io/queue-name: poc-standard-lq
    → ClusterQueue: poc-standard-cq
      → ResourceFlavor: default-flavor
        nominalQuota: cpu=4, memory=4Gi

executionClass: highmem
  → kueue.x-k8s.io/queue-name: poc-highmem-lq
    → ClusterQueue: poc-highmem-cq
      → ResourceFlavor: default-flavor
        nominalQuota: cpu=2, memory=8Gi
```

## pending → admit 검증

### 검증 명령

```bash
kubectl apply -f deploy/poc/sample-job.yaml
kubectl apply -f deploy/poc/sample-job-highmem.yaml
kubectl get workloads -n default
```

### 결과

```
NAME                           QUEUE             RESERVED IN       ADMITTED   FINISHED
job-sample-job-1a07a           poc-standard-lq   poc-standard-cq   True       True
job-sample-job-highmem-36989   poc-highmem-lq    poc-highmem-cq    True       True
```

- standard Job: poc-standard-lq → poc-standard-cq admit → Pod 실행 → Complete (13s)
- highmem Job: poc-highmem-lq → poc-highmem-cq admit → Pod 실행 → Complete (12s)
- 두 Job 모두 `suspend: true`로 생성 → Kueue가 admit 후 `suspend: false`로 변경 → 실행

## PoC Day 3 연결 지점

- Job 생성 시 `kueue.x-k8s.io/queue-name` label 필수
- `suspend: true` 필수 (Kueue admission 전 Pod 생성 방지)
- spawner K8s driver에서 이 두 가지를 Job spec에 주입해야 함
- executionClass → LocalQueue 이름 매핑 로직 필요

## 알려진 이슈 및 해결

| 이슈 | 원인 | 해결 |
|------|------|------|
| kind cluster 생성 실패 | rootful podman socket 없음 | rootless podman socket 사용 |
| LocalQueue 생성 실패 | v1beta1 deprecated field | v1beta2 + `clusterQueue` 필드 사용 |
| helm repo 접근 불가 | 네트워크 제한 | GitHub releases 직접 설치 |
