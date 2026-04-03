# Sprint 9 Final Report
**Date**: 2026-04-03
**Verdict**: Concurrent drain baseline confirmed — `--concurrent-runs 3`에서 wall-clock 91s → 36s

---

## 1. 수정/추가 파일 목록

| 파일 | 유형 | 내용 |
|------|------|------|
| `cmd/ingress/main.go` | **수정** | `--concurrent-runs N` 플래그, `sync` import, `dispatch()` concurrent화 |
| `docs/poc/SPRINT9_FINAL_REPORT.md` | **신규** | 이 문서 |
| `docs/poc/PROGRESS_LOG.md` | **추가** | Sprint 9 항목 기록 |

**변경 없는 파일 (의도적)**:
- `cmd/dummy/main.go` — 변경 없음
- `pkg/queue/redis_streams.go` — 변경 없음
- `pkg/ingress/run_gate.go` — 변경 없음
- `pkg/store/json_run_store.go` — 변경 없음 (Step 0에서 이미 race-safe 확인)
- `runSpecimenPipeline()` / specimen spec builders — 변경 없음
- `docs/poc/PIPELINE_SPECIMEN.md` — 변경 없음

---

## 2. 왜 Dispatcher concurrent drain 하나만 바꿨는가

Sprint 9의 변화축은 하나다: "Dispatcher가 run을 처리하는 방식".

- specimen v0.1은 기준선 유지 → 비교 가능성 보장
- cmd/dummy는 기준선 유지 → 제출 측 변수 동일
- Redis 의미론은 기준선 유지 → at-least-once 보장 유지

이 하나의 변화(sequential → concurrent)가 Sprint 8 대비 wall-clock에 어떤 영향을 주는지
순수하게 관찰하는 것이 Sprint 9의 목적이다.

---

## 3. Step 0: JsonRunStore Race Safety 확인 결과

```bash
go test -race ./...
# ok  github.com/seoyhaein/poc/pkg/store    1.035s
# 전체 PASS
```

**결과: 이미 race-safe.**

```go
// pkg/store/json_run_store.go
type JsonRunStore struct {
    mu      sync.Mutex  // ← 모든 메서드에서 Lock/Unlock
    path    string
    records map[string]RunRecord
}
```

모든 public 메서드(`Enqueue`, `Get`, `UpdateState`, `ListByState`, `Delete`)가
`s.mu.Lock()` / `defer s.mu.Unlock()` 패턴으로 보호되어 있다.

`BoundedDriver`도 buffered channel semaphore + `atomic.Int64`로 race-safe다.

**결론: 별도 mutex 추가 없이 Sprint 9 본 구현으로 진행 가능.**

---

## 4. `--concurrent-runs` 구현 방식 요약

### 핵심 설계

```
semaphore := make(chan struct{}, concurrentRuns)
var wg sync.WaitGroup

for {
    // 슬롯을 Consume 전에 획득 (in-flight 수 제한)
    select {
    case sem <- struct{}{}: // 슬롯 획득
    case <-ctx.Done():      // ctx 만료
    }

    entryID, msg := q.Consume(ctx)  // 슬롯 있을 때만 소비

    dispatched++
    wg.Add(1)
    go func(runID, eid string) {   // 인자로 명시 전달 (capture 버그 방지)
        defer wg.Done()
        defer func() { <-sem }()  // 완료 시 슬롯 반환
        gate.Admit(ctx, runID, ...) → XACK or PEL
    }(msg.RunID, entryID)
}
wg.Wait()  // 모든 goroutine 완료 대기
```

### semaphore를 Consume 앞에 두는 이유

