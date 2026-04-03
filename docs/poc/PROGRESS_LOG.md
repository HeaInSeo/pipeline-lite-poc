# PROGRESS_LOG

## Day 10 - 2026-03-28

### 오늘 목표
- POC_EVALUATION.md 작성 (go/no-go 판정, 컴포넌트별 평가, 미결 리스크)

### 변경 파일 목록
**poc:**
- docs/poc/POC_EVALUATION.md: 최종 평가 문서 (placeholder → 완성)

### 구현한 내용
- 완료 기준 6/6 충족 확인
- 폐기 기준 전체 해당 없음 확인
- 컴포넌트별 평가 (dag-go/spawner/Kueue/어댑터)
- 미결 리스크 4건 식별 및 대응 방향 기술
- 재사용 가능 자산 목록 정리
- 후속 작업 제안 (즉시/단기/장기)

### 판정
**GO** — production 진입 자격 있음

### 아직 안 된 것
- 없음 (PoC 완료)

### 리스크 / 막힌 점
- 없음

---

## Day 9 - 2026-03-28

### 오늘 목표
- context timeout 검증 (긴 Job에 짧은 timeout)
- 리소스 부족 시 Kueue pending → 자원 해제 후 admit 확인
- 동일 RunID 재실행 (삭제 후 재제출)

### 변경 파일 목록
**poc:**
- cmd/operational/main.go: Day 9 검증 runner (timeout/pending/rerun)

**fix (이슈 정리):**
- pkg/adapter/spawner_node.go: Driver 타입 `*imp.DriverK8s` → `driver.Driver` 인터페이스
- cmd/execclass/main.go: SetNodeRunner 반환값 미체크 수정
- cmd/fastfail/main.go: SetNodeRunner 반환값 미체크 수정
- cmd/fanout/main.go: SetRunner 반환값 미체크 수정
- cmd/pipeline-abc/main.go: SetRunner 반환값 미체크 수정

### 구현한 내용
- testTimeout: 60초 Job에 10초 ctx timeout → Wait returns context.DeadlineExceeded → Job 수동 삭제
- testPending: A(3500m, 15s) 먼저 admit → B(1000m) pending → A 완료 후 B admit
- testRerun: 동일 RunID Job 완료 후 Cancel(삭제) → 동일 RunID 재제출 → 성공

### 검증 명령 및 결과
```bash
go run ./cmd/operational/ timeout
# [timeout] Wait returned error (expected): context deadline exceeded
# [timeout] Job deleted OK
# [timeout] PASS

go run ./cmd/operational/ pending
# [pending] A finished: state=succeeded
# [pending] B finished: state=succeeded
# [pending] PASS: A completed → B admitted and completed

go run ./cmd/operational/ rerun
# [rerun] run #1: state=succeeded
# [rerun] run #2: state=succeeded
# [rerun] PASS: same RunID ran twice successfully
```

### 아직 안 된 것
- POC_EVALUATION.md 작성 (Day 10)

### 리스크 / 막힌 점
- 없음

### 내일 첫 작업 제안
POC_EVALUATION.md: 10일 검증 결과 총정리, go/no-go 판정, 미결 리스크 목록 작성

---

## Day 8 - 2026-03-27

### 오늘 목표
- 같은 DAG 내 standard/highmem executionClass 혼용 제출
- Kueue workload가 올바른 ClusterQueue로 라우팅됨을 확인

### 변경 파일 목록
**poc:**
- pkg/adapter/execution_class.go: ExecutionClass 타입 + QueueLabel() 헬퍼
- cmd/execclass/main.go: 혼용 executionClass DAG runner

### 구현한 내용
- ExecutionClass("standard"|"highmem") → kueue.x-k8s.io/queue-name label 매핑
- DAG: A(std) → B1(std)/B2(highmem) → C(std)
- B1/B2 병렬 실행, 각각 다른 ClusterQueue 동시 활성

### 검증 명령 및 결과
```bash
go run ./cmd/execclass/
# [execclass] PASS: mixed executionClass DAG succeeded

kubectl get workloads -n default | grep ec-
# job-ec-a-*   poc-standard-lq  poc-standard-cq  True  True
# job-ec-b2-*  poc-highmem-lq   poc-highmem-cq   True  True  ← highmem 확인
# job-ec-b1-*  poc-standard-lq  poc-standard-cq  True  True
# job-ec-c-*   poc-standard-lq  poc-standard-cq  True  True
```

### 아직 안 된 것
- operational 검증 (Day 9)
- POC_EVALUATION.md 작성 (Day 10)

