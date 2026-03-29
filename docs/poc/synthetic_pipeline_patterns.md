# Synthetic Pipeline Patterns — Living Document

**목적**: 실제 유전체 툴 없이 구조적 리스크를 드러내는 synthetic 패턴 카탈로그.
BoundedDriver/submit burst/collect 병목을 kind 클러스터에서 실측하여 기록한다.

**최초 작성**: 2026-03-28 (Sprint 5)
**상태**: 관찰값 갱신 중

---

## 패턴 카탈로그

### 1. wide-fanout-8

```
start → setup → B1..B8 → collect → end
```

- **목적**: 8개 워커가 동시에 K8s Job Create를 호출할 때 BoundedDriver 포화 관찰
- **수면 스케줄**: 모든 B 노드 sleep=0 (단, K8s Job 실행 고정 ~6s)
- **핵심 측정 항목**:
  - `submit_burst_duration`: B 노드들의 Start() 구간 (sem < 8이면 직렬화)
  - `b_stage_duration`: 마지막 B 노드 완료까지 (모든 B 동일하므로 ≈ 단일 노드 시간)
  - `collector_start_delay`: C 노드 진입 - 마지막 B 완료 (dag-go가 즉시 시작 → 0)
  - `critical_path`: 전체 파이프라인 벽시계 시간

**관찰값 (kind 단일 노드, idle 클러스터)**:

| sem | peak_inflight | b_stage | critical_path | 비고 |
|-----|---------------|---------|---------------|------|
| 1   | 1             | 6s      | 18s           | 동일 |
| 4   | 4 (포화)      | 6s      | 18s           | 동일 |
| 8   | 1             | 6s      | 18s           | 동일 |

**해석**:
- `inner.Start()` ≈ 50ms, `Wait()` ≈ 6000ms → 비율 1:120.
- sem=4이면 8 워커 중 4개가 즉시 진입, 나머지 4개는 첫 번째 4개가 Start()를 마친 후 진입.
  Start()가 50ms이므로 총 submit burst = 100ms.
- Submit burst 100ms는 Wait() 6000ms에 묻혀 critical_path에 영향 없음.
- sem throttle이 파이프라인 지연에 기여하려면: slow K8s API / rate limit / 10+ workers.
- sem=8에서 `peak_inflight=1`은 polling miss (50ms 폴링 윈도가 50ms burst를 빗나감).

---

### 2. two-stage-8x4

```
start → setup → B1..B8 → merge → C1..C4 → final-collect → end
```

- **목적**: 중간 집계 단계(M)가 있는 2단계 파이프라인에서 `collector_start_delay` 측정
- **수면 스케줄**: 모든 노드 sleep=0
- **핵심 측정 항목**:
  - `b_stage_duration`: B8 완료까지
  - `collector_start_delay`: D 진입 - 마지막 B 완료 (M + C stage = ~12s)
  - `critical_path`: 전체

**관찰값**:

| sem | peak_inflight | b_stage | collector_delay | critical_path |
|-----|---------------|---------|-----------------|---------------|
| 4   | 1             | 6s      | 12s             | 30s           |

**해석**:
- M 단계 6s + C1~C4 단계 6s = collector_start_delay 12s.
- B 단계와 D 단계 사이에 두 개의 K8s Job 레이어가 추가되므로 critical_path가 wide-fanout보다 12s 증가.
- 파이프라인 깊이(stage 수) × 6s ≈ critical_path (idle 클러스터 기준).

---

### 3. long-tail-8

```
start → setup → B1..B8 (B7/B8 sleep=12s) → collect → end
```

- **목적**: 스트래글러(straggler) 워커가 전체 파이프라인을 얼마나 지연시키는지 측정
- **수면 스케줄**: B1~B6 sleep=0, B7/B8 sleep=12s
- **핵심 측정 항목**:
  - `b_stage_duration`: 마지막 B 완료 (= B7/B8 기준 = 12+6 = 18s)
  - `critical_path`: 전체 (straggler 지배)
- **주의사항**: dag-go `DefaultTimeout = 30s` (기본값) 문제.
  B7/B8 총 실행 ~18s + C 노드 preflight 대기 ≈ 24s → 30s 근접 → `dag.Wait()` false 리턴.
  **`InitDagWithOptions(noPreflight())`로 DefaultTimeout=0 설정 필수.**

