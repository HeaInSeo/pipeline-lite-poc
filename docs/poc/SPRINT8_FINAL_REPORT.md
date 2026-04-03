# Sprint 8 Final Report
**Date**: 2026-04-03
**Verdict**: dummy submitter + sequential drain baseline confirmed

---

## 1. 수정/추가 파일 목록

| 파일 | 유형 | 내용 |
|------|------|------|
| `cmd/dummy/main.go` | **신규** | N-shot submitter (`//go:build redis`) |
| `pkg/ingress/cmd_wiring_test.go` | **수정** | `cmd/dummy` producer-only 제외 처리 추가 |
| `docs/poc/SPRINT8_FINAL_REPORT.md` | **신규** | 이 문서 |
| `docs/poc/PROGRESS_LOG.md` | **추가** | Sprint 8 항목 기록 |

**후속 정리(Sprint 8 polish)로 추가 수정된 파일**:
- `cmd/ingress/main.go` — `--max-runs N` 플래그 추가 (~6줄, 의미론 변경 없음)
- `pkg/ingress/cmd_wiring_test.go` — producerOnlyCmds 주석 WHY 보강

**기존 파일 변경 없음**:
- `pkg/queue/redis_streams.go` — `q.Enqueue()` 재사용
- `pkg/ingress/run_gate.go` — 변경 없음
- `docs/poc/PIPELINE_SPECIMEN.md` — 참조만
- `runSpecimenPipeline()` / specimen spec builders — 변경 없음

---

## 2. 왜 cmd/dummy 로 분리했는가

`cmd/ingress`는 consumer(Dispatcher)다. `cmd/dummy`는 producer(submitter)다. 역할이 다르다.

- Dispatcher는 `RunGate → dag-go → BoundedDriver → K8s Job` 경로를 포함한다.
- dummy는 `q.Enqueue()` 1개 함수만 호출한다.

같은 파일 안에 두면 `--produce` 플래그처럼 "ingress도 실행해야 submit도 된다"는 오해가 생긴다.
분리하면 두 프로세스를 별도 터미널에서 독립 실행할 수 있다.
이후 dummy app을 변경해도 Dispatcher 코드에 영향이 없다.

```
터미널 A: go run -tags redis ./cmd/ingress/ --pipeline specimen   ← consumer
터미널 B: go run -tags redis ./cmd/dummy/ -n 5                     ← producer
```

---

## 3. dummy app의 범위를 왜 이렇게 작게 잘랐는가

이번 Sprint의 목적은 "N-run submit + sequential drain baseline"이다.
결과를 기다리거나 polling하거나 scheduler를 관찰하는 기능은 이번에 필요 없다.

- `q.Enqueue()` N회 루프: 충분하다
- `-n`, `-delay`, `--pipeline` 플래그 3개: 충분하다
- 결과 polling, watch, 재시도 로직: 이번 non-goal

dummy app을 작게 유지하면 specimen 구조가 바뀌어도 dummy는 바꿀 필요가 없다.
dummy는 "무엇을 몇 번 넣는가"만 책임지고, specimen 정의는 ingress 쪽이 책임진다.

---

## 4. runID 전략

```
dummy-{YYYYMMDD-HHMMSS}-{seq:03d}
```

예시:
```
dummy-20260403-104718-001
dummy-20260403-104718-002
...
dummy-20260403-104718-005
```

- prefix `dummy-`: ingress run(예: `20260403-083318-075`)과 출처 구분
- 세션 타임스탬프(초 단위): 동일 세션 공통 부분
- 3자리 시퀀스: burst에서도 충돌 없음 (`-n 10` burst 검증 완료)

**burst 검증**: `-n 10 -delay 0` 실행 시 10개 runID 모두 unique. entryID도 각자 다름.

---

## 5. N-run Submit 결과

```
go run -tags redis ./cmd/dummy/ -n 5
```