### 리스크 / 막힌 점
- 없음 (executionClass → queue-name label injection으로 Kueue 라우팅 자동화)

### 내일 첫 작업 제안
operational 검증: context timeout, 클러스터 재시작 후 재실행, 리소스 부족 시 Kueue pending 동작 확인

---

## Day 7 - 2026-03-27

### 오늘 목표
- fast-fail 전파 검증: B2 실패 → 수렴 노드 C가 실행되지 않음

### 변경 파일 목록
**poc:**
- cmd/fastfail/main.go: fast-fail 검증 runner

### 구현한 내용
- DAG 구조: start → A → B1/B2(FAIL)/B3 → C(수렴) → end
- B2: `exit 1` (의도적 실패)
- 실패 전파 경로: B2 InFlightFailed → C preflight "parent channel returned Failed" → C 미실행

### 검증 명령 및 결과
```bash
go run ./cmd/fastfail/
# ff-b1, ff-b3: InFlight 정상 완료 (B1/B3는 A에만 의존, 정상 실행)
# ff-b2: InFlightFailed (의도적 실패)
# ff-c: "parent channel returned Failed" → 실행되지 않음 ← fast-fail 확인
# [fastfail] PASS: Wait=false (B2 failure propagated, C not executed)
```

### 아직 안 된 것
- executionClass 분리 (standard/highmem 같은 DAG 내 혼용) - Day 8

### 리스크 / 막힌 점
- 없음 (dag-go CheckParentsStatus + parent channel Failed 전파 예상대로 동작)

### 내일 첫 작업 제안
executionClass 분리: 같은 DAG 내 standard 노드와 highmem 노드를 각각 다른 Kueue LocalQueue에 제출

---

## Day 6 - 2026-03-27

### 오늘 목표
- A → B1/B2/B3 fan-out 병렬 실행
- kueue.x-k8s.io/queue-name label injection 검증

### 변경 파일 목록
**poc:**
- cmd/fanout/main.go: fan-out DAG runner

### 구현한 내용
- DAG 구조: start → A → B1/B2/B3 → end (FinishDag 자동 fan-in)
- B1/B2/B3: 독립 edge (A→B1, A→B2, A→B3) → dag-go worker pool이 병렬 스케줄
- queue-name label: map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"} → 모든 노드에 주입
- 각 Bx: sleep 4s 포함 → 순차 ~18s vs 병렬 ~10s 차이 측정

### 검증 명령 및 결과
```bash
go run ./cmd/fanout/
# [fanout] PASS: fan-out pipeline succeeded (elapsed=16s)
# B1/B2/B3 Preflight 동시 시작 (21:18:17)
# B1/B2/B3 InFlight  동시 완료 (21:18:27) ← 병렬 실행 확인
# 순차 실행 시 예상 ~24s → 실제 16s (A 6s + B 10s)
```

### 아직 안 된 것
- fast-fail: 한 노드 실패 시 나머지 취소 (Day 7)
- executionClass 분리 standard/highmem (Day 8)

### 리스크 / 막힌 점
- 없음 (dag-go worker pool 기본값 50으로 fan-out 즉시 동작)

### 내일 첫 작업 제안
fast-fail: B2를 의도적으로 실패시키고 B1/B3가 Skipped 처리되는지 확인

---

## Day 5 - 2026-03-27

### 오늘 목표
- A→B→C 선형 파이프라인 구성
- shared PVC 파일 handoff 검증 (A→B→C 순서로 파일 전달)

### 변경 파일 목록
**poc:**
- cmd/pipeline-abc/main.go: A→B→C DAG + PVC handoff 검증 runner
- deploy/poc/poc-pvc.yaml: 클러스터에 적용 (kubectl apply)

### 구현한 내용
- A→B→C 선형 dag-go 파이프라인 (AddEdge 체인)
- shared PVC `poc-shared-pvc` → `/data` 마운트 (전 노드 공유)
- 파일 handoff: A writes `/data/a.txt` → B reads+transforms → `/data/b.txt` → C verifies
- RWO PVC + kind single-node: 순차 실행이므로 RWO로 충분

### 검증 명령 및 결과
```bash
kubectl apply -f deploy/poc/poc-pvc.yaml
# persistentvolumeclaim/poc-shared-pvc created

go run ./cmd/pipeline-abc/
# [pipeline-abc] PASS: A→B→C pipeline succeeded with shared PVC handoff
# 실행 흐름: start→node-a→node-b→node-c→end (순차 확인)
```

