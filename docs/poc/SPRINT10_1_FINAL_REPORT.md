# Sprint 10-1 Final Report
**Date**: 2026-04-03
**Verdict**: Kueue admission pressure confirmed — cpu quota 500m에서 wall-clock 36s → 43s (+19%), 개별 Job admission 최대 6s 대기 관찰

---

## 1. 수정/추가 파일 목록

| 파일 | 유형 | 내용 |
|------|------|------|
| `deploy/kueue/queues.yaml` | **임시 수정 후 롤백** | poc-standard-cq cpu: "4" → "500m" (실험), 실험 완료 후 "4" 복원 |
| `docs/poc/SPRINT10_1_FINAL_REPORT.md` | **신규** | 이 문서 |
| `docs/poc/PROGRESS_LOG.md` | **추가** | Sprint 10-1 항목 기록 |

**변경 없는 파일 (의도적)**:
- `cmd/ingress/main.go` — 코드 변경 없음
- `cmd/dummy/main.go` — 코드 변경 없음
- `pkg/queue/redis_streams.go` — 변경 없음
- `pkg/ingress/run_gate.go` — 변경 없음
- `pkg/store/json_run_store.go` — 변경 없음

---

## 2. Sprint 10-1 목적

단일 변수 변경 실험: **poc-standard-cq CPU quota를 4000m → 500m**으로 줄여
Kueue admission 대기가 발생하는 "small-pressure baseline"을 최초로 확보한다.

Sprint 9와 동일 조건(코드, 파이프라인, --concurrent-runs 3, -n 5)을 유지하여,
quota 감소가 wall-clock에 미치는 영향을 순수하게 관찰한다.

---

## 3. 실험 조건

| 항목 | 값 |
|------|-----|
| poc-standard-cq cpu quota | **500m** (Sprint 9: 4000m) |
| `--concurrent-runs` | 3 (동일) |
| `cmd/dummy -n` | 5 (동일) |
| `--pipeline` | specimen (동일) |
| `--max-runs` | 5 (동일) |
| 단일 Job CPU 요청 | 100m (변경 없음) |
| worker 동시 실행 수 (per run) | 3개 (변경 없음) |

**quota 압박 산술**:
- 3 concurrent runs × worker 단계 3개 × 100m = **900m 동시 요청**
- quota 500m → 최대 5 Jobs 동시 admitted (5 × 100m)
- 나머지 4 Jobs 이상 대기 (Kueue admission 큐)

---

## 4. 실험 결과

### 실행 타임라인

```
22:05:11  run-001 → queued → admitted-to-dag → running  ┐
22:05:11  run-002 → queued → admitted-to-dag → running  ├ 3개 동시 시작
22:05:11  run-003 → queued → admitted-to-dag → running  ┘

22:05:29  run-002 → finished (18s)  → XACK → 슬롯 반환 → run-004 즉시 시작
22:05:29  run-004 → queued → admitted-to-dag → running
22:05:35  run-003 → finished (24s, +6s vs Sprint 9)
22:05:35  run-005 → queued → admitted-to-dag → running
22:05:36  run-001 → finished (25s, +7s vs Sprint 9)

22:05:47  run-004 → finished (18s from start)
22:05:54  run-005 → finished
[dispatcher] XPENDING count=0
[dispatcher] max-runs=5 reached — exiting normally
```

**wall-clock: 22:05:11 → 22:05:54 = 43초**

---

## 5. Kueue Admission 압박 관찰 증거

### Workload -w 출력에서 관찰된 admission 대기

```
# 즉시 admitted (0s)
job-i-dummy-20260403-130404-002-prepare-c290f    poc-standard-lq                      0s  ← 등장 직후
job-i-dummy-20260403-130404-002-prepare-c290f    poc-standard-lq   poc-standard-cq   True  0s  ← 즉시 admitted

# 지연된 admission (5~6s)
job-i-dummy-20260403-130404-001-worker-1-034c3   poc-standard-lq                      0s  ← pending
job-i-dummy-20260403-130404-001-worker-1-034c3   poc-standard-lq                      0s  ← still pending
job-i-dummy-20260403-130404-001-worker-1-034c3   poc-standard-lq   poc-standard-cq   True  5s  ← admitted (5s 대기)

job-i-dummy-20260403-130404-003-worker-1-d4569   poc-standard-lq                      0s
job-i-dummy-20260403-130404-003-worker-1-d4569   poc-standard-lq                      0s
job-i-dummy-20260403-130404-003-worker-1-d4569   poc-standard-lq   poc-standard-cq   True  6s  ← admitted (6s 대기)
```

**Kueue admission 대기 패턴**:
- prepare 노드(1개 × 100m): quota 여유 있을 때 즉시 admitted
- worker 노드 일부(3 runs × 3 workers = 9 Jobs, 500m quota): 4~6개는 즉시 admitted, 나머지는 대기
- 대기 중 workload는 `RESERVED IN`/`ADMITTED` 컬럼이 비어 있음 (= ADMITTED pending 상태)

---

## 6. Sprint 9 vs Sprint 10-1 비교 표

