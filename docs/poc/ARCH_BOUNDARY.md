# ARCH_BOUNDARY

> 작성일: 2026-03-28
> 목적: 구조 검증. production 최종안 아님. 미결 가정은 명시함.

---

## 1. 현재 구조 (코드 기준)

```
사용자 요청
    │
    ▼
FrontDoor.Resolve()          ← SpawnKey + Command 생성
    │
    ▼
Dispatcher.Handle()          ← Factory.Bind() → Actor 할당 → Loop 시작
    │
    ▼
Actor.Loop() / K8sActor      ← CmdRun 수신
    │
    ▼
DriverK8s.Prepare/Start      ← K8s Job 즉시 생성 (suspend=true + kueue label)
    │
    ▼
Kueue admission              ← workload 생성 → quota 확인 → admit
    │
    ▼
kube-scheduler               ← admitted Pod → node placement
    │
    ▼
DriverK8s.Wait               ← 2s 폴링으로 Job 완료 감시
```

poc에서의 실제 흐름 (cmd/execclass 등):
```
main()
  └─ dag-go InitDag/AddEdge/FinishDag
  └─ SpawnerNode.RunE() ← dag-go worker pool에서 호출
       └─ DriverK8s.Prepare → Start → Wait   ← K8s Job 즉시 생성
```

---

## 2. 현재 구조 문제 / 경계 위반 후보

### 2.1 RunStore/RunQueue 없음 [경계 위반]

- `Dispatcher.Handle()`이 수신 즉시 Actor로 라우팅 → **사용자 burst가 곧바로 K8s API burst**
- 재시작 시 진행 중인 run 상태 유실 가능 (InMemoryRunStore도 없음)
- 가설 1 "service 앞단의 Run queue / session store가 받는다" 현재 구현 없음

### 2.2 "ready node만 submit" 경계 불명시 [경계 위반]

- `SpawnerNode.RunE()`가 직접 `DriverK8s.Start()` 호출
- dag-go worker pool이 순서를 제어하긴 하지만, "ready node attempt"라는 개념이 명시적 경계로 코드에 없음
- A→B→C에서 A가 submit될 때 B, C는 K8s Job으로 생성되지 않지만,
  이것이 RunQueue/DagSession이 막아서가 아니라 dag-go가 RunE를 호출 안 했을 뿐임
- 가설 2 "dag-go actor/session이 ready node attempt만 만든다" 경계 불명시

### 2.3 DAG 전체 run이 Kueue admission 단위처럼 보임 [설계 오해 유발]

- 현재: poc/cmd/*/main.go에서 DAG 전체를 하나의 실행 단위로 제출
- Kueue는 node별 K8s Job을 admission하지만, 제출 주체가 DAG를 단위로 생각하고 있음
- 가설 5 "DAG 전체 run은 Kueue admission 단위가 아니다" 구조에서 명시되지 않음

### 2.4 K8s 클라이언트가 없으면 바로 panic [앞단 queue 미비 증거]

- `spawner/cmd/server/main.go`: `NewK8sFromKubeconfig` 실패 시 `panic`
- 앞단 queue가 있으면 K8s 연결 실패를 graceful하게 처리 가능 (queued 상태 유지)

### 2.5 Kueue pending vs kube-scheduler unschedulable 미구분

- 현재 Wait 폴링은 `JobComplete` 또는 `JobFailed` 조건만 감시
- Kueue가 quota 초과로 pending한 것과 kube-scheduler가 node affinity로 unschedulable한 것을 구분하는 관찰 경로 없음
- 두 가지의 관찰 포인트가 다름:
  - Kueue pending: `workload.status.conditions[QuotaReserved=False]`
  - kube-scheduler unschedulable: `pod.status.conditions[PodScheduled=False, reason=Unschedulable]`

---

## 3. 제안 최소 인터페이스

### 3.1 RunStore

```go
// poc/pkg/store/run_store.go
type RunState string
const (
    StateQueued        RunState = "queued"          // 제출됨, 처리 대기
    StateHeld          RunState = "held"             // 명시적 보류
    StateResumed       RunState = "resumed"          // held → 재개 요청
    StateCanceled      RunState = "canceled"         // terminal
    StateAdmittedToDag RunState = "admitted-to-dag"  // dag-go 세션에 전달됨
    StateRunning       RunState = "running"          // node attempt 실행 중
    StateFinished      RunState = "finished"         // terminal
)

type RunRecord struct {
    RunID     string
    State     RunState
    Payload   []byte    // serialized DAG spec
    CreatedAt time.Time
    UpdatedAt time.Time
}

type RunStore interface {
    Enqueue(ctx context.Context, rec RunRecord) error
    Get(ctx context.Context, runID string) (RunRecord, bool, error)
    UpdateState(ctx context.Context, runID string, from, to RunState) error
    ListByState(ctx context.Context, state RunState) ([]RunRecord, error)
    Delete(ctx context.Context, runID string) error
}
```

구현:
- `InMemoryRunStore`: 빠르고 단순, 재시작 시 유실
- `JsonRunStore` (durable-lite): 파일 기반, 재시작 복구 가능

ASSUMPTION: production에서는 PostgreSQL/Redis 등으로 교체. 지금은 구조만 검증.

### 3.2 NodeSubmitter (DagSession 내부 경계)

```go
// poc/pkg/session/dag_session.go
type NodeSubmitter interface {
    Submit(ctx context.Context, nodeID string) error
}
```

- production: `spawner.DriverK8s.Start()` wrapping
- test: mock으로 교체 → Submit 호출 시점/횟수 검증

### 3.3 DagSession

```go
type DagSession struct { /* dependency graph + NodeSubmitter */ }
func (s *DagSession) AddNode(nodeID string, deps ...string)
func (s *DagSession) Start(ctx context.Context) error          // ready node만 Submit
func (s *DagSession) NodeDone(ctx context.Context, nodeID string, success bool) error
func (s *DagSession) HeldNodes() []string                      // test용
func (s *DagSession) SubmittedNodes() []string                 // test용
```

### 3.4 상태 전이 정책

```
queued ──────────────────────────────────► admitted-to-dag
  │                                               │
  ▼                                               ▼