```
[dummy] redis ready  addr=localhost:6379 stream=poc:runs
[dummy] submitting   n=5 delay=0s pipeline=specimen
[dummy] enqueued  seq=001 runID=dummy-20260403-104718-001 entryID=1775213238482-0
[dummy] enqueued  seq=002 runID=dummy-20260403-104718-002 entryID=1775213238482-1
[dummy] enqueued  seq=003 runID=dummy-20260403-104718-003 entryID=1775213238482-2
[dummy] enqueued  seq=004 runID=dummy-20260403-104718-004 entryID=1775213238482-3
[dummy] enqueued  seq=005 runID=dummy-20260403-104718-005 entryID=1775213238482-4
[dummy] done  submitted=5/5 pipeline=specimen
```

5개 모두 XADD 성공. dummy는 즉시 종료.

---

## 6. XLEN Backlog 관찰 결과

Dispatcher 없이 submit한 직후:

```bash
podman exec poc-redis redis-cli XLEN poc:runs
# 5
```

**관찰**: dummy가 종료된 시점에 5개 메시지가 Redis stream에 쌓여 있다.
Dispatcher가 없으므로 메시지는 소비되지 않고 backlog 상태를 유지한다.

이것이 현재 구조에서 관찰 가능한 "queue pressure"의 전부다:
- Redis stream XLEN 증가 → 확인됨
- Dispatcher가 연결되기 전까지 메시지는 미소비 상태로 대기

**중요**: Kueue나 K8s scheduler 관점에서는 이 단계에서 아무 일도 일어나지 않는다.
Dispatcher가 XREADGROUP을 호출해야 비로소 Kueue에 workload가 제출된다.

---

## 7. Sequential Drain 관찰 결과

Dispatcher 실행 후 5개 run의 순차 처리 타임라인:

```
19:47:18  dummy 제출 완료 (XLEN=5)
19:49:20  run-001 queued   → 19:49:38 finished  (duration: ~18s)
19:49:38  run-002 queued   → 19:49:57 finished  (duration: ~18s)
19:49:57  run-003 queued   → 19:50:15 finished  (duration: ~18s)
19:50:15  run-004 queued   → 19:50:33 finished  (duration: ~18s)
19:50:33  run-005 queued   → 19:50:51 finished  (duration: ~18s)
```

**각 run 소요 시간 ~18초**: prepare(~6s) → worker-1/2/3 병렬(~6s) → collect(~6s)

**5개 총 소요 시간**: 약 91초 (18s × 5 + Dispatcher 기동 약 1s)

**순차 처리 구조 확인**:
- run-001이 완전히 끝난 후 run-002가 시작됨
- `XPENDING count=0`이 각 run 완료 직후 출력됨 — 항상 pending 상태인 메시지가 최대 1개
- 나머지 4개 메시지는 Redis stream에 대기(undelivered)하다가 순서대로 소비됨

---

## 8. XPENDING / runstore / K8s Job 결과

### XPENDING

```bash
podman exec poc-redis redis-cli XPENDING poc:runs poc-workers - + 10
# (empty)
```

5개 run 모두 XACK 완료. Pending 없음.

각 run 처리 중에는 `XPENDING count=1` (처리 중인 1개만 pending). 다음 Consume 전에 Ack.

### runstore

```json
"dummy-20260403-104718-001": {"State":"finished", ...},
"dummy-20260403-104718-002": {"State":"finished", ...},
"dummy-20260403-104718-003": {"State":"finished", ...},
"dummy-20260403-104718-004": {"State":"finished", ...},
"dummy-20260403-104718-005": {"State":"finished", ...}
```

5개 run 전체 `state=finished`.

### K8s Jobs (5 runs × 5 jobs = 25개)

```
i-dummy-20260403-104718-001-prepare    Complete   1/1
i-dummy-20260403-104718-001-worker-1   Complete   1/1
i-dummy-20260403-104718-001-worker-2   Complete   1/1
i-dummy-20260403-104718-001-worker-3   Complete   1/1
i-dummy-20260403-104718-001-collect    Complete   1/1
(... 002~005 동일 패턴)
```

25개 모두 Complete. Job naming 충돌 없음:
- `i-dummy-{ts}-{seq:03d}-{nodeID}` 형식이 run 간 고유성을 보장함

---

## 9. 왜 이것이 scheduler pressure가 아니라 baseline인가

**현재 `dispatch()`는 동기 단일 처리다**:

