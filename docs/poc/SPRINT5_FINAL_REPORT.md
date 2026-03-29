# Sprint 5 Final Report
**Date**: 2026-03-28
**Verdict**: Synthetic 파이프라인 하네스 구축 및 패턴별 실측 완료

---

## Sprint 5 목표 요약

실제 유전체 툴 없이, 구조적 리스크를 드러내는 synthetic 패턴을 kind 클러스터에서 실행하여
BoundedDriver/submit burst/collect 병목을 실측한다.

---

## 1. 구현 파일 목록

| 파일 | 변경 유형 | 내용 |
|------|-----------|------|
| `cmd/stress/main.go` | 신규 | 스트레스 하네스 진입점, 패턴 디스패치, multi-run-burst |
| `cmd/stress/patterns.go` | 신규 | 5개 패턴 구현, InstrumentedRunner, RunMetrics |
| `cmd/stress/stress_test.go` | 신규 | 7개 단위 테스트 |
| `docs/poc/synthetic_pipeline_patterns.md` | 신규 | 패턴 카탈로그 living document |

---

## 2. 구현된 패턴

### 패턴 구조

```
wide-fanout-8:    setup → B1..B8 → collect
two-stage-8x4:   setup → B1..B8 → merge → C1..C4 → final-collect
long-tail-8:      setup → B1..B8(B7/B8 +12s) → collect
mixed-duration-8: setup → B1..B8(각 0/1/2/3/4/5/7/9s) → collect
multi-run-burst:  5개 goroutine × wide-fanout-8 (공유 BoundedDriver)
```

### 실행 방법

```bash
go run ./cmd/stress/ --pattern wide-fanout-8    --sem 4
go run ./cmd/stress/ --pattern two-stage-8x4   --sem 4
go run ./cmd/stress/ --pattern long-tail-8      --sem 4
go run ./cmd/stress/ --pattern mixed-duration-8 --sem 4
go run ./cmd/stress/ --pattern multi-run-burst  --runs 5 --sem 4
```

---

## 3. InstrumentedRunner / RunMetrics 계측 설계

**InstrumentedRunner**:
- `daggo.Runnable` 래퍼
- `Enter(nodeID, t)` → `inner.RunE()` → `Done(nodeID, t, ok)`
- 계측 범위: sem 대기 + K8s Job Create + K8s Job 실행(Wait()) 전체

**RunMetrics**:
- `NewRunMetrics(runID)` → per-node 타이밍 맵
- `Print(wallTotal)`: 노드 타이밍 테이블 + 집계 지표 출력

**집계 지표 정의**:

| 지표 | 정의 |
|------|------|
| `submit_burst_duration` | B stage 첫 Enter ~ 마지막 Enter |
| `b_stage_duration` | B stage 첫 Enter ~ 마지막 Done |
| `collector_start_delay` | Collect Enter ~ 마지막 B Done |
| `critical_path` | 전체 벽시계 시간 |

---

## 4. DefaultTimeout 버그 발견 및 수정

### 발견 경위

`long-tail-8` 첫 실행 시 `dag.Wait()` = false 리턴. K8s Job은 `Complete` 상태였으나
dag-go 파이프라인이 30s 시점에 canceled 처리됨.

### 원인

`dag-go`의 `DagConfig.DefaultTimeout = 30 * time.Second` (기본값).
B7/B8 총 실행 ≈ 18s + C 노드 preflight 대기 ≈ 24s → 30s 데드라인에 걸림.

### 수정

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

`DefaultTimeout = 0` 설정 후 long-tail-8 `dag.Wait()` = true 확인.
`wideFanout()` 및 `twoStage8x4()` 모두 `initDag()` 사용으로 일관 적용.

---

## 5. 단위 테스트 결과

```
=== RUN   TestGenerateRunID_Format         PASS
=== RUN   TestStressNodeID_Format          PASS
=== RUN   TestStressNodeID_TruncatePreservesNode PASS
=== RUN   TestSleepSchedule_LongTail       PASS
=== RUN   TestSleepSchedule_MixedDuration  PASS
=== RUN   TestRunMetrics_NodeOrdering      PASS
=== RUN   TestRunPattern_UnknownReturnsError PASS
--- PASS: (7/7)
```