**관찰값**:

| sem | peak_inflight | b_stage | critical_path | dag.Wait() 결과 |
|-----|---------------|---------|---------------|-----------------|
| 4   | 4 (포화)      | 18s     | 30s           | true (fix 적용) |

**해석**:
- Straggler B7/B8가 전체 파이프라인을 18s(B_stage) → 30s(total)로 지배.
- fast workers(B1~B6, 6s)와 straggler(B7/B8, 18s)의 3× 시간 차이가 그대로 critical_path에 반영.
- DefaultTimeout 버그: fix 전에는 dag.Wait()가 30s에서 false 리턴.
  fix: `d.Config.DefaultTimeout = 0` (caller의 ctx timeout만 사용).

---

### 4. mixed-duration-8

```
start → setup → B1..B8 (각기 다른 sleep) → collect → end
```

- **목적**: 워커 간 실행 시간이 불균일할 때 B 스테이지 지속 시간 측정
- **수면 스케줄**: `[0, 1, 2, 3, 4, 5, 7, 9]` (초 단위, B1~B8 순서대로)
  - 실제 K8s 시간 = sleep + 6s (K8s Job overhead)
  - B8: 9+6 = 15s, B1: 0+6 = 6s
- **핵심 측정 항목**:
  - `b_stage_duration`: B8 완료 기준 (최장 워커)
  - `critical_path`: 전체

**관찰값**:

| sem | peak_inflight | b_stage | critical_path | 비고 |
|-----|---------------|---------|---------------|------|
| 4   | 0             | 14s     | 26s           | B8=14s (polling miss) |

**해석**:
- B 스테이지 지속 = max(B_i 실행 시간) = B8 ≈ 14s (9s sleep + ~5s overhead).
- `peak_inflight=0`은 polling miss (50ms 폴링이 모든 Start() 구간을 빗나감).
- 8개 워커의 실행 시간이 6s~15s로 분산되어 있어도 collect는 무조건 B8 이후.

---

### 5. multi-run-burst

```
5개 goroutine이 동시에 wide-fanout-8 실행 (공유 BoundedDriver)
```

- **목적**: 다중 concurrent run이 공유 드라이버 자원을 경합할 때 격리 검증
- **격리 조건**: 각 run이 독립적인 `/data/stress/{runId}/...` 경로 사용
- **핵심 측정 항목**:
  - 각 run의 개별 total 시간
  - `wall_total`: 5개 run 동시 완료까지
  - 격리 위반 여부 (교차 경로 접근)
  - peak_inflight (5 runs × 8 B workers = 40 B Jobs, sem=4 병렬)

**관찰값**:

```
=== multi-run-burst (5 runs, sem=4) ===
  run[1] ...r1  total=29s  PASS
  run[2] ...r2  total=34s  PASS
  run[3] ...r3  total=35s  PASS
  run[4] ...r4  total=29s  PASS
  run[5] ...r5  total=33s  PASS
  succeeded: 5/5  wall_total: 35s
```

| sem | runs | 5/5 PASS | wall_total | peak_inflight | 격리 |
|-----|------|----------|------------|---------------|------|
| 4   | 5    | ✓        | 35s        | 4 (포화)      | ✓    |

**해석**:
- 5개 run 동시 실행 시 총 40개 B Job이 생성되지만 sem=4로 동시 K8s API 호출 제한.
- run별 total 시간 편차(29s~35s): 공유 sem에 의한 queueing 지연.
- 격리 완전 보장: 각 runId 기반 경로 분리, RunStore 항목 별도.
- wall_total = max(run total) ≈ 35s (직렬이었다면 5 × 18s = 90s).

---

## DefaultTimeout 이슈 — 상세

**문제**: `dag-go`의 `DagConfig.DefaultTimeout = 30 * time.Second` (기본값).
이 값은 per-node preflight 대기 데드라인이다.

**영향 패턴**: long-tail-8, two-stage-8x4 (실행 시간이 30s에 근접하는 경우).

