# 구조 검증 보고서

> 작성일: 2026-03-28
> 대상: Pipeline-Lite + dag-go + spawner + Kueue + kube-scheduler
> 목적: 현재 PoC 코드의 아키텍처 경계 검증. production 최종안 아님.
> 선행 문서: ARCH_BOUNDARY.md (경계 분석), POC_EVALUATION.md (기능 판정)

---

## 1. 검증 범위 및 배경

10일 PoC(POC_EVALUATION.md)에서 기능 동작은 GO 판정을 받았다.
이 보고서는 그 다음 질문에 답한다:

> **"현재 구조에서 어떤 경계가 명시되어 있고, 어떤 경계가 빠져 있는가?"**

5개 가설을 대상으로 구조적 경계를 확인하고, 최소 인터페이스를 제안하며, 테스트로 증명한다.

---

## 2. 검증 대상 가설

| # | 가설 | 검증 방법 |
|---|------|----------|
| H1 | 사용자 제출은 service 앞단의 Run 큐/세션 스토어가 받는다 | RunStore 인터페이스 + 구현 + 테스트 |
| H2 | dag-go actor/session이 ready node attempt만 생성한다 | DagSession + NodeSubmitter + 테스트 |
| H3 | 사용자 burst는 앞단 큐 경계가 흡수한다 (K8s API에 직접 도달하지 않는다) | BurstBoundary 관찰 테스트 |
| H4 | durable 스토어는 재시작 후 queued run을 복구한다; memory 스토어는 유실한다 | JsonRunStore vs InMemoryRunStore 테스트 |
| H5 | DAG 전체 run은 Kueue admission 단위가 아니다; node attempt별 K8s Job이 단위다 | DagSession 구조 + 코드 분석 |

---

## 3. 현재 코드 경계 분석

### 3.1 실행 흐름 (현재)

```
사용자 요청
    │  ← 여기 RunStore/Queue 없음 [H1 경계 부재]
    ▼
Dispatcher.Handle()
    │  ← burst 즉시 전달 [H3 경계 부재]
    ▼
Actor.Loop() → DriverK8s.Prepare/Start
    │  ← ready/held 구분 없음 [H2 경계 불명시]
    ▼
K8s Job 생성 (suspend=true + kueue label)
    ▼
Kueue admission → kube-scheduler → Pod 실행
```

dag-go를 쓰는 poc/cmd/* 경로:
```
main() → dag-go DAG 구성
    └─ SpawnerNode.RunE()  ← dag-go가 순서를 제어하지만
         └─ DriverK8s.Start()  ← "ready 경계"가 코드로 명시되지 않음
```

### 3.2 발견된 경계 위반

| 위반 | 위치 | 영향 |
|------|------|------|
| RunStore 없음 | `Dispatcher.Handle()` | 재시작 시 진행 중 run 유실, burst 무제한 전달 |
| ready node 경계 불명시 | `SpawnerNode.RunE()` | dag-go가 순서를 제어하지만 "K8s Job 생성 금지" 경계가 코드에 없음 |
| 앞단 큐 없음 | `Dispatcher` → `Actor` 직결 | N 사용자 × M 노드 = N×M K8s API 호출, 제어 포인트 없음 |
| K8s 연결 실패 시 panic | `spawner/cmd/server/main.go` | 앞단 큐가 있으면 graceful 처리 가능 |

---

## 4. 제안 인터페이스 및 구현

### 4.1 RunStore (가설 H1, H4)

**파일**: `poc/pkg/store/run_store.go`

```go
type RunStore interface {
    Enqueue(ctx context.Context, rec RunRecord) error
    Get(ctx context.Context, runID string) (RunRecord, bool, error)
    UpdateState(ctx context.Context, runID string, from, to RunState) error
    ListByState(ctx context.Context, state RunState) ([]RunRecord, error)
    Delete(ctx context.Context, runID string) error
}
```

구현:
- `InMemoryRunStore` (`poc/pkg/store/memory_run_store.go`) — 빠름, 재시작 시 유실
- `JsonRunStore` (`poc/pkg/store/json_run_store.go`) — 파일 기반, 재시작 복구 가능

ASSUMPTION: production에서는 PostgreSQL/Redis 등으로 교체. 인터페이스는 동일.

### 4.2 상태 기계

```
queued ──────────────────────────────► admitted-to-dag
  │                                           │
  ├──► held ──► resumed ──► admitted-to-dag   ▼
  │       │         │              running ──► finished
  │       │         │                 │
  └───────┴─────────┴─────────────────┴──► canceled
```

유효 전이 (코드 기준):
```
queued      → held | admitted-to-dag | canceled
held        → resumed | canceled
resumed     → admitted-to-dag | canceled
admitted-to-dag → running | canceled
running     → finished | canceled
finished    → (terminal)
canceled    → (terminal)
```

역방향 및 terminal→any 는 `ValidateTransition()` 이 거부한다.

ASSUMPTION: "held" 사용 시점 (rate-limit hold vs manual hold vs policy hold) 미결.
ASSUMPTION: "resumed"가 재-큐잉 없이 바로 "admitted-to-dag"로 전이하는지 미결.

### 4.3 DagSession + NodeSubmitter (가설 H2, H3)

**파일**: `poc/pkg/session/dag_session.go`

```go
type NodeSubmitter interface {
    Submit(ctx context.Context, nodeID string) error
}

type DagSession struct { /* dependency graph + NodeSubmitter */ }
func (s *DagSession) Start(ctx) error          // ready node만 Submit
func (s *DagSession) NodeDone(ctx, id, ok) error  // 완료 통보 → 다음 ready 확인
func (s *DagSession) HeldNodes() []string      // K8s Job 미생성 목록
```

핵심 불변식:
- `nodeHeld` 상태인 node는 `NodeSubmitter.Submit()` 호출이 없다
- = K8s Job이 생성되지 않는다
- 이것이 "ready node only" 경계를 코드로 강제하는 방식

---

## 5. 테스트 결과

### 5.1 실행 환경

```
go test ./pkg/store/... ./pkg/session/... -v
```

단위 테스트만 포함 (integration build tag 없음). K8s 클러스터 불필요.

### 5.2 pkg/store 결과 (5/5 PASS)

```
=== RUN   TestValidateTransition_Valid
--- PASS: TestValidateTransition_Valid (0.00s)
    → 10개 유효 전이 모두 허용됨