```go
// dispatch 내부 루프
admitErr := gate.Admit(ctx, msg.RunID, func(ctx context.Context) error {
    return pipelineFn(ctx, msg.RunID, drv)  // ← ~18s 블로킹
})
// Admit() 반환 후에야 다음 Consume 호출
```

이 구조로 인해:

| 관찰된 것 | 관찰되지 않은 것 |
|-----------|-----------------|
| Redis stream XLEN backlog (=5) | Kueue admission queue 압력 |
| 순차 소비 (one-by-one) | concurrent pipeline 실행 |
| XPENDING 항상 ≤ 1 | K8s scheduler saturation |
| 5개 run × 18s = 91s total | admission 대기 시간 |
| at-least-once 보장 유지 | 병렬 처리 throughput |

**Kueue 관점**: run 5개가 제출되어 있어도, Dispatcher는 항상 1개씩 K8s Job을 생성한다.
따라서 Kueue에서 동시에 pending 상태인 workload는 최대 5개(specimen 1 run의 Job들)뿐이다.
"scheduler pressure"라고 부르기에는 부족하다.

**이번 Sprint 8의 정확한 위치**:
> "dummy submitter가 N개 run을 Redis에 쌓고, Dispatcher가 순차 소비할 때 아무 문제가 없음을 확인한 baseline."

이것이 이후 concurrent Dispatcher, scheduler pressure 실험의 기준점이 된다.

---

## 10. 남은 리스크 / Sprint 9로 넘길 항목

### 현재 구조적 한계

| # | 항목 | 설명 |
|---|------|------|
| 1 | Dispatcher 동기 단일 처리 | N개 run은 항상 순차 처리. 진짜 scheduler pressure 없음 |
| 2 | Kueue admission pressure 미발생 | Dispatcher가 동기이므로 workload는 1개씩 제출됨 |
| 3 | XCLAIM 자동 복구 없음 | PEL 잔류 보장만. 자동 재시도 없음 |
| 4 | Redis AOF/RDB 내구성 미검증 | 재시작 시 stream 소실 가능 |
| 5 | runstore 누적 | `/tmp/ingress-runstore.json` 실행마다 항목 추가 |

### Sprint 9 후보

1. **Dispatcher goroutine fan-out** — 수신 메시지를 goroutine으로 병렬 실행 (BoundedDriver가 K8s burst 제어). 이것이 구현되어야 비로소 "진짜 Kueue pressure"를 관찰할 수 있다.
2. **concurrent drain 관찰** — dummy -n 10 → goroutine Dispatcher → 동시 K8s Job 제출 → Kueue pending queue 증가 확인
3. **XCLAIM 자동 복구** — Dispatcher 시작 시 stale PEL 항목 자동 재처리
4. **Redis in-cluster** — 호스트 podman → kind pod 이동

---

## 11. 빌드 결과

| 항목 | 결과 |
|------|------|
| `go build -tags redis ./cmd/dummy/` | PASS |
| `go build ./...` (redis 태그 없음) | PASS |
| `go test ./...` (전체 테스트) | PASS (6 packages) |
| `cmd/dummy` wiring test 제외 | `pkg/ingress/cmd_wiring_test.go` producer-only 예외 처리 추가 |

---

## cmd/dummy 경계 요약

```
┌─────────────────────────────────────────────────────────────────┐
│ cmd/dummy (producer)                                            │
│                                                                 │
│  -n 5, -delay 0, --pipeline specimen                           │
│  runID = dummy-{ts}-{seq:03d}                                  │
│  q.Enqueue(ctx, runID) × N  →  XADD(poc:runs)  →  종료        │
└─────────────────────────────────────────────────────────────────┘
                           ↓ Redis stream (XLEN=N)
┌─────────────────────────────────────────────────────────────────┐
│ cmd/ingress (consumer / Dispatcher)                             │
│                                                                 │
│  --pipeline specimen                                           │
│  XREADGROUP → gate.Admit() → runSpecimenPipeline()            │
│    → dag-go → BoundedDriver → K8s Jobs (prepare/w-N/collect)  │
│    → finished → XACK                                           │
└─────────────────────────────────────────────────────────────────┘

dummy가 아는 것: stream key, runID 형식, pipeline 이름
dummy가 모르는 것: specimen 내부 구조, K8s Job spec, dag-go, RunGate
```

