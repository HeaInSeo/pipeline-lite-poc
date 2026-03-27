# PROGRESS_LOG

## Day 4 - 2026-03-27

### 오늘 목표
- dag-go Runnable interface 구현하는 SpawnerNode adapter 작성
- dag-go로 single node A 실행 (success/fail 양쪽 검증)

### 변경 파일 목록
**poc:**
- pkg/adapter/spawner_node.go: SpawnerNode (dag-go Runnable 구현)
- cmd/dag-runner/main.go: dag-go 기반 검증 runner
- go.mod / go.sum: dag-go v0.0.9 추가

### 구현한 내용
- SpawnerNode: dag-go.Runnable 구현, RunE = Prepare→Start→Wait
- dag-go 실행 흐름: InitDag → CreateNode → AddEdge(StartNode→node) → FinishDag → ConnectRunner → GetReady → Start → Wait
- 핵심 발견: 단일 노드도 반드시 AddEdge(StartNode, nodeID) 연결 필요
- 실패 전파: job 실패 시 RunE가 error 반환 → dag-go가 InFlightFailed 처리 → Wait returns false

### 검증 명령 및 결과
```bash
go run ./cmd/dag-runner/ success
# [dag-runner] PASS: DAG succeeded (node dag-node-a-ok)

go run ./cmd/dag-runner/ fail
# [dag-runner] FAIL: DAG did not succeed (node dag-node-a-fail)
# exit status 1  ← 예상된 실패
```

### 아직 안 된 것
- A→B→C 파이프라인 (Day 5)
- shared PVC handoff (Day 5)

### 리스크 / 막힌 점
- dag-go 단일 노드: start_node가 orphan 오류 → AddEdge(StartNode, id) 추가로 해결

### 내일 첫 작업 제안
A→B→C 파이프라인: dag-go AddEdge로 선형 체인 구성 + shared PVC로 파일 handoff 검증

---

## Day 3 - 2026-03-27

### 오늘 목표
- spawner K8s driver 최소 구현 (Prepare/Start/Wait/Cancel)
- Job 1개 생성/성공/실패 감지

### 변경 파일 목록
**spawner:**
- pkg/api/types.go: RunSpec에 Command, Labels 추가
- cmd/imp/k8s_driver.go: 실제 구현 (client-go)
- cmd/server/main.go: NewK8sFromKubeconfig으로 변경
- go.mod / go.sum: k8s.io/client-go v0.35.3 추가
- .golangci.yml: lint config

**poc:**
- cmd/runner/main.go: 검증용 테스트 runner
- go.mod: spawner local replace 설정

### 구현한 내용
- RunSpec에 Command, Labels 필드 추가
- DriverK8s: Prepare(Job 객체 생성), Start(K8s 제출), Wait(2s 폴링), Cancel(Job 삭제)
- Kueue 연동: suspend=true + kueue.x-k8s.io/queue-name label 주입
- BackoffLimit=0: 즉시 실패 (fast-fail 기반)
- PVC mount: RunSpec.Mounts → K8s Volume/VolumeMount 변환
- spawner 단독 lint 통과

### 검증 명령 및 결과
```bash
go run ./cmd/runner/ success
# [runner] PASS: job succeeded as expected

go run ./cmd/runner/ fail
# [runner] Result: state=failed
# [runner] PASS: job failed as expected
```

### 아직 안 된 것
- dag-go와 spawner 연결 (Day 4)
- shared PVC 실제 마운트 검증 (Day 5)

### 리스크 / 막힌 점
- K8s BackoffLimit 기본값(6)으로 fail 감지 지연 → BackoffLimit=0으로 해결
- pre-existing lint 이슈 suppress (.golangci.yml 추가)

### 내일 첫 작업 제안
dag-go Runnable interface 구현하는 SpawnerNode adapter 작성 (poc/pkg/adapter/)

---

## Day 2 - 2026-03-27

### 오늘 목표
- kind + Kueue 실습 환경 구성
- Kueue 설치 스크립트/매니페스트
- LocalQueue / ClusterQueue / ResourceFlavor 기본 구성
- 샘플 Job으로 pending/admit 재현