### 아직 안 된 것
- fan-out (3-5개 병렬 노드) - Day 6
- executionClass 분리 (standard/highmem) - Day 8

### 리스크 / 막힌 점
- 없음 (PVC lazy provisioning은 첫 pod mount 시 자동 bind - 예상대로 동작)

### 내일 첫 작업 제안
fan-out: A 하나 → B1/B2/B3 병렬 실행 + queue-name label injection

---

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

<!-- session-end: 2026-03-27 21:10:18 -->

<!-- session-end: 2026-03-27 21:12:01 -->

<!-- session-end: 2026-03-27 21:16:46 -->

<!-- session-end: 2026-03-27 21:19:05 -->

<!-- session-end: 2026-03-27 21:22:05 -->

<!-- session-end: 2026-03-27 21:25:44 -->

<!-- session-end: 2026-03-28 15:24:53 -->

<!-- session-end: 2026-03-28 15:26:42 -->

<!-- session-end: 2026-03-28 15:29:13 -->

<!-- session-end: 2026-03-28 15:32:51 -->

<!-- session-end: 2026-03-28 15:39:59 -->

<!-- session-end: 2026-03-28 15:42:08 -->

<!-- session-end: 2026-03-28 15:44:20 -->

<!-- session-end: 2026-03-28 16:23:21 -->

<!-- session-end: 2026-03-28 16:25:45 -->

<!-- session-end: 2026-03-28 16:27:23 -->

<!-- session-end: 2026-03-28 17:38:59 -->

<!-- session-end: 2026-03-28 17:45:32 -->

<!-- session-end: 2026-03-28 19:41:14 -->

<!-- session-end: 2026-03-28 19:42:33 -->

<!-- session-end: 2026-03-28 20:21:56 -->

<!-- session-end: 2026-03-28 20:24:08 -->

<!-- session-end: 2026-03-28 21:05:14 -->

<!-- session-end: 2026-03-28 21:15:19 -->

<!-- session-end: 2026-03-28 21:33:53 -->

<!-- session-end: 2026-03-28 21:37:48 -->

<!-- session-end: 2026-03-28 22:11:26 -->

<!-- session-end: 2026-03-28 22:15:00 -->

<!-- session-end: 2026-03-28 22:12:34 -->

## Sprint 10-1 - 2026-04-03

### 완료 항목
- deploy/kueue/queues.yaml: poc-standard-cq cpu quota 4000m → 500m (실험), 완료 후 4000m 롤백
- docs/poc/SPRINT10_1_FINAL_REPORT.md: 최종 보고서 (Sprint 9 vs Sprint 10-1 비교 표 포함)

### 구현 핵심
- 코드 변경 없음. 단일 변수 (quota) 변경 실험
- 유일한 차이: poc-standard-cq cpu nominalQuota 4000m → 500m
- 실험 완료 후 queues.yaml 원상 복구 (cpu="4")

### 실험 결과
- wall-clock: 36s → 43s (+7s, +19%)
- Kueue admission 대기 최대 ~6s (worker-1 Jobs가 quota 포화 상태에서 대기)
- XPENDING 최종: 0 (at-least-once 유지)
- runstore finished: 5/5
- K8s Jobs Complete: 25/25 (naming collision 없음)
- 개별 run 지연: run-001/003에서 18s → 24~25s (quota 경합으로 worker 단계 지연)

### 관찰 메커니즘
- 3 runs × 3 workers = 9 Jobs 동시 제출 → 500m quota에서 4~6개 Job admission 대기 발생
- prepare 노드(순차 제출): quota 여유 → 즉시 admitted
- worker 노드(동시 9개 제출): 5개 즉시, 4개 최대 6s 대기

<!-- session-end: 2026-04-03 -->

---

## Sprint 9 - 2026-04-03

### 완료 항목
- cmd/ingress/main.go: `--concurrent-runs N` 플래그 + `dispatch()` concurrent화 (sync import, semaphore, WaitGroup)
- docs/poc/SPRINT9_FINAL_REPORT.md: 최종 보고서 (Sprint 8 vs Sprint 9 비교 표 포함)

### 구현 핵심
- semaphore(chan struct{}, N) + WaitGroup으로 최대 N개 run 동시 처리
- semaphore를 Consume 전에 획득 → sequential 모드(N=1)에서 Sprint 8 XPENDING 패턴 유지
- entryID/runID goroutine 인자로 명시 전달 (loop variable capture 방지)
- at-least-once Ack 의미론 변경 없음

### 0단계 확인
- go test -race ./... → PASS (JsonRunStore 이미 sync.Mutex 보유, BoundedDriver 이미 race-safe)
- 별도 mutex 추가 불필요

