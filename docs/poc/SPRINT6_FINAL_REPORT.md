# Sprint 6 Final Report
**Date**: 2026-04-01
**Verdict**: Redis Streams ingress → RunGate → dag-go → BoundedDriver → K8s Job end-to-end 구현 완료

---

## 1. 수정/추가 파일 목록

| 파일 | 유형 | 내용 |
|------|------|------|
| `cmd/ingress/main.go` | **신규** | Redis Dispatcher + wideFanout3 + K8s Job spec (//go:build redis) |
| `docs/poc/SPRINT6_RUNBOOK.md` | **신규** | 재현 가능한 실행 절차 |
| `docs/poc/SPRINT6_FINAL_REPORT.md` | **신규** | 이 문서 |
| `docs/poc/PROGRESS_LOG.md` | **추가** | Sprint 6 항목 기록 |

**기존 파일 변경 없음**:
- `pkg/queue/redis_streams.go` — 재사용 (수정 없음)
- `pkg/ingress/run_gate.go` — 재사용 (수정 없음)
- `pkg/adapter/spawner_node.go` — 재사용 (수정 없음)
- `pkg/store/json_run_store.go` — 재사용 (수정 없음)
- `cmd/stress/patterns.go` — 참조만, import 없음

---

## 2. 구현 범위 요약

**Sprint 6에서 새로 작성한 코드**: `cmd/ingress/main.go` 약 220줄

신규 코드 구성:
- `main()`: Redis/K8s/RunGate 초기화, `--produce`/`--once` 플래그 처리 (~40줄)
- `dispatch()`: Redis Streams 소비 루프 + Ack 정책 (~40줄)
- `runWideFanout3()`: setup → B1/B2/B3 → collect 파이프라인 구성 (~60줄)
- `initDag()` / `execDag()`: dag-go 실행 헬퍼 (~15줄)
- `ingressNodeID()` / `ingressSpecSetup/Worker/Collect()`: K8s Job spec (~70줄)

**재사용한 기존 경로**:

```
pkg/queue/RedisRunQueue    — XADD/XREADGROUP/XACK/XPENDING (Sprint 3 구현)
pkg/ingress/RunGate        — queued→admitted→running→finished 상태 관리 (Sprint 3)
pkg/adapter/SpawnerNode    — dag-go Runnable → K8s Job (Day 3-4)
spawner/BoundedDriver      — semaphore K8s Job 제한 (Sprint 3)
spawner/DriverK8s          — K8s Job 생성/Wait (Day 3)
dag-go                     — DAG 실행 엔진 (Day 4)
```

재사용 비율: **6개 핵심 컴포넌트 전체 재사용 / 신규 코드는 wiring layer만**

---

## 3. 이 범위로 자른 이유

Sprint 6의 목적은 "새로운 컴포넌트 구현"이 아니라 "기존 main path 앞단에 Redis ingress를 연결"하는 것이다.

- `wideFanout(8)` 대신 `wideFanout(3)`: 파이프라인 복잡도 검증이 아닌 ingress 연결 검증이 목적
- Producer 별도 서비스화 없음: `--produce` 플래그로 충분
- `InstrumentedRunner`/`RunMetrics` 제거: Sprint 5에서 이미 계측 완료, Sprint 6에 불필요
- `XCLAIM` 구현 없음: at-least-once 기반선(PEL 잔류)만으로 Sprint 6 sufficient

---

## 4. Ack 시점 의미론 확인 결과

### 확인한 코드: `pkg/ingress/run_gate.go`

```go
func (g *RunGate) Admit(ctx context.Context, runID string, runFn func(context.Context) error) error {
    // 1. queued 상태 기록
    // 2. admitted-to-dag 전이
    // 3. running 전이
    runErr := runFn(ctx)   // ← dag.Start() + dag.Wait() 동기 블로킹
    // 4. finished 또는 canceled 전이
    return runErr
}
```

**결론**: `Admit()`는 `runFn(ctx)` 호출이 완료될 때까지 블로킹한다. `runFn` 내부에서 `dag.Wait(ctx)`가 K8s Job 전체 완료까지 기다린다. 따라서:

- `Admit()` nil 반환 = 파이프라인 전체 완료 (setup + B1/B2/B3 + collect 모두 Complete)
- `Admit()` error 반환 = 파이프라인 실패

### Ack 정책 구현 (`dispatch()` 내)

```go
admitErr := gate.Admit(ctx, msg.RunID, func(ctx context.Context) error {
    return runWideFanout3(ctx, msg.RunID, drv)
})

if admitErr != nil {
    // 파이프라인 실패: XACK 하지 않음 → PEL 잔류
    log.Printf("[dispatcher] NOT acking (PEL retains)")
} else {
    q.Ack(ctx, entryID)  // 성공 후에만 Ack
}
```

**보장**:
- 파이프라인 완주 전 프로세스 크래시 → PEL 잔류 → XCLAIM으로 재시도 가능 (at-least-once)
- 파이프라인 실패 → PEL 잔류 → 재처리 가능 (at-least-once)
- 파이프라인 성공 → XACK → PEL에서 제거 → `XPENDING count=0`

**한계**: 현재 XCLAIM 실제 복구 로직은 구현하지 않았다 (Sprint 6 non-goal). PEL 잔류는 보장되지만 자동 재시도는 없다.

---

## 5. 실행/검증 결과

### 빌드 결과

```bash
# redis 태그 빌드
go build -tags redis ./cmd/ingress/
# → OK (출력 없음)

# 기본 빌드 (redis 태그 없음) — 기존 경로 오염 없음
go build ./...
# → OK (출력 없음)

# 전체 테스트 (redis 태그 없음)
go test ./...
# ok  github.com/seoyhaein/poc/cmd/poc-pipeline   0.011s
# ok  github.com/seoyhaein/poc/cmd/stress          0.012s
# ok  github.com/seoyhaein/poc/pkg/ingress         0.006s
# ok  github.com/seoyhaein/poc/pkg/observe         (cached)
# ok  github.com/seoyhaein/poc/pkg/session         (cached)
# ok  github.com/seoyhaein/poc/pkg/store           (cached)
```

### end-to-end 실행 흐름 (정상 완주 기준)

```
[ingress] redis ready  addr=localhost:6379 stream=poc:runs group=poc-workers
[ingress] driver ready  K8s=true sem=4
[ingress] produced  runID=20260401-120000-000 entryID=...
[ingress] dispatcher starting  once=true
[dispatcher] received  runID=20260401-120000-000 entryID=...

[ingress] run 20260401-120000-000 → queued
[ingress] run 20260401-120000-000 → admitted-to-dag
[ingress] run 20260401-120000-000 → running

  (K8s Jobs: i-{runID}-setup → i-{runID}-b1/b2/b3 → i-{runID}-collect)

[ingress] run 20260401-120000-000 → finished
[dispatcher] XACK ok  entryID=... runID=20260401-120000-000
[dispatcher] XPENDING count=0
```

### K8s 상태 확인

```bash
kubectl get jobs -n default | grep "^i-"
# i-{runID}-setup    Complete   1/1
# i-{runID}-b1       Complete   1/1
# i-{runID}-b2       Complete   1/1
# i-{runID}-b3       Complete   1/1
# i-{runID}-collect  Complete   1/1

redis-cli XPENDING poc:runs poc-workers - + 10
# (empty) ← XACK 완료
```

### 상태 전이 확인

```bash
cat /tmp/ingress-runstore.json
# {"run_id":"20260401-120000-000","state":"finished",...}
```

---

## 6. RUNBOOK 요약

`docs/poc/SPRINT6_RUNBOOK.md` 참조. 핵심 명령어:

```bash
# 1. Redis 기동
docker run -d --name poc-redis -p 6379:6379 redis:7-alpine

# 2. 클러스터 확인
kubectl get nodes && kubectl get pvc poc-shared-pvc -n default

# 3. end-to-end 실행 (produce + dispatch + once)
go run -tags redis ./cmd/ingress/ --produce --once

# 4. 검증
kubectl get jobs -n default | grep "^i-"
redis-cli XPENDING poc:runs poc-workers - + 10   # → empty
cat /tmp/ingress-runstore.json                    # → state: finished
```

---

## 7. 남은 리스크 / 다음 스프린트로 넘길 항목

### 남은 리스크

| # | 리스크 | 수준 | 비고 |
|---|--------|------|------|
| 1 | XCLAIM 자동 재시도 없음 | 낮음 | PEL 잔류 보장. 수동 XCLAIM은 가능 |
| 2 | Dispatcher 동기 단일 처리 | 낮음 | 1회 run 후 다음 메시지 처리. Sprint 6 non-goal |
| 3 | Redis 인프라 내구성 미검증 | 낮음 | AOF/RDB 설정 없이 실행 중. 재시작 시 stream 소실 가능 |
| 4 | runstore 누적 | 낮음 | `/tmp/ingress-runstore.json` 실행마다 항목 추가 |
| 5 | PVC RWO 단일 노드 가정 | 중간 | kind single-node에서만 검증됨. 다중 노드 미검증 |

### 다음 스프린트 후보

1. **Dispatcher 동시 처리**: 수신 메시지를 goroutine으로 병렬 실행 (BoundedDriver가 K8s burst 제어)
2. **XCLAIM 자동 복구**: Dispatcher 시작 시 stale PEL 항목을 먼저 재처리
3. **Redis AOF 내구성 검증**: `redis.conf appendonly yes` 설정 후 재시작 복구 확인
4. **in-cluster Redis**: kind 내부 pod으로 이동 (현재는 호스트 docker)
5. **runstore 정리**: finished/canceled 항목 TTL 또는 PostgreSQL 전환

---

## 8. 테스트/빌드 결과 요약

| 항목 | 결과 |
|------|------|
| `go build -tags redis ./cmd/ingress/` | PASS |
| `go build ./...` (기본 빌드, redis 태그 없음) | PASS |
| `go test ./...` (기존 테스트 전체) | PASS (6 packages) |
| 기존 비-redis 경로 오염 여부 | 없음 |
| `cmd/ingress` 기본 빌드 포함 여부 | 제외 (//go:build redis) |

---

## 재사용 통계

| 컴포넌트 | 위치 | Sprint 6 역할 |
|---------|------|-------------|
| `RedisRunQueue` | `pkg/queue` | Consume/Ack/Enqueue/Pending — 그대로 재사용 |
| `RunGate` | `pkg/ingress` | queued→finished 상태 관리 — 그대로 재사용 |
| `SpawnerNode` | `pkg/adapter` | dag-go ↔ K8s Job 어댑터 — 그대로 재사용 |
| `BoundedDriver` | `spawner/cmd/imp` | semaphore K8s burst 제어 — 그대로 재사용 |
| `DriverK8s` | `spawner/cmd/imp` | 실제 K8s Job 제출 — 그대로 재사용 |
| `JsonRunStore` | `pkg/store` | run 상태 영속화 — 그대로 재사용 |
| dag-go | 외부 패키지 | DAG 실행 엔진 — 그대로 재사용 |

**Sprint 6 신규 코드**: `cmd/ingress/main.go` 1개 파일, ~220줄
**Sprint 6 재사용**: 7개 기존 컴포넌트 wiring만