| 지표 | Sprint 9 (500m→4000m, N=3) | Sprint 10-1 (quota=500m, N=3) | 비고 |
|------|---------------------------|-------------------------------|------|
| **poc-standard-cq cpu quota** | 4000m | **500m** | 유일한 변경 |
| **wall-clock (5 runs)** | 36초 | **43초** | +7s (+19%) |
| **단일 run 최단** | ~18s (run-002) | ~18s (run-002) | quota 여유 구간 |
| **단일 run 최장** | ~18s (모두 동일) | **~25s (run-001)** | admission 대기 포함 |
| **Job admission 대기 최대** | 0s (즉시) | **~6s** | quota 경합 |
| **XPENDING 최대** | 3 | 3 | concurrent 수 동일 |
| **XPENDING 최종** | 0 | 0 | at-least-once 유지 |
| **runstore finished** | 5 | 5 | 동일 |
| **K8s Jobs Complete** | 25 | 25 | naming collision 없음 |
| **Kueue admission 거부** | 없음 | 없음 (대기만 발생) | 거부 ≠ 대기 |
| **at-least-once 유지** | ✓ | ✓ | 변화 없음 |

---

## 7. Ack/상태 의미론 유지 여부

Sprint 9와 동일하게 유지됨.

| 항목 | Sprint 10-1 | 변화 |
|------|------------|------|
| 성공 → XACK | ✓ | 없음 |
| 실패 → PEL 잔류 | ✓ | 없음 |
| XPENDING final=0 | ✓ | 없음 |
| at-least-once | ✓ | 없음 |
| runstore State=finished | ✓ (5/5) | 없음 |

---

## 8. Kueue Admission 압박 메커니즘 해석

### 왜 일부 노드만 지연됐는가

specimen v0.1의 DAG 구조: `prepare → worker-1/worker-2/worker-3 → collect`

- **prepare**: 한 번에 하나씩 실행. 100m × 3 runs = 300m → 500m 미만 → 즉시 admitted
- **worker 단계**: 3 runs × 3 workers = 9 Jobs 동시 제출. 9 × 100m = 900m > 500m
  - Kueue: 5개 먼저 admitted (500m), 4개 대기
  - 앞 Jobs 완료 → quota 반환 → 대기 중 Jobs admitted
- **collect**: worker 완료 후 1개씩 제출. quota 여유 있음 → 즉시 admitted

### 왜 run-001/003이 더 오래 걸렸는가

3개 concurrent run이 모두 worker 단계에 진입하면 9개 Jobs가 동시 제출된다.
500m quota에서 5개만 admitted되고, run-001/003의 일부 worker-1 Jobs가 대기 큐에 들어갔다.
→ worker 완료 대기 → collect 시작 지연 → 총 run 시간 증가

run-002는 운이 좋아 모든 worker가 즉시 admitted → 18s로 완료.

---

## 9. quota rollback

실험 완료 후 `deploy/kueue/queues.yaml`의 poc-standard-cq cpu quota를 **"4" (4000m)** 으로 복원했다.

```bash
kubectl apply -f deploy/kueue/queues.yaml
# clusterqueue.kueue.x-k8s.io/poc-standard-cq configured
kubectl get clusterqueue poc-standard-cq -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[0].nominalQuota}'
# 4
```

현재 `deploy/kueue/queues.yaml`은 Sprint 9 기준(cpu="4")으로 복원된 상태다.

---

## 10. 아직 못 본 것 / Sprint 10-2 이후 후보

### 이번에 새로 관찰된 것

| 항목 | 관찰 내용 |
|------|-----------|
| Kueue admission 대기 | 500m quota에서 최대 6s Job admission 대기 확인 |
| wall-clock 증가 | quota 압박으로 36s → 43s (+19%) |
| per-node 지연 편차 | run-002: 18s (대기 없음), run-001: 25s (대기 있음) |

### 아직 못 본 것

| 항목 | 이유 |
|------|------|
| ADMITTED=False 명시적 표시 | `kubectl get workloads` 출력에서 빈 ADMITTED 컬럼으로 대기 상태 관찰됨. 단, 별도 `ADMITTED=False` 로그 컬럼은 watcher 포맷 한계 |
| quota saturation (완전 차단) | 500m에서도 대기 후 최종 admitted됨. 완전 거부는 발생 안 함 |
| concurrent + --fail 조합 | PEL 잔류 concurrent 동작 미검증 |
| N=10 이상의 burst | 현재는 N=3만 검증 |
| XCLAIM 자동 복구 | stale PEL 항목 재처리 미구현 |

### Sprint 10-2 후보

1. **quota 더 낮춰 saturation 관찰** — cpu="200m"에서 완전 admission 대기 발생 여부
2. **concurrent + --fail 조합** — PEL 잔류가 concurrent 모드에서 올바르게 동작하는지
3. **N=10 burst** — `--concurrent-runs 10` + `cmd/dummy -n 20`으로 Kueue 대기 큐 깊이 관찰
4. **XCLAIM 자동 복구** — stale PEL 항목 재처리

---

## 11. 빌드/테스트 결과

| 항목 | 결과 |
|------|------|
| 코드 변경 없음 | 빌드/테스트 Sprint 9 결과 그대로 유지 |
| quota 수정 → apply | PASS (poc-standard-cq configured) |
| `--concurrent-runs 3 --max-runs 5` 실행 | PASS (wall-clock 43s, XPENDING=0, 25 Jobs Complete) |
| quota rollback → apply | PASS (poc-standard-cq configured, nominalQuota=4) |
| runstore 5/5 finished | PASS |