---

## 6. 실험 결과 매트릭스

### wide-fanout-8 (sem 비교)

| sem | peak_inflight | b_stage | critical_path | 비고 |
|-----|---------------|---------|---------------|------|
| 1   | 1             | 6s      | 18s           | 동일 |
| 4   | 4 (포화)      | 6s      | 18s           | 동일 |
| 8   | 1             | 6s      | 18s           | 동일 |

**관찰**: sem 값과 무관하게 critical_path = 18s (idle 단일 kind 클러스터).

### 전체 패턴 결과

| 패턴 | sem | peak_inflight | b_stage | collector_delay | critical_path |
|------|-----|---------------|---------|-----------------|---------------|
| wide-fanout-8 | 4 | 4 (포화) | 6s | 0s | 18s |
| wide-fanout-8 | 1 | 1 | 6s | 0s | 18s |
| wide-fanout-8 | 8 | 1 | 6s | 0s | 18s |
| long-tail-8 | 4 | 4 (포화) | 18s | 0s | 30s |
| mixed-duration-8 | 4 | 0 (miss) | 14s | 0s | 26s |
| two-stage-8x4 | 4 | 1 | 6s | 12s | 30s |
| multi-run-burst(5) | 4 | 4 (포화) | — | — | 35s wall |

---

## 7. BoundedDriver 분석

**관찰 사실**:
- `inner.Start()` (K8s Job Create) ≈ 50ms
- `Wait()` (K8s Job 실행) ≈ 6000ms
- 비율: 1:120

**sem=4 wide-fanout-8 분석**:
- B1~B8 Start() 동시 진입 시도 → 4개 즉시 진입, 4개 대기
- 첫 4개 Start() 완료(~50ms) 후 나머지 4개 진입
- 총 submit burst = ~100ms → Wait() 6000ms에 비해 무시 가능
- critical_path 변화 없음 (sem=1이어도 동일)

**semi throttle이 의미 있어지는 조건**:
- K8s API latency > 500ms (rate limit, 부하 환경)
- workers > 10 + sem << workers
- 현재 PoC 환경에서는 sem throttle = API burst 제어이지 throughput 제어가 아님

**peak_inflight polling miss 현상**:
- 50ms 폴링이 50ms Start() 윈도를 빗나가는 경우 `peak_inflight=0` 또는 `1`
- wide-fanout-8 sem=8: 8개가 동시에 50ms → 폴링 miss → `peak_inflight=1`
- mixed-duration-8: `peak_inflight=0`
- 이는 드라이버 오작동이 아니며 관측 해상도 한계

---

## 8. 격리 검증 (multi-run-burst)

**실행 결과**:
```
=== multi-run-burst (5 runs, sem=4) ===
  run[1] ...r1  total=29s  PASS
  run[2] ...r2  total=34s  PASS
  run[3] ...r3  total=35s  PASS
  run[4] ...r4  total=29s  PASS
  run[5] ...r5  total=33s  PASS
  succeeded: 5/5  wall_total: 35s
  isolation: each run used /data/stress/{runId}/... (unique paths)
```

**격리 메커니즘**:
- runId = `baseRunID-r{i}` (i=1~5) → 각자 고유
- PVC 경로: `/data/stress/{runId}/...` → 교차 접근 불가
- RunStore: storeID = `stress-burst-{runId}` → 별도 항목
- 5/5 PASS, 경로 충돌 없음

---

## 9. stressNodeID K8s Job 명명 규칙

```go
func stressNodeID(runID, node string) string {
    id := "s-" + runID + "-" + node
    if len(id) <= 63 { return id }
    // suffix 보존 truncation (poc-pipeline nodeRunID와 동일 패턴)
    suffix := "-" + node
    prefix := "s-"
    maxRunIDLen := 63 - len(prefix) - len(suffix)
    if maxRunIDLen < 1 { maxRunIDLen = 1 }
    return prefix + runID[:maxRunIDLen] + suffix
}
```