held ──► resumed ──► admitted-to-dag ──► running ──► finished
  │          │             │                │
  └──────────┴─────────────┴────────────────┴──► canceled
```

유효 전이만 허용, 역방향/terminal→any 거부.

ASSUMPTION: "held" 사용 시점(rate limit vs manual hold vs policy hold)은 미결.
ASSUMPTION: "resumed"가 직접 "admitted-to-dag"로 가는지 "queued"로 돌아가는지 미결.

---

## 4. 테스트 목록 (각 테스트가 증명하는 것)

| 테스트 | 파일 | 증명하는 것 |
|--------|------|------------|
| `TestValidateTransition_Valid` | run_store_test.go | 유효 전이가 허용됨 |
| `TestValidateTransition_Invalid` | run_store_test.go | 역방향/terminal 전이가 거부됨 |
| `TestMemoryStore_DoesNotSurviveReset` | run_store_test.go | InMemoryStore는 재시작 시 데이터 유실 |
| `TestJsonStore_RecoveryAfterRestart` | run_store_test.go | JsonStore는 재시작 후 queued run 복구 |
| `TestMemoryStore_StateTransition` | run_store_test.go | UpdateState가 상태 기계 정책을 강제 |
| `TestOnlyReadyNodesSubmitted` | dag_session_test.go | A→B→C에서 Start() 직후 A만 Submit됨 |
| `TestNonReadyNodeNotSubmitted` | dag_session_test.go | B는 A 완료 전 K8s Job으로 생성 안 됨 |
| `TestFastFail_DependantNotSubmitted` | dag_session_test.go | B 실패 시 C가 Submit 안 됨 |
| `TestBurstBoundary_AllReadyAtStart` | dag_session_test.go | queue 경계 없으면 N개 독립 node가 동시 Submit — K8s API burst 가능성 노출 |
| `TestKueuePendingVsUnschedulable` | kueue_observe_test.go (integration) | Kueue quota pending과 kube-scheduler unschedulable의 조건 필드 차이 관찰 |

---

## 5. gang scheduling 경로 (최소 예제 가정)

ASSUMPTION: gang이 필요한 노드는 예외 경로로 처리.
- 일반 노드: `DagSession` → `NodeSubmitter` → K8s Job (Kueue suspend=true)
- gang 노드: `DagSession.NodeGroupPromotion()` → Kueue Workload 직접 생성 (PodSet 여러 개)
- 현재 구현 없음. 최소 예제는 `cmd/gangscheduling/`으로 별도 검증 예정.

---

## 6. 구조 리스크 (현재 남는 것)

| 리스크 | 심각도 | 처리 시점 |
|--------|--------|----------|
| RunStore 없음 → 재시작 시 run 유실 | 높음 | 이번 diff에서 인터페이스 + 테스트 추가 |
| user burst → K8s API burst | 높음 | NodeAttemptQueue 추가 (다음 diff) |
| Kueue pending vs unschedulable 미구분 | 중간 | integration test로 관찰 포인트 확인 |
| gang 노드 경로 없음 | 중간 | 최소 예제 예정 |
| DagSession이 dag-go와 분리 → dag-go 역할 재정의 필요 | 낮음 | production 단계 |

---

## 7. 다음 single small diff 제안

**`NodeAttemptQueue` 추가 (concurrency limit)**

```go
// poc/pkg/session/attempt_queue.go
type NodeAttemptQueue struct {
    sem     chan struct{} // 동시 submit 제한
    submitter NodeSubmitter
}
```

- `DagSession.submitAll()`이 직접 submitter 호출 대신 `NodeAttemptQueue`를 거치도록
- burst 실험 재실행: 10개 독립 노드 → sem=3 → 최대 3개 동시 Submit 검증
- 이것이 "사용자 burst가 K8s API burst가 되지 않도록 앞단 queue 경계" 역할

ASSUMPTION: NodeAttemptQueue의 backpressure 정책 (drop vs block vs park)은 미결.
