# Round 2: Release Control 경계 보고서

> 작성일: 2026-03-28
> 목적: 동시에 ready 상태가 된 노드들이 K8s API에 무제한으로 도달하는 것을 막는 NodeAttemptQueue를 구현하고 검증한다.

---

## 1. 배경 및 목표

### 현재 문제 (구현 전)

```
DagSession.submitBatch()
    │
    ├──► Submit(node-00) ──► K8s Job Create
    ├──► Submit(node-01) ──► K8s Job Create
    ├──► ...
    └──► Submit(node-09) ──► K8s Job Create
    (N개 ready → N번 즉시 K8s API 호출, 제한 없음)
```

실제 측정:
```
TestBurstBoundary_AllReadyAtStart (기준선):
  10개 독립 노드, submitBatch sequential
  max_concurrent=1, elapsed=52ms
  caller가 N개 DagSession을 동시에 실행하면 → N×M K8s API 호출
```

이 측정이 드러낸 것: DagSession 단위 내에서는 sequential이지만, DagSession 밖에서 여러 run이 동시에 실행될 경우 합산 K8s API 호출에 상한선이 없다.

### 이번 라운드 목표

- `NodeAttemptQueue` 구현: FIFO + block + semaphore
- `DagSession`이 `NodeSubmitter`로 `NodeAttemptQueue`를 받을 수 있도록 조합 검증
- burst=10, sem=3일 때 inner_max_concurrent ≤ 3 증명
- ctx 취소 시 슬롯 미소비(K8s Job 누락 없음) 증명

---

## 2. 구현 내용

### NodeAttemptQueue (poc/pkg/session/attempt_queue.go)

```go
type NodeAttemptQueue struct {
    sem   chan struct{}  // 동시 submit 제한
    inner NodeSubmitter
    // 관찰 카운터
    totalSubmits    atomic.Int64
    blockedSubmits  atomic.Int64
    maxConcurrent   atomic.Int64
    currentInflight atomic.Int64
}

func NewNodeAttemptQueue(inner NodeSubmitter, concurrency int) *NodeAttemptQueue

func (q *NodeAttemptQueue) Submit(ctx context.Context, nodeID string) error {
    // 1. 슬롯 즉시 획득 시도 (fast path)
    // 2. 실패 시 blocking 대기 또는 ctx 취소
    // 3. 슬롯 획득 후 inner.Submit 호출
    // 4. defer로 슬롯 반납
}

func (q *NodeAttemptQueue) Stats() (total, blocked, maxConcurrent int64)
func (q *NodeAttemptQueue) Concurrency() int
```

**설계 원칙:**
- **FIFO**: Go channel 의미론으로 대기 순서 보장
- **Block (drop 없음)**: Submit이 슬롯이 날 때까지 blocking. 조용한 K8s Job 누락 방지
- **Semaphore**: `chan struct{}` 크기 = concurrency limit
- `NodeSubmitter` 인터페이스를 구현 → DagSession과 직접 조합 가능 (DagSession 수정 불필요)

ASSUMPTION: production에서는 priority queue, fair-scheduling, back-pressure 신호 추가. v1은 release control seed만 구현.

---

## 3. 테스트 결과

```
go test ./pkg/session/... -v -run "TestBurst|TestNodeAttemptQueue"
```

### TestBurstBoundary_UnboundedBaseline (기준선)

```
OBSERVATION (no queue): 10 nodes, inner_max_concurrent=10
→ 모든 호출이 K8s API에 동시 도달
CONTRAST: with NodeAttemptQueue(sem=3), inner_max_concurrent is capped at 3
```

### TestBurstBoundary_BoundedRelease (sem=3)

```
PASS: 10 nodes, sem=3
      queue_max_concurrent=3
      inner_max_concurrent=3
      blocked=7

경계 성립: 7개가 슬롯 대기, K8s API 동시 호출 3으로 제한됨
```

### TestBurstBoundary_ContextCancelWhileBlocked

```
sem=1, slot-holder가 슬롯 점유 중
blocked submit에 10ms timeout 설정

PASS: blocked submit returned ctx.Err()=context deadline exceeded
→ 슬롯 미소비, K8s Job 누락 없음
```

### TestNodeAttemptQueue_DagSessionComposition

```
DagSession + NodeAttemptQueue(sem=2) 조합
A → B1/B2/B3/B4 (A 완료 후 4개 동시 ready)

결과: A+B1+B2+B3+B4 총 5개 제출 완료
      queue_max_concurrent=1 (sem=2)

NOTE: DagSession.submitBatch가 sequential이므로 queue를 통해도
      실질 max_concurrent=1. NodeAttemptQueue의 concurrency 효과는
      submitBatch를 goroutine으로 변경하거나 여러 DagSession이
      같은 queue를 공유할 때 발휘됨.
```

**총 4/4 PASS** (Round 1 테스트 포함 전체 15/15 PASS)

---

## 4. 경계 성립 여부

| 검증 항목 | 성립 여부 | 근거 |
|-----------|-----------|------|
| sem=3일 때 inner_max_concurrent ≤ 3 | ✅ | `TestBurstBoundary_BoundedRelease` |
| drop 없음 (block 정책) | ✅ | 10개 모두 제출 완료 |
| ctx 취소 시 슬롯 미소비 | ✅ | `TestBurstBoundary_ContextCancelWhileBlocked` |
| DagSession과 인터페이스 조합 | ✅ | `TestNodeAttemptQueue_DagSessionComposition` |
| 기존 DagSession 불변식(ready only) 유지 | ✅ | 기존 5개 테스트 동시 PASS |

---

## 5. 관찰: DagSession.submitBatch와의 조합 한계

현재 DagSession.submitBatch:
```go
func (s *DagSession) submitBatch(ctx context.Context, nodes []*nodeEntry) error {
    for _, n := range nodes {
        if err := s.submitter.Submit(ctx, n.id); err != nil { ... }  // sequential
        ...
    }
    return nil
}
```

Sequential 호출이므로 NodeAttemptQueue를 삽입해도 **단일 DagSession 내에서는** max_concurrent=1.

NodeAttemptQueue의 semaphore 효과가 발휘되는 시나리오:
1. 여러 DagSession이 **같은** NodeAttemptQueue 인스턴스를 공유할 때
2. submitBatch를 goroutine 기반으로 변경할 때 (다음 diff 후보)

현재 v1은 "release control seed"로서 구조를 갖추는 것이 목적. 실제 concurrent submit은 위 변경 후 측정 필요.

---

## 6. 남은 리스크

| 리스크 | 심각도 |
|--------|--------|
| DagSession.submitBatch sequential → NodeAttemptQueue 효과 단일 세션에서 미발휘 | 중간 |
| submitBatch goroutine화 시 DagSession 내부 상태(nodeHeld→nodeSubmitted) race 가능성 검토 필요 | 중간 |
| NodeAttemptQueue v1에 priority/fair-scheduling 없음 | 낮음 (ASSUMPTION으로 명시) |

---

## 7. 수정 파일 목록

| 파일 | 변경 |
|------|------|
| `poc/pkg/session/attempt_queue.go` | NEW — NodeAttemptQueue 구현 |
| `poc/pkg/session/attempt_queue_test.go` | NEW — 단위 테스트 4개 |