=== RUN   TestValidateTransition_Invalid
--- PASS: TestValidateTransition_Invalid (0.00s)
    → 5개 무효 전이 (역방향, terminal→any) 모두 거부됨

=== RUN   TestMemoryStore_DoesNotSurviveReset
--- PASS: TestMemoryStore_DoesNotSurviveReset (0.00s)
    OBSERVATION: InMemoryRunStore lost 2 queued runs on reset

=== RUN   TestMemoryStore_StateTransition
--- PASS: TestMemoryStore_StateTransition (0.00s)
    PASS: invalid backward transition rejected: invalid state transition: admitted-to-dag → queued
    PASS: wrong 'from' state rejected: state mismatch: expected queued, got admitted-to-dag

=== RUN   TestJsonStore_RecoveryAfterRestart
--- PASS: TestJsonStore_RecoveryAfterRestart (0.00s)
    OBSERVATION: JsonRunStore recovered 1 queued + 1 admitted runs after restart

=== RUN   TestJsonStore_NoDuplicateEnqueue
--- PASS: TestJsonStore_NoDuplicateEnqueue (0.00s)
    PASS: duplicate enqueue rejected: run already exists
```

### 5.3 pkg/session 결과 (5/5 PASS)

```
=== RUN   TestOnlyReadyNodesSubmitted
--- PASS: TestOnlyReadyNodesSubmitted (0.00s)
    PASS: only A submitted; B, C held ([B C])

=== RUN   TestNonReadyNodeNotSubmitted
--- PASS: TestNonReadyNodeNotSubmitted (0.00s)
    confirmed: B not submitted before A completes
    PASS: A→B→C submitted in dependency order

=== RUN   TestFastFail_DependantNotSubmitted
--- PASS: TestFastFail_DependantNotSubmitted (0.00s)
    PASS: C remained held after B failed; held: [C]

=== RUN   TestFanOut_AllReadyNodesSubmittedAtOnce
--- PASS: TestFanOut_AllReadyNodesSubmittedAtOnce (0.00s)
    PASS: fan-out B1/B2/B3 submitted after A; C still held

=== RUN   TestBurstBoundary_AllReadyAtStart
--- PASS: TestBurstBoundary_AllReadyAtStart (0.05s)
    OBSERVATION: 10 nodes, 10 calls, max_concurrent=1, elapsed=52ms
    OBSERVATION: submitBatch is sequential → max_concurrent=1
    OBSERVATION: caller can fire N DagSessions concurrently → N×M K8s API calls with no cap
    OBSERVATION: NodeAttemptQueue(sem=3) proposed in ARCH_BOUNDARY.md §7