초기 구현에서는 semaphore를 Consume 뒤에 배치했다. 이 경우 sequential 모드에서
"run-001 goroutine이 실행 중인 동안 run-002가 Consume되어 PEL에 들어가는" 현상이 발생해
XPENDING=1이 중간에 관찰됐다. Consume 전에 슬롯을 획득하면:
- sequential(N=1): run-001 goroutine이 슬롯을 반환하기 전까지 run-002 소비 없음
- concurrent(N=3): 3개 goroutine이 슬롯을 반환할 때만 다음 메시지 소비
- Sprint 8의 `XPENDING=0 per run` 패턴 유지

### 안전성 보장

| 위험 | 대응 방법 |
|------|-----------|
| loop variable capture (entryID/runID) | goroutine 인자로 명시 전달 |
| main goroutine이 먼저 종료 | `wg.Wait()` — 모든 goroutine 완료 대기 |
| goroutine 내부 에러 사라짐 | gate.Admit() 에러는 log.Printf로 반드시 출력 |
| concurrent XACK 순서 불일치 | 문제 없음. 각 entryID는 독립 XACK |
| JsonRunStore 동시 쓰기 | sync.Mutex로 이미 보호됨 (Step 0 확인) |

---

## 5. `--concurrent-runs 1` Regression 결과

```
go run -tags redis ./cmd/ingress/ --pipeline specimen --concurrent-runs 1 --max-runs 3

[dispatcher] starting  concurrent-runs=1
[ingress] run run-001 → queued → running
[ingress] run run-001 → finished
[dispatcher] XACK ok  runID=run-001
[dispatcher] XPENDING count=0          ← run-001 완료 후 즉시 0 (Sprint 8 동일)
[ingress] run run-002 → queued → running
[ingress] run run-002 → finished
[dispatcher] XACK ok  runID=run-002
[dispatcher] XPENDING count=0          ← 동일 패턴
...
[dispatcher] max-runs=3 reached — exiting normally
```

**판정: PASS** — Sprint 8 sequential 패턴과 동일. 각 run 완료 후 XPENDING=0 확인.

---

## 6. `--concurrent-runs 3` Baseline 실험 결과

```
go run -tags redis ./cmd/dummy/ -n 5
go run -tags redis ./cmd/ingress/ --pipeline specimen --concurrent-runs 3 --max-runs 5
```

**실행 타임라인:**

```
21:16:48  run-001 → queued → running  ┐
21:16:48  run-002 → queued → running  ├ 3개 동시 시작
21:16:48  run-003 → queued → running  ┘

21:17:06  run-002 → finished  ┐ XACK → 슬롯 반환 → run-004 즉시 시작
21:17:06  run-004 → queued → running
21:17:06  run-003 → finished  ┐ XACK → 슬롯 반환 → run-005 즉시 시작
21:17:06  run-005 → queued → running
21:17:07  run-001 → finished  (18s 경과)

21:17:24  run-004 → finished
21:17:24  run-005 → finished
[dispatcher] XPENDING count=0
[dispatcher] max-runs=5 reached — exiting normally
```

**wall-clock: 21:16:48 → 21:17:24 = 36초**

**동시 running 증거**: `21:16:48`에 서로 다른 runID의 `→ running` 로그가 3개 동시 출력.

**XPENDING 최대값: 3** (3개 concurrent run이 모두 PEL에 있던 순간)
**XPENDING 최종: 0** (모든 run XACK 완료)

---

## 7. wall-clock / XPENDING / runstore / K8s Job 비교

### Sprint 8 vs Sprint 9 비교 표

| 지표 | Sprint 8 (sequential, N=1) | Sprint 9 (concurrent, N=3) | 비고 |
|------|---------------------------|---------------------------|------|
| **wall-clock (5 runs)** | ~91초 | **36초** | -60%, ceil(5/3)×18s 이론값 일치 |
| **XPENDING 최대** | 1 | 3 | concurrent 수와 일치 |
| **XPENDING 최종** | 0 | 0 | 동일 (at-least-once 유지) |
| **runstore finished** | 5 | 5 | 동일 |
| **K8s Jobs Complete** | 25 | 25 | 동일, naming collision 없음 |
| **단일 run wall-clock** | ~18s | ~18s | specimen 자체는 변화 없음 |
| **동시 running 수** | 1 | 최대 3 | semaphore(N=3) 효과 |
| **XACK 순서** | 제출 순 | 완료 순 (run-002가 run-001보다 먼저 Ack) | 정상 동작 |
| **Ack 의미론** | 성공→XACK, 실패→PEL | 동일 | 변화 없음 |

