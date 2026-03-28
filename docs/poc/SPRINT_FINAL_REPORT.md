# 구조 검증 스프린트 최종 보고서

> 작성일: 2026-03-28
> 스프린트 목표: Ingress / Release Control / Observability 세 경계 구현 및 판정

---

## 1. 수정 파일 목록

### spawner repo

| 파일 | 변경 | 내용 |
|------|------|------|
| `pkg/store/run_store.go` | NEW | RunStore 인터페이스, RunState 상태 기계 (7개 상태), ValidateTransition |
| `pkg/store/memory_run_store.go` | NEW | InMemoryRunStore |
| `pkg/store/json_run_store.go` | NEW | JsonRunStore (파일 기반, 재시작 복구) |
| `pkg/store/run_store_test.go` | NEW | 단위 테스트 5개 |
| `pkg/error/error.go` | MOD | `ErrK8sUnavailable` 추가 |
| `pkg/dispatcher/dispatcher.go` | MOD | `runStore`, `k8sAvailable` 필드; `WithRunStore`, `WithK8sUnavailable`, `Bootstrap()` 추가; Handle() ingress gate 삽입 |
| `pkg/dispatcher/ingress_test.go` | NEW | 단위 테스트 6개 |
| `cmd/imp/nop_driver.go` | NEW | NopDriver — K8s 불가 시 fallback, ErrK8sUnavailable 반환 |
| `cmd/imp/k8s_observer.go` | NEW | K8sObserver — Kueue workload + Pod scheduling 관찰 |
| `cmd/imp/k8s_driver.go` | MOD | `buildConfig()` 헬퍼 도입 (InCluster → KUBECONFIG → ~/.kube/config) |
| `cmd/server/main.go` | MOD | panic → graceful (NopDriver + JsonRunStore + Bootstrap) |

### poc repo

| 파일 | 변경 | 내용 |
|------|------|------|
| `pkg/session/attempt_queue.go` | NEW | NodeAttemptQueue — FIFO + block + semaphore |
| `pkg/session/attempt_queue_test.go` | NEW | 단위 테스트 4개 |
| `pkg/integration/kueue_observe_test.go` | NEW | integration 테스트 2개 (`//go:build integration`) |

---

## 2. 구현한 경계

### Round 1: Ingress 경계

**구현 전:**
```
사용자 → Dispatcher.Handle() → Actor → K8s Job
         (즉시 전달, 큐 없음, panic on K8s failure)
```

**구현 후:**
```
사용자 → RunStore.Enqueue(queued)
           │
           ▼
         K8s 가용?
           │ 아니오 → queued → held (run 보존, Actor 미호출)
           │ 예     → Dispatcher.Handle() → Actor
           │                                   │
           └──────── queued → admitted-to-dag ◄┘
재시작 시: Bootstrap() → queued + admitted 복구
```

### Round 2: Release Control 경계

**구현 전:**
```
DagSession.submitBatch() → N×Submit 직접 호출 → K8s API burst
```

**구현 후:**
```
DagSession.submitBatch() → NodeAttemptQueue(sem=k) → 최대 k개 동시 호출
```

### Round 3: Observability 경계

**구현 전:**
```
DriverK8s.Wait() → JobComplete/JobFailed만 감시
Kueue pending ≡ kube-scheduler unschedulable (구분 불가)
```

**구현 후:**
```
K8sObserver.ObserveWorkload() → workload.status.conditions[QuotaReserved]
K8sObserver.ObservePod()      → pod.status.conditions[PodScheduled]
두 상태를 독립적인 관찰 경로로 구분 가능
```

---

## 3. 테스트/실험 목록

### 단위 테스트 (go test ./... — K8s 불필요)

| 테스트 | 위치 | 결과 |
|--------|------|------|
| `TestValidateTransition_Valid/Invalid` | spawner/pkg/store | PASS |
| `TestMemoryStore_DoesNotSurviveReset` | spawner/pkg/store | PASS |
| `TestMemoryStore_HeldOnK8sUnavailable` | spawner/pkg/store | PASS |
| `TestMemoryStore_StateTransition` | spawner/pkg/store | PASS |
| `TestJsonStore_RecoveryAfterRestart` | spawner/pkg/store | PASS |
| `TestIngress_EnqueuesRunAsQueuedBeforeDispatching` | spawner/pkg/dispatcher | PASS |
| `TestIngress_TransitionsToAdmittedOnSuccessfulDispatch` | spawner/pkg/dispatcher | PASS |
| `TestIngress_RunHeldNotDispatchedWhenK8sUnavailable` | spawner/pkg/dispatcher | PASS |
| `TestIngress_BootstrapRecoversByState` | spawner/pkg/dispatcher | PASS |
| `TestIngress_BootstrapIsNopWithoutRunStore` | spawner/pkg/dispatcher | PASS |
| `TestIngress_IdempotentEnqueue` | spawner/pkg/dispatcher | PASS |
| `TestBurstBoundary_BoundedRelease` | poc/pkg/session | PASS |
| `TestBurstBoundary_UnboundedBaseline` | poc/pkg/session | PASS |
| `TestBurstBoundary_ContextCancelWhileBlocked` | poc/pkg/session | PASS |
| `TestNodeAttemptQueue_DagSessionComposition` | poc/pkg/session | PASS |
| (기존) `TestOnlyReadyNodesSubmitted` 등 5개 | poc/pkg/session | PASS |
| (기존) `TestValidateTransition_*` 등 5개 | poc/pkg/store | PASS |

