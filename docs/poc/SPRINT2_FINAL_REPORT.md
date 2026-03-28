# 구조 검증 스프린트 2 — 최종 보고서

> 작성일: 2026-03-28
> 스프린트 목표: 5가지 구조 가설을 최소 구현 + 테스트 + 실험으로 검증하고 채택 가능 여부를 판정

---

## 1. 수정 파일 목록

### poc repo

| 파일 | 변경 | 내용 |
|------|------|------|
| `pkg/ingress/run_gate.go` | NEW | RunGate — ingress boundary를 dag-go 실행 경로에 연결 |
| `pkg/ingress/run_gate_test.go` | NEW | Q1 검증 테스트 6개 |
| `pkg/observe/pipeline_status.go` | NEW | PipelineStatusReader + AllSignals — 6개 정지 원인의 observable signal 목록 |
| `pkg/observe/pipeline_status_test.go` | NEW | Q4 검증 테스트 7개 |
| `pkg/session/dag_session.go` | MOD | submitBatch goroutine화 (기존: sequential → 현재: concurrent goroutine per node) |
| `pkg/session/dag_session_role_test.go` | NEW | Q3 검증 테스트 4개 (DagSession 책임 경계 분석) |
| `pkg/session/attempt_queue_test.go` | MOD | Q2 검증 테스트 추가 (TestBurstBoundary_DagSessionGoroutineSubmit) |
| `pkg/session/dag_session_test.go` | MOD | 관찰 로그 업데이트 (max_concurrent=1→10 반영) |
| `pkg/store/json_run_store.go` | MOD | persist() atomic write (os.WriteFile → tmp+rename) |
| `pkg/store/run_store_test.go` | MOD | Q5 검증 테스트 2개 추가 (atomic write + durability grade) |

### spawner repo

| 파일 | 변경 | 내용 |
|------|------|------|
| `pkg/store/json_run_store.go` | MOD | persist() atomic write (동일 적용) |

---

## 2. 이번 작업에서 추가/수정한 최소 구현 목록

### [Ingress 최소 구현]

**RunGate** (`poc/pkg/ingress/run_gate.go`):

```go
type RunGate struct {
    Store store.RunStore
}

// Admit records run in store, calls runFn, advances state.
// queued → admitted-to-dag → running → finished/canceled
func (g *RunGate) Admit(ctx context.Context, runID string, runFn func(context.Context) error) error
```