### K8s Jobs 결과

```
kubectl get jobs -n default | grep "^i-dummy-20260403-121645" | wc -l
# 25

kubectl get jobs -n default | grep "^i-dummy-20260403-121645" | grep -v "Complete" | wc -l
# 0
```

25개 모두 Complete. naming collision 없음.

---

## 8. Ack/상태 의미론 유지 여부

**Sprint 8 의미론과 동일하게 유지됨.**

| 의미론 항목 | Sprint 8 | Sprint 9 | 변화 |
|------------|----------|----------|------|
| 성공 → XACK | ✓ | ✓ | 없음 |
| 실패 → PEL 잔류 | ✓ | ✓ | 없음 |
| XPENDING final=0 | ✓ | ✓ | 없음 |
| at-least-once | ✓ | ✓ | 없음 |
| runstore state=finished | ✓ | ✓ | 없음 |

**주의 사항 — 로그 해석**:

concurrent 모드에서는 여러 goroutine이 동시에 XPENDING을 출력한다.
같은 시점에 2개 goroutine이 XPENDING 쿼리를 하면 같은 값이 두 번 출력될 수 있다.
`XPENDING count=2`가 두 줄 연속 나타나도 이상이 아니다.
최종적으로 XPENDING=0이면 모든 run이 ACK된 것이다.

---

## 9. 아직 못 본 것 / Sprint 10 이후로 넘길 것

### 새로 관찰된 것

| 항목 | 관찰 내용 |
|------|-----------|
| concurrent drain 속도 | sequential 91s → 36s (N=3 기준) |
| Kueue admission | 3 runs × 5 Jobs = 15 Jobs가 동시 제출됨. admission 지연 없이 처리됨 |
| BoundedDriver + concurrent 상호작용 | sem=4 아래서 15개 Jobs가 4개씩 K8s API에 제출됨. 문제 없음 |

### 아직 못 본 것

| 항목 | 이유 |
|------|------|
| Kueue admission saturation | poc-standard-cq(cpu=4)에서 N=3 run이 사용하는 cpu(3×100m=300m)가 한도(4000m) 대비 낮음 |
| K8s scheduler 포화 | kind single-node, idle cluster. 실질적 경쟁 없음 |
| concurrent 실패 시 PEL 동작 | --fail + --concurrent-runs 조합 미검증 (다음 스프린트 후보) |
| N=10 이상의 burst | 현재는 N=3만 검증 |

### Sprint 10 후보

1. **concurrent + --fail 조합 검증** — concurrent 실패 시 PEL 잔류가 올바르게 동작하는지
2. **N을 높여 Kueue 한계 관찰** — `--concurrent-runs 10` + `cmd/dummy -n 20`으로 admission 대기 발생 여부 확인
3. **XCLAIM 자동 복구** — stale PEL 항목 재처리
4. **in-cluster Redis** — 호스트 podman → kind pod 이동
5. **runstore 정리** — finished/canceled 항목 누적 관리

---

## 10. 빌드/테스트 결과

| 항목 | 결과 |
|------|------|
| `go build -tags redis ./cmd/ingress/` | PASS |
| `go build ./...` (redis 태그 없음) | PASS |
| `go test -race ./...` (전체, race detector 포함) | PASS (6 packages) |
| `--concurrent-runs 1` regression | PASS (Sprint 8 sequential 동일) |
| `--concurrent-runs 3 --max-runs 5` baseline | PASS (wall-clock 36s, XPENDING=0, 25 Jobs Complete) |