이 경계가 느슨한 결합(loose coupling)의 핵심이다.
specimen v0.2, v0.3으로 진화해도 dummy는 바꿀 필요가 없다.

---

## [후속] Dispatcher 종료 UX 개선 — --max-runs

**배경**: Sprint 8 최초 실험에서 Dispatcher는 5개 run을 모두 처리한 뒤에도
빈 stream에서 계속 대기했다. 실험 종료를 위해 `pkill`이 필요했고 exit code 144가 발생했다.

**변경**: `cmd/ingress/main.go`에 `--max-runs N` 플래그를 추가했다.

```bash
# N=5로 고정 종료 — pkill 불필요
go run -tags redis ./cmd/ingress/ --pipeline specimen --max-runs 5
# → [dispatcher] max-runs=5 reached — exiting normally
```

- `--once`(1개 처리 후 종료)와 독립적으로 동작한다
- 0이면 무제한(기존 동작 유지)
- 정상 종료(exit code 0)를 보장하므로 실험 스크립트에서 사용 가능

**diff**: flags에 1줄, dispatch() 시그니처에 1개 인자, 루프 내 카운터 + 조건 3줄. 총 ~6줄.

---

## dummy ↔ ingress 최소 계약 (v0.1)

dummy와 ingress 사이의 계약은 최소한으로 유지한다.
specimen 내부 구조가 바뀌어도 이 계약이 유지되면 dummy는 수정이 필요 없다.

| 항목 | 값 | 책임 |
|------|----|------|
| Redis stream key | `poc:runs` | 양쪽 공유 상수 |
| consumer group | `poc-workers` | ingress가 생성, dummy는 무관 |
| run_id 형식 | `dummy-{YYYYMMDD-HHMMSS}-{seq:03d}` | dummy가 생성 |
| payload 최소 shape | `{"run_id": "...", "created_at": "..."}` | `pkg/queue.RunMessage` |
| pipeline 필드 | `--pipeline specimen` (dummy 플래그, payload에 기록) | dummy가 전달, ingress가 해석 |

**역할 분리**:
- dummy는 runID를 만들어 `q.Enqueue(ctx, runID)`만 호출한다
- Dispatcher(`cmd/ingress`)가 `--pipeline` 인자를 보고 어떤 파이프라인을 실행할지 결정한다
- dummy는 specimen 내부 구조(node 이름, K8s Job spec, dag-go)를 알 필요 없다

---

## Sprint 8 Sequential Drain Baseline — 핵심 지표 (재실험 기준)

Sprint 9 이후 concurrent drain과 비교할 때 이 숫자를 기준선으로 사용한다.

| # | 지표 | Sprint 8 기준값 | 측정 방법 |
|---|------|----------------|-----------|
| 1 | **XLEN (submit 직후)** | N (제출 수와 일치) | `redis-cli XLEN poc:runs` |
| 2 | **XPENDING (drain 후)** | 0 | `redis-cli XPENDING poc:runs poc-workers - + 10` |
| 3 | **runstore finished 수** | N개 전체 finished | `cat /tmp/ingress-runstore.json` |
| 4 | **K8s Job Complete 수** | N × 5 개 | `kubectl get jobs -n default \| grep "^i-dummy"` |
| 5 | **단일 run wall-clock** | ~18s (prepare 6s + workers 6s + collect 6s) | Dispatcher 로그 queued→finished 간격 |

**재실험 명령 (N=5 기준)**:

```bash
# 1. Redis 초기화
podman exec poc-redis redis-cli DEL poc:runs

# 2. 5개 제출
go run -tags redis ./cmd/dummy/ -n 5

# 3. XLEN 확인 (Dispatcher 없이)
podman exec poc-redis redis-cli XLEN poc:runs
# → 5

# 4. sequential drain (정상 종료)
rm -f /tmp/ingress-runstore.json
go run -tags redis ./cmd/ingress/ --pipeline specimen --max-runs 5

# 5. 결과 확인
podman exec poc-redis redis-cli XPENDING poc:runs poc-workers - + 10  # → (empty)
cat /tmp/ingress-runstore.json                                          # → 5개 finished
kubectl get jobs -n default | grep "^i-dummy" | wc -l                  # → 25
```