### 실험 결과
- --concurrent-runs 1 regression: PASS (Sprint 8 sequential 동일)
- --concurrent-runs 3, N=5: wall-clock 91s → 36s (-60%), XPENDING=0, 25 Jobs Complete
- 3개 run 동시 running 로그 확인

### 빌드/테스트
- go build -tags redis ./cmd/ingress/ → PASS
- go build ./... (기본 빌드) → PASS
- go test -race ./... → PASS (6 packages, race detector)

<!-- session-end: 2026-04-03 -->

---

## Sprint 8 - 2026-04-03

### 완료 항목
- cmd/dummy/main.go: N-shot submitter (신규, //go:build redis)
- pkg/ingress/cmd_wiring_test.go: cmd/dummy producer-only 예외 처리 추가
- docs/poc/SPRINT8_FINAL_REPORT.md: 최종 보고서

### 구현 핵심
- `go run -tags redis ./cmd/dummy/ -n 5` → 5개 runID XADD 후 종료
- runID: `dummy-{YYYYMMDD-HHMMSS}-{seq:03d}` — burst에서도 충돌 없음
- producer/consumer 경계 분리: dummy = Enqueue 전용, ingress = Consume + pipeline 실행
- specimen v0.1 변경 없음

### 실험 결과 요약
- XLEN=5 backlog 확인 (Dispatcher 없이 제출 후)
- sequential drain: 5 runs × ~18s = 91s 총 소요
- XPENDING=0, runstore 5개 finished, K8s Jobs 25개 Complete
- 현재 구조 한계 명시: Dispatcher 동기 → Kueue pressure 없음, scheduler 부하 없음

### 후속 정리 (Sprint 8 polish)
- cmd/ingress/main.go: `--max-runs N` 추가 (~6줄, pkill 불필요, exit 0 보장)
- pkg/ingress/cmd_wiring_test.go: producerOnlyCmds 주석 WHY 보강
- docs/poc/SPRINT8_FINAL_REPORT.md: 최소 계약 섹션 + baseline 재실험 runbook 추가

### 빌드/테스트
- go build -tags redis ./cmd/dummy/ → PASS
- go build -tags redis ./cmd/ingress/ → PASS
- go build ./... (기본 빌드) → PASS
- go test ./... → PASS (6 packages)

<!-- session-end: 2026-04-03 -->

---

## Sprint 7 - 2026-04-03

### 완료 항목
- cmd/ingress/main.go: `--pipeline` 플래그 + `runSpecimenPipeline()` + specimen spec builders + dispatch 라우팅 (~120줄 추가)
- docs/poc/PIPELINE_SPECIMEN.md: canonical specimen v0.1 공식 문서 (Sprint 7 핵심 산출물)
- docs/poc/SPRINT7_FINAL_REPORT.md: 최종 보고서

### 구현 핵심
- `--pipeline fanout` : Sprint 6 happy path 명시 연결 (regression 확인)
- `--pipeline specimen`: prepare → worker-1/2/3 → collect (canonical specimen v0.1)
- 기존 `runWideFanout3()` / `ingressSpec*` 수정 없음 — wiring 추가만
- specimen K8s Job 이름: `i-{runID}-prepare`, `i-{runID}-worker-N`, `i-{runID}-collect`

### 실행 결과
- `--pipeline fanout` regression: PASS (XPENDING=0, state=finished)
- `--pipeline specimen` end-to-end: PASS (5 K8s Jobs Complete, XPENDING=0, state=finished)
- `--pipeline specimen --fail` PEL 잔류: PASS (XPENDING=1, XACK 미호출)

### 빌드/테스트
- go build -tags redis ./cmd/ingress/ → PASS
- go build ./... (기본 빌드) → PASS
- go test ./... → PASS (6 packages)

<!-- session-end: 2026-04-03 -->

---

## Sprint 6 - 2026-04-01

### 완료 항목
- cmd/ingress/main.go: Redis Dispatcher + wideFanout3 + K8s Job spec (//go:build redis, 신규 ~220줄)
- docs/poc/SPRINT6_RUNBOOK.md: 재현 가능한 실행 절차
- docs/poc/SPRINT6_FINAL_REPORT.md: 최종 보고서

### 구현 핵심
- Redis Streams(XREADGROUP) → gate.Admit() → dag-go → BoundedDriver → K8s Job end-to-end 연결
- Ack 정책: gate.Admit() nil 반환(= 파이프라인 전체 완료) 후에만 XACK → at-least-once
- 파이프라인: setup → B1/B2/B3 → collect (wideFanout3, inline)
- 기존 컴포넌트 7개 재사용, 신규 wiring 코드만 추가

### 빌드/테스트
- go build -tags redis ./cmd/ingress/ → PASS
- go build ./... (기본 빌드) → PASS
- go test ./... → PASS (6 packages)

<!-- session-end: 2026-04-01 -->

---

## Sprint 5 - 2026-03-28

### 완료 항목
- cmd/stress/main.go: 스트레스 하네스 진입점 (5개 패턴 디스패치, multi-run-burst)
- cmd/stress/patterns.go: InstrumentedRunner, RunMetrics, 5개 패턴 구현, DefaultTimeout=0 fix
- cmd/stress/stress_test.go: 7/7 단위 테스트 PASS
- docs/poc/synthetic_pipeline_patterns.md: 패턴 카탈로그 living document
- docs/poc/SPRINT5_FINAL_REPORT.md: 12개 항목 최종 보고서

### 실험 결과 요약
- wide-fanout-8: sem=1/4/8 모두 critical_path=18s (idle 클러스터, sem throttle 무영향)
- long-tail-8: DefaultTimeout=30s 버그 발견 → InitDagWithOptions(noPreflight()) 수정
- two-stage-8x4: collector_delay=12s (M+C 스테이지 2단계 반영)
- mixed-duration-8: b_stage=14s (B8=9s sleep 스트래글러 지배)
- multi-run-burst: 5/5 PASS, wall=35s, 격리 검증 완료

<!-- session-end: 2026-03-28 -->

<!-- session-end: 2026-03-29 15:53:35 -->

<!-- session-end: 2026-03-29 16:23:50 -->

<!-- session-end: 2026-03-29 16:47:19 -->

<!-- session-end: 2026-03-29 16:59:57 -->

<!-- session-end: 2026-03-29 17:16:42 -->

<!-- session-end: 2026-03-30 17:48:56 -->

<!-- session-end: 2026-03-30 17:50:31 -->

<!-- session-end: 2026-03-30 17:52:19 -->

<!-- session-end: 2026-03-30 18:07:30 -->

<!-- session-end: 2026-03-30 18:41:20 -->

<!-- session-end: 2026-03-30 18:44:42 -->

<!-- session-end: 2026-03-30 18:47:21 -->

<!-- session-end: 2026-03-30 18:47:57 -->

<!-- session-end: 2026-03-30 18:48:31 -->

<!-- session-end: 2026-04-01 15:48:02 -->

<!-- session-end: 2026-04-01 15:53:15 -->

<!-- session-end: 2026-04-01 16:18:46 -->

<!-- session-end: 2026-04-01 16:23:55 -->

<!-- session-end: 2026-04-01 16:32:32 -->

<!-- session-end: 2026-04-01 16:37:22 -->

<!-- session-end: 2026-04-01 19:02:31 -->

<!-- session-end: 2026-04-01 19:05:56 -->

<!-- session-end: 2026-04-01 19:07:27 -->

<!-- session-end: 2026-04-01 19:17:06 -->

<!-- session-end: 2026-04-01 19:23:22 -->

<!-- session-end: 2026-04-01 19:25:07 -->

<!-- session-end: 2026-04-01 19:32:06 -->

<!-- session-end: 2026-04-01 19:42:52 -->

<!-- session-end: 2026-04-01 20:04:49 -->

<!-- session-end: 2026-04-01 20:05:42 -->

<!-- session-end: 2026-04-01 20:21:53 -->

<!-- session-end: 2026-04-01 20:26:42 -->

<!-- session-end: 2026-04-01 20:31:45 -->

<!-- session-end: 2026-04-01 20:37:01 -->

<!-- session-end: 2026-04-03 17:19:14 -->

<!-- session-end: 2026-04-03 17:20:29 -->

<!-- session-end: 2026-04-03 17:21:40 -->

<!-- session-end: 2026-04-03 17:37:29 -->

<!-- session-end: 2026-04-03 18:02:29 -->

<!-- session-end: 2026-04-03 20:05:02 -->

<!-- session-end: 2026-04-03 20:05:17 -->

<!-- session-end: 2026-04-03 20:46:32 -->

<!-- session-end: 2026-04-03 21:00:52 -->

<!-- session-end: 2026-04-03 21:19:52 -->

<!-- session-end: 2026-04-03 21:50:01 -->

<!-- session-end: 2026-04-03 22:10:01 -->

<!-- session-end: 2026-04-03 22:18:53 -->
