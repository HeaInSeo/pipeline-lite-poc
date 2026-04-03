# Sprint 7 Final Report
**Date**: 2026-04-03
**Verdict**: Canonical specimen pipeline v0.1 baseline confirmed — end-to-end complete

---

## 1. 수정/추가 파일 목록

| 파일 | 유형 | 내용 |
|------|------|------|
| `cmd/ingress/main.go` | **수정** | `--pipeline` 플래그 추가, `runSpecimenPipeline()` + specimen spec builders 추가, `dispatch()` 라우팅 추가 |
| `docs/poc/PIPELINE_SPECIMEN.md` | **신규** | canonical specimen v0.1 공식 문서 (Sprint 7 핵심 산출물) |
| `docs/poc/SPRINT7_FINAL_REPORT.md` | **신규** | 이 문서 |
| `docs/poc/PROGRESS_LOG.md` | **추가** | Sprint 7 항목 기록 |

**기존 파일 변경 없음**:
- `pkg/queue/redis_streams.go` — 재사용 (수정 없음)
- `pkg/ingress/run_gate.go` — 재사용 (수정 없음)
- `pkg/adapter/spawner_node.go` — 재사용 (수정 없음)
- `pkg/store/json_run_store.go` — 재사용 (수정 없음)
- Sprint 6의 `runWideFanout3()` / `ingressSpec*` 함수 — 수정 없음 (regression baseline 유지)

---

## 2. 왜 Sprint 7을 이렇게 정의했는가

Sprint 6에서 Redis Streams ingress end-to-end 경로는 닫혔다.
그러나 Sprint 6의 파이프라인(`wideFanout3`)은 "ingress 연결 검증"을 위한 임시 이름 체계를 사용했다:
- `setup`, `b1`, `b2`, `b3`, `collect`

Sprint 8 이후 dummy app이 반복 제출할 workload의 이름이 `b1`, `b2`이어서는 안 된다.
Sprint 7의 목적은 이 경로 위에 **canonical specimen baseline**을 이름 붙이고 공식화하는 것이다:
- 새로운 DAG topology 발명 없음
- 새로운 의미론 도입 없음
- 기존 fan-out shape을 specimen 기준으로 이름 정리 후 1회 완주로 고정

---

## 3. Sprint 6 대비 무엇이 같고 무엇이 다른가

### 같은 것
- DAG shape: `start → [single] → [3 parallel] → [single] → end`
- 핵심 경로: Redis ingress → gate.Admit() → dag-go → BoundedDriver → K8s Job → XACK
- Ack 정책: at-least-once (성공 후에만 XACK)
- 재사용 컴포넌트: `RedisRunQueue`, `RunGate`, `SpawnerNode`, `BoundedDriver`, `DriverK8s`, `JsonRunStore`, `dag-go`
- K8s Job spec: `busybox:1.36`, `poc-standard-lq`, cpu=100m/mem=64Mi

### 다른 것

| 항목 | Sprint 6 (fanout) | Sprint 7 (specimen) |
|------|-------------------|---------------------|
| 선행 노드 이름 | `setup` | `prepare` |
| 병렬 노드 이름 | `b1`, `b2`, `b3` | `worker-1`, `worker-2`, `worker-3` |
| 수렴 노드 이름 | `collect` | `collect` (동일) |
| 출력 디렉토리 | `b-output/worker-N/` | `worker-N-output/` |
| 플래그 | 없음 (고정) | `--pipeline fanout` \| `--pipeline specimen` |
| 문서 | SPRINT6_FINAL_REPORT.md | PIPELINE_SPECIMEN.md (공식 specimen 문서) |
| 목적 | ingress 연결 검증 | canonical baseline 공식화 |

---

## 4. `--pipeline fanout` Regression 결과