**수치 예시 (long-tail-8)**:
- B7/B8 K8s Job 실행: sleep 12s + overhead 6s = 18s
- C 노드 preflight 대기: 마지막 B 완료 대기 ≈ 24s (dag 시작 기준)
- `dag.Wait()` false 리턴 타이밍: ~30s (DefaultTimeout 만료)
- K8s Job 실제 상태: `Complete` (정상 완료됨)
- **결과**: 파이프라인이 성공했음에도 불구하고 `dag.Wait() = false` → `canceled` 상태.

**수정**:
```go
func noPreflight() daggo.DagOption {
    return func(d *daggo.Dag) {
        d.Config.DefaultTimeout = 0
    }
}
func initDag() (*daggo.Dag, error) {
    return daggo.InitDagWithOptions(noPreflight())
}
```

`DefaultTimeout = 0`으로 설정하면 per-node preflight timeout이 비활성화되고,
overall timeout은 caller의 `ctx`에만 의존한다.
현재 `cmd/stress`의 ctx timeout = 30분 (충분).

---

## 측정 항목 정의

| 항목 | 정의 |
|------|------|
| `submit_burst_duration` | B stage 첫 번째 Enter ~ 마지막 Enter |
| `b_stage_duration` | B stage 첫 번째 Enter ~ 마지막 Done |
| `collector_start_delay` | Collect 노드 Enter ~ 마지막 B Done |
| `critical_path` | 전체 pipeline wall time |
| `peak_inflight` | BoundedDriver 50ms 폴링으로 관찰한 최대 동시 Start() 수 |

**InstrumentedRunner 계측 범위**:
- Enter(nodeID, t) → `inner.RunE()` 직전: sem_wait + K8s Job Create + K8s Job execution (Wait()) 전체 포함
- Done(nodeID, t, ok) → `inner.RunE()` 직후
- 즉, `DoneAt - EnterAt` = sem 대기 + K8s Job 전체 생애주기

---

## 실험 환경 명세

- 클러스터: kind (단일 노드, `poc-control-plane`, Kubernetes v1.35.0)
- Kueue LocalQueue: `poc-standard-lq`, ClusterQueue Quota: CPU 4 / Memory 4Gi
- PVC: `poc-shared-pvc`, RWO, 1Gi
- 이미지: `busybox:1.36` (sleep + echo 워크로드)
- 드라이버: `BoundedDriver(inner=DriverK8s, sem=N)`
- K8s Job Create latency: ~50ms (idle 환경)
- K8s Job 실행 최소 overhead: ~6s (pending → running → complete)

---

## 한계 및 미측정 항목

1. **K8s API rate limit 환경 미측정**: idle 클러스터에서는 sem throttle이 파이프라인 시간에 영향 없음.
   sem=2 vs sem=8의 차이는 10+ workers + slow API 환경에서만 관측 가능.

2. **PVC RWO 단일 노드 가정**: `poc-shared-pvc`가 RWO이므로 다중 노드 환경에서
   B 워커들이 다른 노드에 스케줄되면 마운트 실패. 현재 kind 단일 노드에서만 검증됨.

3. **K8s Job 스케줄링 지터**: idle 클러스터에서도 pending→running 전환에 0.5~2s 편차 있음.
   `collector_start_delay = 0` 관찰은 dag-go 즉시 시작 확인이지 지터 측정이 아님.

4. **sem × pattern 교차 실험 미완**: wide-fanout-8에서만 sem=1/4/8 비교 수행.
   long-tail/mixed-duration은 sem=4 고정.

5. **Kueue WorkloadPending 흐름 미관찰**: quota 초과 시 pending → admit 전환은
   이번 스프린트 범위 밖.

---

## 향후 확장 계획

1. K8s rate-limit 환경에서 sem 실효성 재검증 (10+ workers, artificially slow API)
2. PVC ReadWriteMany 또는 node-affinity로 다중 노드 환경 지원
3. Kueue WorkloadPending → admit 흐름 관찰 (quota 초과 시나리오)
4. per-node retry 로직 추가 시 InstrumentedRunner retry 카운터 포함
5. `RunMetrics.Print()` → JSON export → Grafana 연동

---

*이 문서는 Sprint 5에서 시작하여 향후 실험마다 관찰값이 추가된다.*