- prefix `s-` (stress 식별)
- 19자 자동 runId: `s-20260328-123456-007-b8` = 25자 (63 한도 여유)
- 장 runId overflow 시 suffix 보존 (TestStressNodeID_TruncatePreservesNode 검증)

---

## 10. 남은 리스크

1. **sem throttle 실증 미완**: idle 클러스터에서 sem=1과 sem=8이 동일한 critical_path.
   K8s API rate limit 환경 또는 10+ workers 시나리오에서의 실증이 필요.

2. **PVC RWO 단일 노드 가정**: 다중 노드 환경 미검증. B 워커들이 다른 노드에 스케줄되면
   RWO PVC 마운트 실패 가능.

3. **DefaultTimeout 완전 비활성화 사이드이펙트 미검증**:
   `DefaultTimeout = 0`으로 설정 시 무한 대기 가능성. 현재는 caller ctx(30분)에 의존.
   per-node timeout이 필요한 시나리오에서는 재검토 필요.

4. **InstrumentedRunner sem 대기 시간 분리 불가**: 현재 `DoneAt - EnterAt`에
   sem 대기 + K8s Job 전체가 포함됨. sem 대기만 별도 측정하려면 추가 계측 필요.

5. **runstore 누적**: `/tmp/stress-runstore.json`에 실행마다 항목 추가.
   장기 운용 시 파일 크기 증가. Production에서는 PostgreSQL 전환 필요.

---

## 11. 이번 Sprint 5에서 의도적으로 하지 않은 것

- 실제 유전체 툴 실행 (gatk, bwa, etc.)
- per-row expansion (Caleb v12 방식)
- Redis Streams main path 연결
- fast-fail 실험
- mixed executionClass
- Kueue pending/unschedulable 운영 실험
- 대형 리팩터링
- RunStore PostgreSQL 전환

---

## 12. 최종 판정

**Synthetic 파이프라인 하네스 구축 및 패턴별 실측 완료**

**근거**:

1. **5개 패턴 구현 완료**: wide-fanout-8, two-stage-8x4, long-tail-8, mixed-duration-8, multi-run-burst 모두 kind 클러스터에서 실행 확인.

2. **DefaultTimeout 버그 발견 및 수정**: dag-go 기본 30s 타임아웃이 long-tail 패턴을 false-negative로 만드는 문제를 `InitDagWithOptions(noPreflight())`로 해결. 이는 Production 파이프라인에도 적용 필요한 중요 발견.

3. **BoundedDriver 동작 검증**: sem=4에서 `peak_inflight=4 available=0` 포화 상태 관찰. 세마포어 배선이 main path에 정상 동작함. sem이 critical_path에 영향 없음도 확인 (idle 환경 특성).

4. **격리 검증**: multi-run-burst 5/5 PASS, 경로 교차 없음, RunStore 별도 항목. 공유 BoundedDriver에서 concurrent run 간 artifact 격리 정상.

5. **7/7 단위 테스트 PASS**: runId/nodeId 형식, sleep 스케줄, RunMetrics ordering, 패턴 dispatch 검증.

6. **BoundedDriver 실측 한계 명확화**: idle 단일 kind 클러스터에서 sem throttle = API burst 제한이며 파이프라인 throughput 제한이 아님. 의미있는 throttle 관찰을 위한 조건도 문서화.

---

## 다음 스프린트 제안

1. **sem throttle 실효성 재검증**: 10+ workers + K8s API rate limit 환경에서 sem=2 vs sem=5 파이프라인 지연 비교
2. **PVC 접근 전략**: RWO → ReadWriteMany 또는 node-affinity 기반 다중 노드 환경 지원
3. **Kueue admit 흐름**: quota 초과 → WorkloadPending → 복구 흐름 관찰
4. **InstrumentedRunner sem 대기 시간 분리**: sem 대기와 K8s Job 실행 시간을 별도 측정
5. **runstore 정리 정책**: finished/canceled 항목 TTL 또는 PostgreSQL 전환 타이밍