```
go run -tags redis ./cmd/ingress/ --produce --once --pipeline fanout

[ingress] dispatcher starting  once=true fail=false pipeline=fanout
[dispatcher] received  runID=20260403-083256-145 entryID=1775205176145-0
[ingress] run 20260403-083256-145 → queued
[ingress] run 20260403-083256-145 → admitted-to-dag
[ingress] run 20260403-083256-145 → running
Preflight setup / InFlight setup / PostFlight setup
Preflight b1/b2/b3 (병렬) / InFlight b1/b2/b3 / PostFlight b1/b2/b3
Preflight collect / InFlight collect / PostFlight collect
[ingress] run 20260403-083256-145 → finished
[dispatcher] XACK ok  entryID=1775205176145-0 runID=20260403-083256-145
[dispatcher] XPENDING count=0
```

**판정: PASS** — Sprint 6 happy path와 동일하게 완주. Regression 없음.

---

## 5. `--pipeline specimen` 실행 결과

```
go run -tags redis ./cmd/ingress/ --produce --once --pipeline specimen

[ingress] dispatcher starting  once=true fail=false pipeline=specimen
[dispatcher] received  runID=20260403-083318-075 entryID=1775205198076-0
[ingress] run 20260403-083318-075 → queued
[ingress] run 20260403-083318-075 → admitted-to-dag
[ingress] run 20260403-083318-075 → running
Preflight prepare / InFlight prepare / PostFlight prepare
Preflight worker-1/worker-2/worker-3 (병렬) / InFlight worker-3/worker-1/worker-2 / PostFlight (완료)
Preflight collect / InFlight collect / PostFlight collect
[ingress] run 20260403-083318-075 → finished
[dispatcher] XACK ok  entryID=1775205198076-0 runID=20260403-083318-075
[dispatcher] XPENDING count=0
```

**판정: PASS** — specimen pipeline 단일 run 완주 확인.

---

## 6. K8s Job 완료 증거

```bash
kubectl get jobs -n default | grep "^i-"
```

```
# fanout run (20260403-083256-145)
i-20260403-083256-145-setup      Complete   1/1   4s   98s
i-20260403-083256-145-b1         Complete   1/1   4s   92s
i-20260403-083256-145-b2         Complete   1/1   4s   92s
i-20260403-083256-145-b3         Complete   1/1   4s   92s
i-20260403-083256-145-collect    Complete   1/1   4s   86s

# specimen run (20260403-083318-075)
i-20260403-083318-075-prepare    Complete   1/1   4s   76s
i-20260403-083318-075-worker-1   Complete   1/1   4s   70s
i-20260403-083318-075-worker-2   Complete   1/1   4s   70s
i-20260403-083318-075-worker-3   Complete   1/1   4s   70s
i-20260403-083318-075-collect    Complete   1/1   4s   64s
```

specimen: `prepare` / `worker-1` / `worker-2` / `worker-3` / `collect` 5개 모두 Complete.

---

## 7. XPENDING / XACK / runstore 결과

### XPENDING (정상 경로)

```bash
podman exec poc-redis redis-cli XPENDING poc:runs poc-workers - + 10
# (empty) ← 두 run 모두 XACK 완료
```

### runstore

```json
{
  "20260403-083256-145": {"RunID":"20260403-083256-145","State":"finished",...},
  "20260403-083318-075": {"RunID":"20260403-083318-075","State":"finished",...}
}
```

두 run 모두 `state=finished`.

### `--fail` regression (specimen 경로)

```
go run -tags redis ./cmd/ingress/ --produce --once --pipeline specimen --fail

[ingress] run 20260403-083439-659 → canceled: synthetic failure (--fail flag): PEL retention test
[dispatcher] run FAILED  runID=20260403-083439-659 — NOT acking (PEL retains)
[dispatcher] XPENDING count=1
```

XACK 미호출, `XPENDING count=1` 확인. **Regression 없음**.

---

## 8. PIPELINE_SPECIMEN.md 요약

`docs/poc/PIPELINE_SPECIMEN.md`에 포함된 내용:

1. specimen v0.1의 목적 — dummy app canonical workload baseline
2. shape — `prepare → worker-1/2/3 → collect` 다이어그램
3. 각 노드의 역할 — prepare/worker/collect 역할 분리, content dependency 없음 명시
4. naming 규칙 — 노드 ID, K8s Job 이름, 출력 디렉토리 패턴
5. 실행 방법 — `--pipeline specimen` 예시, 분리 방식, `--fail` 검증
6. acceptance criteria 표
7. non-goals 목록 — content dependency, workers N, pkg 추출 등 명시적 제외
8. dummy app 연결 맥락 — Sprint 8+ dummy app이 이 specimen을 반복 제출

---

## 9. 왜 이것이 dummy app 이전 기준선으로 충분한가

| 기준 | 이유 |
|------|------|
| **이름이 의미 있다** | `b1/b2/b3`가 아닌 `prepare/worker-N/collect` — run의 의도를 읽을 수 있다 |
| **고정된 shape** | fan-out/fan-in, workers=3 고정 — dummy app이 "표준 1건"을 명확히 정의할 수 있다 |
| **검증된 경로** | Sprint 6 동일 경로 위 — 새로운 리스크 없음 |
| **재현 가능** | `--produce --once --pipeline specimen` 1줄로 재현 가능 |
| **failure path 확인** | `--fail` + PEL 잔류 확인 — at-least-once 보장 유지 |
| **문서화 완료** | PIPELINE_SPECIMEN.md에 shape/role/naming/non-goals 기록 — 향후 버전 업데이트 기준 확보 |

---

## 10. 남은 리스크 / Sprint 8로 넘길 항목

### 남은 리스크

| # | 리스크 | 수준 | 비고 |
|---|--------|------|------|
| 1 | Dispatcher 동기 단일 처리 | 낮음 | 1회 run 후 다음 메시지. Sprint 7 non-goal |
| 2 | XCLAIM 자동 재시도 없음 | 낮음 | PEL 잔류 보장. 수동 XCLAIM 가능 |
| 3 | Redis 인프라 내구성 미검증 | 낮음 | AOF/RDB 없이 실행 중. 재시작 시 stream 소실 가능 |
| 4 | PVC RWO 단일 노드 가정 | 중간 | kind single-node에서만 검증 |
| 5 | runstore 누적 | 낮음 | `/tmp/ingress-runstore.json` 실행마다 추가 |
| 6 | specimen shape 고정 (workers=3) | 낮음 | v0.1 설계 의도. 파라미터화 필요 시 명시 |

### Sprint 8 후보

1. **dummy app** — specimen을 반복 제출하는 client loop
2. **Dispatcher 동시 처리** — goroutine fan-out (BoundedDriver가 K8s burst 제어)
3. **XCLAIM 자동 복구** — stale PEL 항목 자동 재처리
4. **Redis AOF 내구성** — `appendonly yes` + 재시작 복구 검증
5. **in-cluster Redis** — 호스트 podman → kind pod 이동

---

## 11. 빌드/테스트 결과

| 항목 | 결과 |
|------|------|
| `go build -tags redis ./cmd/ingress/` | PASS |
| `go build ./...` (redis 태그 없음, 기존 경로 오염 없음) | PASS |
| `go test ./...` (전체 테스트) | PASS (6 packages) |
| `--pipeline fanout` regression | PASS |
| `--pipeline specimen` end-to-end | PASS (5 K8s Jobs Complete, XPENDING=0, state=finished) |
| `--pipeline specimen --fail` regression | PASS (XPENDING=1, XACK 미호출) |

---

## 신규 코드 통계

| 항목 | Sprint 7 추가 |
|------|---------------|
| `cmd/ingress/main.go` 추가 코드 | ~120줄 (`runSpecimenPipeline` + 3 spec builders + dispatch 라우팅) |
| 신규 파일 | `docs/poc/PIPELINE_SPECIMEN.md`, `docs/poc/SPRINT7_FINAL_REPORT.md` |
| 수정된 기존 코드 | `dispatch()` 시그니처 + pipeline 라우팅 (~10줄) |
| 기존 경로 변경 | 없음 |