```

### 5.4 테스트가 증명하는 것

| 테스트 | 증명 | 관련 가설 |
|--------|------|----------|
| `TestValidateTransition_Valid/Invalid` | 상태 기계가 정책을 강제함 | H1 |
| `TestMemoryStore_DoesNotSurviveReset` | 메모리 스토어는 재시작 시 run을 잃음 | H4 |
| `TestJsonStore_RecoveryAfterRestart` | 파일 스토어는 재시작 후 queued run을 복구함 | H4 |
| `TestOnlyReadyNodesSubmitted` | Start() 직후 deps 없는 노드만 K8s Job이 됨 | H2 |
| `TestNonReadyNodeNotSubmitted` | A 완료 전 B는 K8s Job이 아님 | H2, H5 |
| `TestFastFail_DependantNotSubmitted` | 부모 실패 시 자식은 K8s Job으로 생성되지 않음 | H2 |
| `TestFanOut_AllReadyNodesSubmittedAtOnce` | 준비된 노드가 동시에 여러 개가 돼도 C는 held | H2, H5 |
| `TestBurstBoundary_AllReadyAtStart` | 큐 없이 N 독립 노드 → N번 직접 호출 (H3 경계 부재 노출) | H3 |

---

## 6. 가설별 판정

| 가설 | 현재 코드 | 구조 검증 결과 |
|------|-----------|---------------|
| H1: 앞단 Run 큐 | **부재** — Dispatcher 직결 | RunStore 인터페이스 제안·구현 완료. production 연결 필요. |
| H2: ready node만 K8s Job | **불명시** — dag-go가 암묵적으로 처리 | DagSession으로 경계 명시. 5개 테스트로 불변식 증명. |
| H3: burst 앞단 흡수 | **부재** — N×M 직접 호출 | BurstBoundary 관찰 테스트로 노출. NodeAttemptQueue 제안(미구현). |
| H4: durable 스토어 재시작 복구 | **부재** — 구현 자체 없음 | JsonRunStore 구현 + 테스트 증명. InMemoryRunStore 유실도 증명. |
| H5: node별 K8s Job이 단위 | **혼재** — DAG 전체를 submit 단위로 혼동 가능 | DagSession 구조로 "run ≠ Job, node attempt = Job" 명시. |

---

## 7. 남은 리스크

| 리스크 | 심각도 | 상태 |
|--------|--------|------|
| H3 burst 경계 미구현 (NodeAttemptQueue) | 높음 | 설계 제안 완료, 구현 미착수 |
| RunStore ↔ Dispatcher 연결 없음 | 높음 | 인터페이스 완료, 연결 코드 미작성 |
| Kueue pending vs kube-scheduler unschedulable 미구분 | 중간 | 관찰 경로 분석 완료, integration test 미작성 |
| gang scheduling 노드 경로 없음 | 중간 | 설계 메모 완료, 최소 예제 미구현 |
| DagSession이 dag-go와 분리 → dag-go 역할 재정의 필요 | 낮음 | production 단계 과제 |

---

## 8. 다음 단계 제안 (우선순위 순)

### 즉시 (다음 single small diff)

**NodeAttemptQueue** — `poc/pkg/session/attempt_queue.go`

```go
type NodeAttemptQueue struct {
    sem      chan struct{} // 동시 submit 제한 (예: 3)
    inner    NodeSubmitter
}
func (q *NodeAttemptQueue) Submit(ctx context.Context, nodeID string) error
```

- `DagSession`의 `submitBatch`가 `NodeSubmitter` 대신 `NodeAttemptQueue`를 사용
- 검증: 10 독립 노드, sem=3 → max_concurrent가 3으로 제한되는지 확인
- 이것이 H3 "burst 앞단 흡수" 경계를 코드로 완성하는 최소 diff

### 단기

1. `Dispatcher`에 `RunStore` 연결 (queued 상태 보존 경로 완성)
2. Kueue pending vs kube-scheduler unschedulable 관찰 integration test 작성
3. `JsonRunStore` → WAL 또는 atomic write 개선 (현재 non-atomic)

### 장기

4. gang scheduling 최소 예제 (`cmd/gangscheduling/`)
5. RunStore → PostgreSQL 교체 (인터페이스 변경 없이 구현만 교체)

---

## 9. 결론

**현재 PoC 코드는 기능적으로 동작하지만, 4개 경계 중 2개가 완전히 부재하고 2개는 불명시 상태다.**

이번 구조 검증에서:
- 가설 H1/H4: `RunStore` 인터페이스와 두 구현체를 제안·검증함. 재시작 복구 차이를 테스트로 증명함.
- 가설 H2/H5: `DagSession` + `NodeSubmitter`로 "ready node만 K8s Job이 된다"는 불변식을 코드로 명시하고 5개 테스트로 증명함.
- 가설 H3: `TestBurstBoundary_AllReadyAtStart`로 현재 경계 부재를 관찰·노출함. 해결은 NodeAttemptQueue (다음 diff).

10개 테스트 10/10 PASS. 빌드 오류 없음.

---

*ARCH_BOUNDARY.md에서 제안된 구조의 실제 구현 및 테스트 결과 기록.*
*미결 가정(ASSUMPTION)은 각 섹션에 명시함.*