INVARIANT: `RunStore.Enqueue(queued)` is called BEFORE `runFn` is invoked.
BACKWARD COMPAT: nil Store = no-op gate (existing cmd/* paths unaffected).

### [Release Control 최소 구현]

**DagSession.submitBatch goroutine화** (`poc/pkg/session/dag_session.go`):

```go
// 기존: sequential for loop → NodeAttemptQueue 효과 없음 (max_concurrent=1)
// 수정: goroutine per node + sync.WaitGroup
// 결과: NodeAttemptQueue(sem=k)가 단일 DagSession 내에서도 실제로 burst를 k로 제한
```

### [Observability 최소 구현]

**AllSignals** (`poc/pkg/observe/pipeline_status.go`):

6개 정지 원인과 각각의 observable signal을 코드로 정의:

```go
var AllSignals = []ObservableSignal{
    {CauseIngressQueue,    "RunStore.ListByState(queued)", false},
    {CauseReleaseQueue,    "NodeAttemptQueue.Stats().blocked > 0", false},
    {CauseKueuePending,    "K8sObserver.ObserveWorkload().QuotaReserved==false", true},
    {CauseSchedulerUnsched,"K8sObserver.ObservePod().Scheduled==false", true},
    {CauseRunning,         "RunStore.ListByState(running)", false},
    {CauseFinished,        "RunStore.ListByState(finished)", false},
}
```

### [Durability 최소 구현]

**JsonRunStore atomic write** (`poc/pkg/store/json_run_store.go`):

```go
// 기존: os.WriteFile(path, data) → partial write 가능
// 수정: os.CreateTemp + Write + Close + os.Rename(tmp, path)
// 결과: crash 시 old file 그대로 유지 (POSIX atomic rename 보장)
```

---

## 3. 이번 작업에서 추가/수정한 테스트 목록

### poc/pkg/ingress (신규 6개)

| 테스트 | 검증 항목 |
|--------|-----------|
| `TestRunGate_EnqueuesBeforeRunFnIsCalled` | Enqueue가 runFn 실행 전 호출됨 (핵심 Q1) |
| `TestRunGate_FullStateTransition` | queued→admitted-to-dag→running→finished 전이 |
| `TestRunGate_TransitionsToCanceledOnRunFnError` | runFn 오류 시 canceled 전이 |
| `TestRunGate_NilStore_Noop` | nil store = no-op (하위 호환) |
| `TestRunGate_BypassPath_DocumentsGap` | 현재 cmd/* 우회 경로 문서화 |
| `TestRunGate_RestartRecovery` | JsonRunStore 재시작 후 복구 |

### poc/pkg/session (수정 + 신규)

| 테스트 | 검증 항목 |
|--------|-----------|
| `TestBurstBoundary_DagSessionGoroutineSubmit` | goroutine submitBatch + sem=3 → inner_max_concurrent=3 (Q2) |
| `TestDagSession_Responsibilities_IsAdapterNotEngine` | DagSession 책임 분류 (Q3) |
| `TestDagSession_DoesNotKnowAboutDagGo` | DagSession이 dag-go import 없음 확인 |
| `TestDagSession_CurrentUsagePath` | cmd/*에서 DagSession 미사용 — 구조적 갭 기록 |
| `TestDagSession_ReadinessAuthority` | dag-go vs DagSession readiness 권한 비교 |

### poc/pkg/observe (신규 7개)

| 테스트 | 검증 항목 |
|--------|-----------|
| `TestQ4_AllSignalsRepresented` | 6개 원인 모두 observable signal 존재 |
| `TestQ4_IngressQueueState_Observable` | RunStore.queued → observable |
| `TestQ4_ReleaseQueueState_Observable` | NodeAttemptQueue.blocked > 0 → observable |
| `TestQ4_RunningState_Observable` | RunStore.running → observable |
| `TestQ4_FinishedState_Observable` | RunStore.finished → observable |
| `TestQ4_KueuePending_And_SchedulerUnschedulable_SignalsDocumented` | K8s 의존 신호 문서화 |
| `TestQ4_StoppedRunDiagnosis` | 운영자 진단 워크플로우 완전 표 |

### poc/pkg/store (추가 2개)

| 테스트 | 검증 항목 |
|--------|-----------|
| `TestJsonStore_AtomicWrite_NoPartialCorruption` | 20건 write → 재시작 후 전체 복구 (Q5) |
| `TestJsonStore_Q5_DurabilityGrade` | 현재 구현 durability 등급 판정 |

---

## 4. 5가지 질문별 검증 결과

### Q1: Ingress 경계가 주 실행 경로 전체를 통과하는가?

**최소 구현:** `RunGate` — `Admit(ctx, runID, runFn)` wraps dag-go execution with RunStore tracking.

**검증 방법:** `TestRunGate_EnqueuesBeforeRunFnIsCalled` — runFn 내부에서 store 상태를 확인.

**실제 결과:**
```
run run-q1 → queued
[runFn called]
  run-q1 state in store = running  ← runFn 진입 시 이미 store에 기록됨
run run-q1 → finished
PASS: Enqueue(queued) happened BEFORE runFn — ingress boundary holds
```

**판정: PARTIAL**

**근거:**
- RunGate 구조 자체는 성립한다 — Enqueue → runFn → UpdateState 순서 보장 (5/5 PASS).
- **그러나 `cmd/pipeline-abc`, `cmd/execclass`, `cmd/fanout` 등 모든 cmd/* 실행 경로는 여전히 RunGate를 사용하지 않는다.** dag-go → SpawnerNode.RunE → DriverK8s 경로가 직접 호출된다.
- RunGate가 cmd/* 경로에 연결되려면 각 main.go를 수정해야 하며, 이는 K8s 없이 테스트하기 어려운 범위이다.

**ASSUMPTION:** production 연결 시 `gate.Admit(ctx, runID, func(ctx) error { dag.Start(); return dagWait(ctx) })` 패턴 사용.

---

### Q2: NodeAttemptQueue가 실제로 burst를 제어하는가?

**최소 구현:** `DagSession.submitBatch` goroutine화 — 각 node를 goroutine에서 Submit, `sync.WaitGroup`으로 완료 대기.

**검증 방법:** `TestBurstBoundary_DagSessionGoroutineSubmit` — sem=3, 10 independent nodes.

**실제 결과:**
```
Q2 PASS: DagSession(goroutine submitBatch) + NodeAttemptQueue(sem=3)
  inner_max_concurrent=3 (capped at sem=3)
  queue_max_concurrent=3
  PROOF: NodeAttemptQueue is NOT a test decoration — it controls actual burst
```

**판정: PARTIAL**

**근거:**
- **DagSession 경로에서는 PASS**: goroutine submitBatch + NodeAttemptQueue(sem=3) → inner_max_concurrent=3 (경계 성립).
- **그러나 주 실행 경로 (dag-go → SpawnerNode.RunE)에는 NodeAttemptQueue가 연결되어 있지 않다.** dag-go는 자체 goroutine pool로 ready node를 병렬 실행한다 — 이 경로에는 burst 제어가 없다.
- 따라서 "NodeAttemptQueue가 burst를 제어한다"는 DagSession 경로에서만 성립. cmd/* 주 실행 경로에서는 미성립.

---

### Q3: DagSession이 dag-go의 두 번째 orchestration engine으로 커지고 있는가?

**최소 구현:** 코드 분석 + 4개 evidence test.

**실제 결과:**
```
ADAPTER roles (4개): NodeSubmitter 인터페이스, 노드 완료→submit 변환, ready→NodeSubmitter 전달, 상태 추적
ENGINE roles (4개): 자체 dependency graph, 자체 ready 계산, 자체 Start(), 자체 NodeDone()

DagSession has no dag-go imports
cmd/pipeline-abc: dag.Start() → SpawnerNode.RunE() directly (DagSession 미사용)
cmd/execclass: dag.Start() → SpawnerNode.RunE() directly
cmd/fanout: dag.Start() → SpawnerNode.RunE() directly
cmd/fastfail: dag.Start() → SpawnerNode.RunE() directly
cmd/dag-runner: dag.Start() → SpawnerNode.RunE() directly

OBSERVATION: 0 cmd paths use DagSession
```

**판정: PARTIAL (위험 신호 있음)**

**근거:**
- DagSession은 adapter 역할 4개 + engine 역할 4개를 동시에 가진다 — adapter:engine = 1:1 비율.
- dag-go와 완전히 분리된 별도 dependency graph를 가진다 (dag-go import 없음).
- 현재 cmd/*에서 전혀 사용되지 않으므로 "실질적으로 두 번째 engine"은 아니다.
- **위험**: 만약 DagSession이 cmd/*에 연결된다면, 두 readiness 시스템이 동시에 존재하는 구조가 된다. DAG 명세가 양쪽에 이중 등록되는 maintenance 버그 위험.
- **현재 판정**: DagSession은 engine 특성을 가지나 실제 orchestration을 수행하지 않는다 (cmd/* 연결 없음). 단, 연결 시 구조적 위험 증가.

---

### Q4: 운영자가 "멈춤"을 봤을 때 원인을 한 단계에서 설명할 수 있는가?

**최소 구현:** `AllSignals` (6개 정지 원인 + observable signal 매핑) + `PipelineStatusReader`.

**실제 결과:**
```
Cause                          Signal                                   Pod?       Operator Fix
ingress_queue                  RunStore.queued                          NO         Check RunStore
release_queue                  NodeAttemptQueue.blocked>0               NO         Wait for queue slot
kueue_pending                  workload.QuotaReserved=false             NO         Reduce CPU / increase quota
scheduler_unschedulable        pod.PodScheduled=false                   YES        Fix nodeSelector/taint
running                        RunStore.running                         YES        Normal
finished                       RunStore.finished                        YES        Check job logs

PASS: each stopped state maps to exactly one observable signal at one layer
PASS: operator can distinguish all 6 states without guessing
```

**판정: PASS (K8s 의존 신호 포함)**

**근거:**
- 6개 정지 원인 모두 observable signal이 존재한다.
- 1, 2, 5, 6번 원인은 단위 테스트로 검증 완료 (K8s 불필요).
- 3번 (Kueue pending), 4번 (scheduler unschedulable)은 Round 3 integration test에서 실 클러스터로 검증 완료.
- 운영자가 모르는 레이어 없이 원인을 추적 가능하다.

**LIMITATION**: AllSignals는 현재 static mapping — 실시간 통합 dashboard는 미구현 (ASSUMPTION으로 명시).

---

### Q5: Durable store가 개념 검증을 넘어서는가?

**최소 구현:** `JsonRunStore.persist()` — `os.WriteFile` → `os.CreateTemp + Write + Close + os.Rename`.

**실제 결과:**
```
Q5 PASS: atomic write preserved 20 runs across simulated restart
  queued=10, admitted-to-dag=10

=== JsonRunStore Durability Assessment ===
  Survives process restart                 YES — JsonRunStore (file-backed)
  Atomic write (no partial corruption)     YES — tmp+rename pattern
  Concurrent multi-process access          NO — single mutex, single file
  High-throughput writes                   NO — full JSON rewrite per mutation
  WAL / journal                            NO — no write-ahead log
  Production recommendation                Replace with PostgreSQL or equivalent

GRADE: Conditional Production Candidate
```

**판정: PASS (조건부)**

**근거:**
- 재시작 후 복구: YES (TestJsonStore_RecoveryAfterRestart 포함 5개 테스트 PASS).
- Partial write 방지: YES (tmp+rename atomic write 구현).
- 하지만 단일 파일 전체 JSON rewrite — 고속/다중 프로세스 환경 부적합.
- **등급: 개념 검증 수준은 넘었다 (조건부 production candidate).**

---

## 5. 통합 관찰 결과 요약

### 테스트 결과 (단위 테스트)

| 패키지 | 신규 테스트 | PASS |
|--------|------------|------|
| poc/pkg/ingress | 6 | 6/6 |
| poc/pkg/observe | 7 | 7/7 |
| poc/pkg/session (Q2+Q3) | 5 | 5/5 |
| poc/pkg/store (Q5) | 2 | 2/2 |
| 기존 유지 (store/session/dispatcher) | 26 | 26/26 |
| **합계** | **46** | **46/46** |

Integration test (Round 3, kind+Kueue): 2/2 PASS (별도 환경 필요)

### 핵심 관찰

```
[Q1] 주 실행 경로 tracing:
  cmd/pipeline-abc → dag.Start() → SpawnerNode.RunE() → DriverK8s
  RunStore/RunGate: 연결 안 됨 (구조적 갭 확인)
  RunGate는 존재하고 동작하나 cmd/* 경로에 미삽입

[Q2] burst 제어:
  NodeAttemptQueue(sem=3) + goroutine submitBatch → inner_max_concurrent=3 (PROOF)
  dag-go 경로: goroutine은 dag-go 엔진이 제어 → NodeAttemptQueue 미적용

[Q3] DagSession:
  adapter:engine 역할 = 4:4 (병렬 orchestration 가능성)
  cmd/* 5개 경로 전부 dag-go 직접 사용 (DagSession 0개)
  두 readiness 시스템이 완전히 분리된 채 존재

[Q4] 멈춤 관찰:
  6개 원인 × 1개 signal — 완전한 coverage
  K8s 없이 4개, K8s 포함 6개 전부 관찰 경로 확보

[Q5] durable store:
  atomic write 구현 → partial corruption 방지
  개념 검증 수준 초과, 단일 프로세스 환경에서 신뢰 가능
```

---

## 6. 남은 리스크

| 리스크 | 심각도 | 상태 |
|--------|--------|------|
| Ingress 경계가 cmd/* 주 실행 경로에 미연결 (RunGate 미삽입) | **높음** | 구조 확인, 연결 작업 필요 |
| DagSession readiness ≠ dag-go readiness — 이중 시스템 | **중간** | production 연결 시 명확히 하나로 통일 필요 |
| dag-go 경로에 NodeAttemptQueue 미적용 — burst 미제어 | **중간** | BoundedDriver 또는 dag-go 레벨 semaphore 필요 |
| K8sObserver가 어떤 실행 경로에도 통합되지 않음 | **중간** | Wait() 경로에 통합 필요 |
| JsonRunStore: 단일 파일 전체 rewrite — 고성능 환경 부적합 | 낮음 | ASSUMPTION으로 명시, PostgreSQL 교체 예정 |
| Bootstrap이 RunRecord.Payload 역직렬화 → 재디스패치를 caller에 위임 | 낮음 | ASSUMPTION으로 문서화 |

---

## 7. 최종 가능 여부 판정

### **조건부 가능 (Conditional Go)**

---

## 8. 판정 근거

**채택 가능 기준**: 5가지 질문이 모두 PASS이고, 주 실행 경로에 구조가 관통하며, 운영 관찰도 분명하고, store도 신뢰 수준에 도달.

**조건부 가능으로 판정한 이유:**

**성립한 것:**

1. **RunGate 구조 성립** (Q1 구조 검증): Enqueue → runFn → UpdateState 불변식은 5/5 테스트로 증명. `TestRunGate_EnqueuesBeforeRunFnIsCalled`가 핵심 불변식을 확인.

2. **NodeAttemptQueue burst 제어 성립** (Q2): goroutine submitBatch + NodeAttemptQueue(sem=3) → inner_max_concurrent=3. `TestBurstBoundary_DagSessionGoroutineSubmit`이 이를 증명.

3. **운영 관찰 경로 완전** (Q4): 6개 정지 원인 모두 observable signal 확보. K8s 의존 2개는 integration test로 이미 검증.

4. **Durable store 신뢰 수준 향상** (Q5): atomic write로 partial corruption 위험 제거. "개념 검증용" 등급 졸업.

5. **DagSession 범위 초과 없음** (Q3): 현재 cmd/*에서 DagSession 미사용 → 실질적 second engine으로 동작하지 않음.

**"채택 가능"이 아닌 이유 (production 전 필수 해결):**

1. **Ingress 경계가 주 실행 경로를 통과하지 않는다 (Q1 PARTIAL)**: cmd/pipeline-abc 등 5개 cmd/* 모두 RunGate 없이 dag.Start() 직접 호출. RunStore 불변식이 실제 실행 경로에서 성립하지 않는다.

2. **NodeAttemptQueue가 dag-go 경로에 미적용 (Q2 PARTIAL)**: DagSession 경로에서는 성립하나, 실제 cmd/* 경로는 dag-go → SpawnerNode.RunE를 사용. 이 경로에는 burst 제어가 없다.

3. **DagSession과 dag-go가 병렬 readiness 시스템으로 존재 (Q3 위험)**: cmd/*에 RunGate+DagSession을 연결하면 두 readiness 시스템 동시 존재 위험. 하나로 통일 필요.

위 3가지는 설계를 뒤집는 구조 문제가 아니다. RunGate를 cmd/*에 삽입하고 DagSession 사용 여부를 명확히 결정하면 된다. 경계 개념은 성립했다.

---

## 9. 다음 단계 제안

### 필수 (production 진입 전)

**[Step 1] RunGate를 cmd/* 실행 경로에 연결**

```go
// cmd/pipeline-abc/main.go 패턴:
rs, _ := store.NewJsonRunStore("/tmp/poc-runstore.json")
gate := ingress.NewRunGate(rs)

err := gate.Admit(ctx, "pipeline-abc-run-001", func(ctx context.Context) error {
    if !dag.Start() { return fmt.Errorf("dag start failed") }
    if !dag.Wait(ctx) { return fmt.Errorf("dag wait failed") }
    return nil
})
```

**[Step 2] dag-go 경로에 burst 제어 추가**

선택지 A: `BoundedDriver` — `driver.Driver`를 semaphore로 감싸서 SpawnerNode에 주입.

```go
type BoundedDriver struct {
    inner  driver.Driver
    sem    chan struct{}
}
func (b *BoundedDriver) Start(ctx context.Context, p driver.Prepared) (driver.Handle, error) {
    b.sem <- struct{}{}
    defer func() { <-b.sem }()
    return b.inner.Start(ctx, p)
}
```

선택지 B: DagSession을 dag-go 경로에 연결 (dag-go RunE callback → DagSession.NodeDone).

**[Step 3] DagSession 사용 여부 결정**

- 옵션 A: DagSession 사용 → dag-go를 DagSession에 연결, DagSession이 NodeAttemptQueue를 제어.
- 옵션 B: DagSession 폐기 → BoundedDriver로 burst 제어, dag-go가 유일한 orchestration engine.
- 옵션 B가 더 단순하며 두 번째 orchestration engine 위험을 제거한다.

### 단기 (production v0.1)

- K8sObserver를 DriverK8s.Wait() 내부에 통합 — 단일 wait 경로에서 Kueue pending / scheduler unschedulable 감지
- Bootstrap 재디스패치 루프 구현
- RunStore → PostgreSQL 인터페이스 교체 검토

---

*스프린트 커밋: poc + spawner 동시 변경*