### 변경 파일 목록
- hack/kind-up.sh (신규)
- hack/kind-down.sh (신규)
- deploy/kind/kind-config.yaml (신규)
- deploy/kueue/queues.yaml (신규, v1beta2)
- deploy/kueue/install.sh (신규)
- deploy/poc/poc-pvc.yaml (신규)
- deploy/poc/sample-job.yaml (신규)
- deploy/poc/sample-job-highmem.yaml (신규)
- docs/poc/KUEUE_LAB.md (작성 완료)

### 구현한 내용
- kind wrapper 수정: rootful sudo → rootless podman (user socket)
- kind 클러스터 `poc` 생성 (Kubernetes v1.35.0)
- Kueue v0.16.4 설치 (kubectl apply --server-side, GitHub releases)
- Kueue v1beta2 queue 구성: ResourceFlavor, 2x ClusterQueue, 2x LocalQueue
- sample-job (standard) + sample-job-highmem (highmem) 검증

### 검증 명령 및 결과
```bash
kubectl get workloads -n default
# NAME                           QUEUE             RESERVED IN       ADMITTED   FINISHED
# job-sample-job-1a07a           poc-standard-lq   poc-standard-cq   True       True
# job-sample-job-highmem-36989   poc-highmem-lq    poc-highmem-cq    True       True

kubectl get jobs -n default
# sample-job           Complete   1/1   13s
# sample-job-highmem   Complete   1/1   12s
```

### 아직 안 된 것
- spawner K8s driver 구현 (Day 3)
- PVC 생성 및 마운트 검증 (Day 5)

### 리스크 / 막힌 점
- helm chart repo 접근 불가 → GitHub releases로 우회 해결
- kind rootful podman 미동작 → rootless podman으로 전환 해결
- Kueue v1beta1 deprecated → v1beta2로 전환 해결

### 내일 첫 작업 제안
spawner/cmd/imp/k8s_driver.go에 client-go 추가 및 Job 생성/Wait 최소 구현

<!-- session-end: 2026-03-27 -->

---

## Day 1 - 2026-03-27

### 오늘 목표
- POC_SCOPE.md 작성
- PIPELINE_LITE_SPEC.md 작성
- PROGRESS_LOG.md 생성
- caleb 참고 지점 정리
- 목표/비목표/완료 기준/폐기 기준 고정
- 코드 변경 없음

### 변경 파일 목록
- poc/docs/poc/POC_SCOPE.md (신규)
- poc/docs/poc/PIPELINE_LITE_SPEC.md (신규)
- poc/docs/poc/PROGRESS_LOG.md (신규)
- poc/docs/poc/KUEUE_LAB.md (placeholder 신규)
- poc/docs/poc/POC_EVALUATION.md (placeholder 신규)
- poc/.gitignore (신규)
- poc/README.md (신규)

### 구현한 내용
[Describe: directory structure created, documents written, GitHub repo created at HeaInSeo/pipeline-lite-poc, git initialized and first commit pushed]

### caleb 참고 지점
List the key caleb files referenced:
- caleb/pipeline_v11.jsonc: node 구조, dependsOn, resource limits 참조
- caleb/pipelone_v12.jsonc: executionClass, resourceProfile 개념 참조
- caleb/director.md: 디렉토리 정책 참조
- caleb/thinking.md: enhancement proposals 참조

Key extractions used:
- executionClass → standard/highmem → Kueue LocalQueue 매핑
- resourceProfile → K8s resource requests/limits
- dependsOn → dag-go edge definition
- fan-out → dag-go parallel nodes
- fast-fail → dag-go built-in parent failure propagation

### 검증 명령
None (Day 1 is documentation only)

### 아직 안 된 것
- 모든 Day 2~10 작업

### 리스크 / 막힌 점
- kind가 sudo wrapper로 실행됨 → sudoers SETENV 설정으로 해결 예정 (Day 2)
- spawner go.mod에 client-go 없음 → Day 3에 추가 예정

### 내일 첫 작업 제안
kind 클러스터 생성 (kind-up.sh 작성 및 실행)

<!-- session-end: 2026-03-27 18:59:33 -->

<!-- session-end: 2026-03-27 18:59:51 -->

<!-- session-end: 2026-03-27 19:31:03 -->

<!-- session-end: 2026-03-27 19:31:13 -->

<!-- session-end: 2026-03-27 21:05:18 -->
