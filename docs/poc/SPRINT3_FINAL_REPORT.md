# Sprint 3 Final Report
**Date**: 2026-03-28
**Verdict**: PASS — Q1 PARTIAL 해소, Q2 PARTIAL 해소, 구조 고정

---

## Sprint 3 목표 요약

Sprint 2 결과 Q1(ingress boundary)과 Q2(release queue 실효성) 가 PARTIAL로 판정됨.
Sprint 3 목표: 이 두 PARTIAL을 완전히 해소하고, BoundedDriver 구현, Redis Streams 평가, DagSession 배치 확정.

---

## Round A: Main Path Ingress Wiring (Q1 fix)

### 변경 사항

**모든 7개 cmd/* 경로에 RunGate + JsonRunStore 적용:**

| cmd | RunGate import | gate.Admit | BoundedDriver |
|-----|---------------|------------|---------------|
| pipeline-abc | YES | YES | YES |
| fastfail | YES | YES | YES |
| fanout | YES | YES | YES |
| execclass | YES | YES | YES |
| dag-runner | YES | YES | YES |
| runner | YES | YES | YES |
| operational | YES | YES | YES |

**K8s init 실패 → graceful degradation:**
- `log.Fatalf("driver init: %v", err)` → `NopDriver` fallback + `log.Printf` (WARN)
- NopDriver는 `ErrK8sUnavailable`을 반환, 프로세스가 종료되지 않음
- RunStore에 `StateQueued`로 기록됨 → 재시작 후 복구 가능

### 증거 테스트

```
=== RUN   TestQ1_MainPathWiring_AllCmdUsesRunGate
Q1 PASS: all 7 cmd/* paths use RunGate (ingress boundary wired)
Q2 PASS: all 7 cmd/* paths use BoundedDriver (burst control wired)
SPRINT3 FIX: Q1 and Q2 PARTIAL eliminated — main path wiring complete
--- PASS
```

### 판정

**Q1: PASS** (이전: PARTIAL)
`pkg/ingress/RunGate` 가 모든 cmd/* 메인 실행 경로에 연결됨.
RunStore.Enqueue(queued) 는 dag.Start() 전에 반드시 호출됨.
Bootstrap recovery: 재시작 시 queued/held 상태 복구 로깅.

---

## Round B: BoundedDriver 구현 (Q2 fix)

### 구현 파일

`spawner/cmd/imp/bounded_driver.go`

```
BoundedDriver:
  - driver.BaseDriver 임베딩 → sealed interface 만족
  - semaphore channel (len=maxConcurrent) → Start() 호출 전 획득, 반환 후 해제
  - Prepare/Wait/Signal/Cancel: pass-through (K8s 리소스 생성 없음)
  - Stats(): Inflight/Available/MaxConcurrent 스냅샷
```

### 증거 테스트

```
=== RUN   TestBoundedDriver_LimitsConcurrentStart
Q2 PASS: BoundedDriver(sem=3) limited inner.Start() to max=3 concurrent
  goroutines=10, inner_max_concurrent=3 (capped at sem=3)
  PROOF: BoundedDriver controls K8s Job creation burst
--- PASS

=== RUN   TestBoundedDriver_ContextCancelWhileWaiting
PASS: blocked goroutine received error on ctx cancel: context deadline exceeded
--- PASS

=== RUN   TestBoundedDriver_Stats
PASS: Stats() reports MaxConcurrent=5 Available=5 Inflight=0
--- PASS

=== RUN   TestBoundedDriver_PreparePassthrough
PASS: Prepare() is pass-through, slot count unchanged (2)
--- PASS
```

### 아키텍처 배치 (확정)

```
cmd/* → RunGate → dag-go → SpawnerNode.RunE() → BoundedDriver(sem=3) → DriverK8s → K8s API
                    ↑
                    dag-go = 유일한 readiness/dependency source of truth
```

BoundedDriver는 SpawnerNode와 DriverK8s 사이에 삽입됨.
모든 K8s Job Create 호출이 semaphore를 통과해야 함.

### 판정

**Q2: PASS** (이전: PARTIAL)
BoundedDriver가 실제 cmd/* 실행 경로에 배치됨.
concurrent Submit 상한이 이제 main path에서 보장됨.

---

## Round C: Redis Streams 평가 (Q5 후속)

### 구현 파일

`poc/pkg/queue/redis_streams.go` (`//go:build redis`)

구현된 패턴:
- `XADD` → 메시지 enqueue
- `XREADGROUP` → consumer group 기반 at-least-once 소비
- `XACK` → PEL에서 제거 (처리 완료 확인)
- `XPENDING` → queue depth 관찰 (backpressure 신호)
- `XCLAIM` → 크래시된 consumer의 메시지 재취득

### Redis Streams vs JsonRunStore 비교

| 항목 | JsonRunStore | Redis Streams |
|------|-------------|---------------|
| 프로세스 재시작 생존 | YES (file) | YES (AOF/RDB) |
| 동시 multi-consumer | NO (mutex) | YES (consumer groups) |
| At-least-once 보장 | MANUAL | BUILT-IN (PEL + XACK) |
| 크래시 복구 | NO | YES (XCLAIM) |
| 고처리량 쓰기 | NO (full rewrite) | YES (append-only) |
| 수평 확장 | NO (single file) | YES |
| 관찰 가능성 | ListByState | XPENDING count |
| 설치 복잡도 | 없음 | 낮음 (docker redis) |

### PostgreSQL 대신 Redis Streams를 먼저 평가한 이유

1. **스키마 마이그레이션 불필요**: Redis Streams는 스키마리스
2. **빌트인 pending/ack/reclaim**: WAL 없이 exactly-once에 근접
3. **XPENDING 관찰**: queue depth 조회가 O(1) (SQL SELECT COUNT 불필요)
4. **PoC 마찰 최소화**: `docker run redis` 한 줄 vs PostgreSQL 스키마 설정

### 역할 배정 (확정)

```
ingress queue (run 제출):    Redis Streams (poc/pkg/queue)
release queue (Job 생성 제어): BoundedDriver semaphore (in-process)
run state 저장:               JsonRunStore → PostgreSQL (production)
readiness source of truth:    dag-go (변경 없음)
```

### 판정

**Q5 (후속): Redis Streams가 JsonRunStore보다 release queue 역할에 적합**
기술적 근거 확인됨. XADD/XREADGROUP/XACK/XCLAIM 패턴 구현 완료.
실제 Redis 서버 없이 default 빌드에서 제외됨 (`//go:build redis`).

---

## Round D: Observability 업데이트 (Q4 enhancement)

### 변경 사항

`poc/pkg/observe/pipeline_status.go`:
- `CauseReleaseQueue` 신호: `NodeAttemptQueue.Stats()` → `BoundedDriver.Stats().Inflight == MaxConcurrent`
- Sprint 3 main path 기준으로 업데이트됨

### 판정

**Q4: PASS** (유지)
6개 StopCause 모두 observable signal 존재.
cause 2 신호가 BoundedDriver (main path)로 갱신됨.

---

## Round E: DagSession 배치 확정

### 결정

`poc/pkg/session/dag_session.go` 패키지 주석에 명시적 레이블 추가:

```go
// ─── EXPERIMENTAL PATH (Sprint 3) ─────────────────────────────────────────────
// DagSession은 production cmd/* 실행 경로에 없음.
// 생산 메인 경로:
//   cmd/* → dag-go → SpawnerNode.RunE() → BoundedDriver → DriverK8s
//
// dag-go가 유일한 readiness/dependency source of truth.
// DagSession은 dag-go 의존성 로직을 미러링 → 두 개의 병렬 오케스트레이션 엔진.
// PoC 구조 검증용으로는 허용되나 production path에 도입해서는 안 됨.
```

DagSession이 존재하는 이유 (3가지):
1. 구조적 boundary 증명 (Sprint 2 Q2/Q3 검증)
2. NodeAttemptQueue 동작 단위 테스트 격리
3. 미래: dag-go 어댑터 복잡도 증가 시 대체 후보

### 판정

**Q3 (DagSession 역할): PASS** (유지)
실험적 경로로 명시적 레이블링됨.
어떤 cmd/*도 DagSession을 호출하지 않음.

---

## 전체 테스트 결과

### poc 레포지토리

```
ok  github.com/seoyhaein/poc/pkg/ingress     (4 tests + 1 new wiring test = 5)
ok  github.com/seoyhaein/poc/pkg/observe     (all Q4 tests)
ok  github.com/seoyhaein/poc/pkg/session     (all Q2/Q3 tests)
ok  github.com/seoyhaein/poc/pkg/store       (all Q5 tests)
```

새로 추가된 테스트:
- `TestQ1_MainPathWiring_AllCmdUsesRunGate` — 7/7 cmd/* PASS
- `TestBoundedDriver_LimitsConcurrentStart` — sem=3, 10 goroutines, max_observed=3
- `TestBoundedDriver_ContextCancelWhileWaiting` — slot 대기 중 ctx cancel
- `TestBoundedDriver_Stats` — Stats() 정확성
- `TestBoundedDriver_PreparePassthrough` — Prepare semaphore 비소비

### spawner 레포지토리

```
ok  github.com/seoyhaein/spawner/cmd/imp     (all BoundedDriver tests)
```

---

## Sprint 3 판정 요약

| 질문 | Sprint 2 | Sprint 3 |
|------|----------|----------|
| Q1: Ingress boundary main path? | PARTIAL | **PASS** |
| Q2: BoundedDriver/release queue 실효성? | PARTIAL | **PASS** |
| Q3: DagSession 역할 명확성? | PASS (risk noted) | **PASS** (labeled experimental) |
| Q4: 운영자 중단 상태 구분 가능? | PASS | **PASS** (BoundedDriver 신호 업데이트) |
| Q5: 내구성 저장소 수준? | PASS (conditional) | **PASS** (Redis Streams 평가 완료) |

**전체 판정: 조건부 가능 → 구조 확정**

Sprint 2의 PARTIAL들이 모두 해소됨.
10일 PoC의 구조적 핵심 질문들에 대한 답변이 코드와 테스트로 증명됨.

---

## 최종 아키텍처 (확정)

```
                    ┌─────────────────────────────────────────────────┐
                    │ cmd/* (pipeline-abc, fanout, fastfail, ...)      │
                    │   JsonRunStore (durable-lite state)              │
                    │   RunGate.Admit(runID, runFn)  ← Q1 FIXED       │
                    └──────────────────┬──────────────────────────────┘
                                       │ dag.Start() + dag.Wait()
                    ┌──────────────────▼──────────────────────────────┐
                    │ dag-go (readiness/dependency source of truth)    │
                    │   SpawnerNode.RunE(nodeID)                       │
                    └──────────────────┬──────────────────────────────┘
                                       │ Driver.Start()
                    ┌──────────────────▼──────────────────────────────┐
                    │ BoundedDriver(sem=3)  ← Q2 FIXED                │
                    │   semaphore → cap concurrent K8s Job Create      │
                    └──────────────────┬──────────────────────────────┘
                                       │ DriverK8s.Start()
                    ┌──────────────────▼──────────────────────────────┐
                    │ K8s API (Job Create, suspend=true)               │
                    │   Kueue → kube-scheduler → Pod                  │
                    └─────────────────────────────────────────────────┘

실험적 경로 (production 불가):
  DagSession → NodeAttemptQueue → SpawnerNode → BoundedDriver → DriverK8s

Redis Streams 역할 (평가 완료, 향후 ingress queue로 채택 가능):
  XADD(runID) → XREADGROUP → RunGate → dag-go path
```