**총 단위 테스트: 26개 전부 PASS**

### Integration 테스트 (kind + Kueue — `//go:build integration`)

```
go test ./pkg/integration/... -v -tags integration
```

| 테스트 | 설명 | 결과 |
|--------|------|------|
| `TestObserveKueuePending_QuotaExceeded` | CPU=5000m > 4000m quota → Kueue pending 관찰 | PASS |
| `TestObserveUnschedulable_NodeSelectorMismatch` | CPU=100m 정상 admit + nodeSelector 불일치 → scheduler 구분 | PASS |

---

## 4. 관찰 결과

### 4.1 Ingress

```
TestIngress_RunHeldNotDispatchedWhenK8sUnavailable:
  k8s unavailable → run teamA:run-001 → held
  Actor.EnqueueCtx 호출 없음 ← 경계 성립
  RunStore 상태: held (유실 없음)

TestIngress_BootstrapRecoversByState:
  Bootstrap recovered 3 runs (queued + admitted-to-dag)
  r1: queued, r2: queued, r3: admitted-to-dag 모두 복구됨

TestJsonStore_RecoveryAfterRestart:
  OBSERVATION: JsonRunStore recovered 1 queued + 1 admitted after restart
  InMemoryRunStore: 2 queued runs lost on reset
```

### 4.2 Release Control

```
TestBurstBoundary_UnboundedBaseline (기준선):
  OBSERVATION: 10 nodes, inner_max_concurrent=10
  → 모든 호출이 K8s API에 동시 도달

TestBurstBoundary_BoundedRelease (sem=3):
  PASS: 10 nodes, sem=3, queue_max_concurrent=3, inner_max_concurrent=3, blocked=7
  → 7개 슬롯 대기, K8s API 동시 호출 3으로 제한됨

TestBurstBoundary_ContextCancelWhileBlocked:
  PASS: blocked submit returned ctx.Err()=context deadline exceeded
  → 취소 시 슬롯 미소비, K8s Job 누락 없음
```

### 4.3 Kueue pending vs kube-scheduler unschedulable 구분

```
[Kueue pending — CPU=5000m > quota=4000m]:
  workload=job-obs-quota-*
  QuotaReserved=false
  Admitted=false
  PendingReason="couldn't assign flavors to pod set main: insufficient quota for cpu
    in flavor default-flavor, previously considered podsets requests (0) + current
    podset request (5) > maximum capacity (4)"
  pod: NOT FOUND (Kueue never unsuspended the Job)
  관찰 경로: workload.status.conditions[QuotaReserved=False]

[kube-scheduler unschedulable — CPU=100m, nodeSelector 불일치]:
  workload=job-obs-unsched-manual-*
  QuotaReserved=true, Admitted=true ← Kueue는 통과
  pod=obs-unsched-manual-blzkt: Pending
  pod.status.conditions[PodScheduled]:
    status: False
    reason: Unschedulable
    message: "0/1 nodes are available: 1 node(s) didn't match Pod's node
      affinity/selector."
  관찰 경로: pod.status.conditions[PodScheduled=False]

핵심 구분:
  Kueue pending   → workload 조건 관찰, pod 없음
  scheduler 불가  → pod 조건 관찰, workload는 admitted
  둘 다 "pending"처럼 보이지만 수정 방법이 다름
```

---

## 5. 남은 리스크

| 리스크 | 심각도 | 상태 |
|--------|--------|------|
| RunStore ↔ Dispatcher 연결이 spawner 내에만 존재; dag-go 경로(poc/cmd/*)와 미연결 | 높음 | production 연결 필요 |
| NodeAttemptQueue 이점이 DagSession sequential submitBatch에서 제한됨 (max=1) | 중간 | submitBatch goroutine화 필요 |
| JsonRunStore가 non-atomic write (크래시 시 partial write 가능) | 중간 | production: WAL 또는 PostgreSQL |
| K8sObserver가 Wait()와 분리됨 — 운영 시 두 경로 동시 모니터링 필요 | 중간 | production 단계 설계 |
| Bootstrap이 RunRecord.Payload 역직렬화 → 재디스패치를 caller에 위임 | 낮음 | 명시적 ASSUMPTION으로 문서화됨 |

---

## 6. 구조 판정

### **조건부 가능 (Conditional Go)**

---

## 7. 판정 근거

**채택 가능 기준:** ingress / release / observability 세 경계가 모두 실제로 동작하고 큰 구조 위반이 없음

**판정: "조건부 가능"으로 결정한 이유:**

**성립한 것 (3개 경계 모두 동작):**

1. **Ingress 경계** — 실제로 동작함
   - `Handle()` 진입 시 RunStore.Enqueue(queued) 먼저 호출 (테스트 증명)
   - K8s 불가 시 panic 없이 held 상태 보존 (테스트 증명)
   - Bootstrap()이 queued + admitted 복구 (테스트 증명)
   - JsonRunStore가 재시작 후 상태 복구 (테스트 증명)

2. **Release Control 경계** — 실제로 동작함
   - NodeAttemptQueue(sem=3)로 K8s API 동시 호출 3개로 제한 (테스트 증명)
   - ctx 취소 시 슬롯 미소비, K8s Job 누락 없음 (테스트 증명)
   - DagSession과 조합 검증 완료

3. **Observability 경계** — 실제로 동작함
   - Kueue pending: `workload.status.conditions[QuotaReserved=False]` — 실 클러스터 관찰 완료
   - kube-scheduler unschedulable: `pod.status.conditions[PodScheduled=False]` — 실 클러스터 관찰 완료
   - 두 경로가 완전히 다른 K8s 객체에서 관찰됨 (구분 성립)

**"채택 가능"이 아닌 이유 (production 전 필수 해결 항목):**

1. **Ingress 경계가 dag-go 경로(poc/cmd/*)와 단절됨**
   - 현재 RunStore ↔ Dispatcher 연결은 spawner의 서버 경로에만 존재
   - poc/cmd/execclass, pipeline-abc 등 실제 실행 경로는 여전히 DagSession → SpawnerNode.RunE() → DriverK8s 직접 호출
   - "사용자 제출이 RunStore를 거쳐야 한다"는 불변식이 전체 경로에 걸쳐 성립하지 않음

2. **NodeAttemptQueue이 DagSession과 sequential 조합에서 효과 미발휘**
   - DagSession.submitBatch()가 sequential → max_concurrent=1
   - 실제 burst 제한 효과를 보려면 submitBatch goroutine화 필요

3. **JsonRunStore non-atomic write**
   - 프로세스 크래시 중 파일 기록 시 partial write 가능
   - production 신뢰도 미달

위 항목들은 설계를 뒤집는 구조적 문제가 아니다. 경계 개념은 성립했으며 implementation gap이 남은 것이다.

---

## 8. 다음 큰 단계 제안

### 필수 (production 진입 전)

**[Step 1] Ingress 경계를 poc/cmd/* 경로에도 연결**

현재 poc/cmd/execclass 등은 RunStore를 거치지 않음. 다음 구조로 연결 필요:

```
사용자 제출 → RunStore.Enqueue(queued)
                  ↓
              RunAdmitter.Admit() → queued → admitted-to-dag
                  ↓
              DagSession 생성 + NodeAttemptQueue 주입
                  ↓
              dag-go 실행 (SpawnerNode.RunE → DriverK8s)
```

**[Step 2] DagSession.submitBatch를 goroutine으로 변경**

NodeAttemptQueue의 semaphore 효과 발휘를 위해 submitBatch가 Submit을 goroutine으로 호출하도록 변경. DagSession의 기존 불변식(ready node only) 유지.

**[Step 3] JsonRunStore atomic write**

현재 `os.WriteFile`을 `os.WriteFile(tmpPath) + os.Rename` 패턴으로 교체. crash-safe write.

### 단기 (production v0.1)

- K8sObserver를 DriverK8s.Wait() 내부에 통합 (단일 관찰 경로)
- Bootstrap 재디스패치 루프 구현 (RunRecord.Payload → RunSpec 역직렬화)
- spawner gRPC 서버 완성 (현재 main.go는 데모 수준)

### 장기 (production v1.0)

- RunStore → PostgreSQL 교체 (인터페이스 변경 없이 구현만 교체)
- NodeAttemptQueue → priority queue + fair scheduling
- Kueue WorkloadPriorityClass 연동
- K8s Watch 기반 Wait (현재 2초 폴링 교체)

---

*스프린트 커밋:*
- *spawner: `6884ee4` — arch/round1: Ingress boundary*
- *poc: `6b40124` — arch/round2+3: NodeAttemptQueue + integration test*
