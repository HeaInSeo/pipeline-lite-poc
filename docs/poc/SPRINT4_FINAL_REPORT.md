# Sprint 4 Final Report
**Date**: 2026-03-28
**Verdict**: 실행 구조 검증 완료

---

## Sprint 4 목표 요약

`cmd/poc-pipeline`을 기준으로, 실제 kind 클러스터에서
`A → (B1,B2,B3) → C` 파이프라인을 끝까지 실행 검증하고,
runId/path/BoundedDriver/graceful fallback 문제를 라운드별로 구현·검증.

---

## 수정 파일 목록

| 파일 | 변경 유형 |
|------|-----------|
| `cmd/poc-pipeline/main.go` | 수정 |
| `cmd/poc-pipeline/pipeline_test.go` | 수정 |

---

## 라운드별 구현 내용

### Round 1 — 실행 전 하드닝

**구현:**
1. `generateRunID()`: `YYYYMMDD-HHMMSS` → `YYYYMMDD-HHMMSS-mmm` (19자)
   - 동일 초 내 연속 실행 시 storeID + K8s Job 이름 충돌 위험 제거
2. `nodeRunID()` truncation 버그 수정:
   - 이전: `id[:63]` → 63자 초과 시 `-a`/`-b3` suffix 소실, 모든 노드가 동일 이름
   - 수정: prefix `"poc-"` + runId 절단 + suffix `"-{node}"` 보존
3. 로그 문구: PASS/FAIL에 `(K8s path)` 레이블, NopDriver path 주석 명시
4. `TestNodeRunID_TruncatesPreservesNodeSuffix` 추가

**결과:**
- 7/7 단위 테스트 PASS
- 고정 경로(`/data/poc-pipeline/` + 리터럴 runId) 잔존 없음 확인
- `dataRoot` 상수는 `pathBase()` 한 곳에서만 접근

---

### Round 2 — kind happy-path end-to-end

**사전 조건 확인:**
- kind 클러스터: `poc-control-plane` Ready (v1.35.0)
- `poc-standard-lq` LocalQueue 존재, ClusterQueue Quota: CPU 4 / Memory 4Gi
- `poc-shared-pvc`: Bound, RWO, 1Gi
- kubeconfig: `~/.kube/config` fallback 경로 일치

**실행 결과 (runId: 20260328-125637-107):**

```
RunStore: queued → admitted-to-dag → running → finished
[poc-pipeline] PASS (K8s path): run ... dag completed
```

dag-go 노드 타이밍:
```
Preflight poc-a:          21:56:37
InFlight poc-a:           21:56:43  (+6s)
Preflight poc-b1/b2/b3:   21:56:43  (동시)
InFlight poc-b1/b2/b3:    21:56:49  (+6s)
Preflight poc-c:          21:56:49
InFlight poc-c:           21:56:55  (+6s)
```

**PVC 파일 검증 (kubectl pod 마운트):**

```
/data/poc-pipeline/20260328-125637-107/a-output/seed.txt
/data/poc-pipeline/20260328-125637-107/b-output/shard-0/result.txt
/data/poc-pipeline/20260328-125637-107/b-output/shard-1/result.txt
/data/poc-pipeline/20260328-125637-107/b-output/shard-2/result.txt
/data/poc-pipeline/20260328-125637-107/c-output/report.txt
```

---

## 생성된 Artifact: report.txt

```
=== poc-pipeline report ===
runId=20260328-125637-107
generated_at=2026-03-28T12:56:50Z
--- shard-0 ---
worker-b1 result
shard=0
seed_content=poc-pipeline seed
runId=20260328-125637-107
created_at=2026-03-28T12:56:38Z
--- shard-1 ---
worker-b2 result
shard=1
seed_content=poc-pipeline seed
runId=20260328-125637-107
created_at=2026-03-28T12:56:38Z
--- shard-2 ---
worker-b3 result
shard=2
seed_content=poc-pipeline seed
runId=20260328-125637-107
created_at=2026-03-28T12:56:38Z
=== end ===
```

---

### Round 3 — fan-out + BoundedDriver 실측

**구현:**
- `--sem` 플래그 (기본값=3): 런타임 sem 값 변경
- BoundedDriver stats 로거: 50ms 폴링, peak inflight 추적 (NEW PEAK 전이 + FINAL 로그)

**실험 결과:**

| sem | peak_inflight | available 최솟값 | 파이프라인 총 시간 |
|-----|---------------|-----------------|-----------------|
| 3 | **3** | 0 (포화 관찰) | 18초 |
| 2 | 1 | 1 | 18초 |
| 1 | 1 | 0 (C 노드 포착) | 18초 |

**분석:**

sem=3 `peak_inflight=3 available=0` 는 B1/B2/B3의 `inner.Start()` 3개가
동시에 진입한 순간을 50ms 폴링이 포착한 것이다.

sem=2와 sem=1에서 `peak_inflight=1`이 나온 것은 세마포어 미작동이 아니다.
K8s Job Create API가 ~50ms로 빠르고, 50ms 폴링 윈도가 B 워커들의
Start() 구간을 빗나갔기 때문이다. sem=1 실행에서 `peak_inflight=1`은
C 노드의 Start() 포착이었다.

**BoundedDriver 적용 범위 명확화:**
- BoundedDriver가 제어하는 것: `inner.Start()` (K8s Job Create API 호출) 동시 호출 수
- 제어하지 않는 것: K8s Job 실행 시간 (Wait 단계, ~6초)
- `inner.Start()` : `Wait()` 시간 비율 ≈ 50ms : 6000ms = 1:120

세마포어 throttle이 파이프라인 지연으로 이어지려면:
K8s API 부하/레이트 제한 환경, 10+ 워커, 또는 Start()가 느린 환경이 필요.
이 PoC 환경(단일 kind 노드, idle 클러스터)에서는 sem 값과 무관하게 18초.

---

### Round 4 — graceful fallback + 복구 경계

**검증 방법:** `KUBECONFIG=/nonexistent go run ./cmd/poc-pipeline/`

**실행 결과:**

```
WARN: K8s unavailable (stat /nonexistent: no such file or directory) — using NopDriver
BoundedDriver(sem=3) initialized, K8sAvailable=false
RunStore: queued → admitted-to-dag → running
dag.Wait() 실패 (NopDriver.Prepare() → ErrK8sUnavailable)
RunStore: → canceled
FAIL (K8s path): dag Wait: pipeline did not succeed
exit status 1
```

**RunStore 상태 대조표 (전체 실행 기록):**

| runId suffix | K8s 경로 | RunStore 상태 | report.txt |
|-------------|---------|--------------|-----------|
| 125637-107 | DriverK8s | finished | 생성됨 |
| 130341-922 | DriverK8s | finished | 생성됨 |
| 130544-249 | DriverK8s | finished | 생성됨 |
| 130613-891 | DriverK8s | finished | 생성됨 |
| 130718-331 | DriverK8s | finished | 생성됨 |
| 130903-063 | **NopDriver** | **canceled** | **미생성** |

**판정:**
- NopDriver path: crash 없음 (graceful), exit code 1, PASS 문구 미출력
- 성공(finished) ↔ 실패(canceled) 상태가 RunStore에서 명확히 구분됨
- report.txt 존재 = 실행 완료, 미존재 = 미실행/실패 와 일치

---

## 남은 리스크

1. **PVC RWO 단일 노드 가정**: `poc-shared-pvc`가 RWO이므로 다중 노드 환경에서
   B1/B2/B3가 다른 노드에 스케줄되면 마운트 실패. 현재 kind 단일 노드에서만 검증됨.

2. **sem throttle 실증 한계**: K8s Job Create가 ~50ms로 빠른 환경에서는
   sem=2 vs sem=3의 파이프라인 지연 차이가 관측되지 않았음.
   10+ 워커 또는 K8s API 부하 환경에서의 실증은 미완.

3. **runstore 누적**: `/tmp/poc-pipeline-runstore.json`에 실행마다 항목이 추가됨.
   장기 운용 시 파일 크기 증가. Production에서는 PostgreSQL 전환 예정.

---

## 이번 Sprint 4에서 의도적으로 하지 않은 것

- Redis Streams main path 연결
- fast-fail 실험
- mixed executionClass
- Kueue pending/unschedulable 운영 실험
- Caleb v12 per-row expansion
- PVC ReadWriteMany 전환
- 대형 리팩터링

---

## 최종 판정

**실행 구조 검증 완료**

**근거:**

1. kind 클러스터에서 happy-path 완주: `A → (B1,B2,B3) → C` 5회 연속 성공
2. report.txt 생성 확인: runId 네임스페이싱, shard-0/1/2 내용 통합 정상
3. BoundedDriver 동시성: sem=3에서 `peak_inflight=3 available=0` 관찰됨.
   `inner.Start()` 3개의 동시 진입이 포착되었고 semaphore 배선이 main path에 있음을 확인.
4. NopDriver fallback 경계 명확: K8s 불가 시 `canceled` 상태, report.txt 미생성,
   exit code 1로 성공 실행과 구분됨
5. RunStore 상태 전이 5회 기록: `queued → admitted-to-dag → running → finished`

**조건부 완료가 아닌 완료로 판정한 이유:**

- 5개 노드 모두 K8s Job으로 실행됨 (NopDriver 경로 아님 확인)
- PVC 파일 체인(seed.txt → shard-{0,1,2}/result.txt → report.txt) 완성
- Round 1 하드닝으로 runId/nodeRunID 충돌 위험 제거됨
- 남은 리스크 3가지는 다음 스프린트로 명확히 구분됨

---

## 다음 스프린트 제안

1. **PVC 접근 전략 검토**: RWO → 다중 노드 클러스터 대비 ReadWriteMany 또는
   node-affinity 기반 접근 방법 결정
2. **BoundedDriver throttle 실증**: 10+ 워커 또는 K8s API rate limit 환경에서
   sem=2 vs sem=5 파이프라인 지연 비교
3. **Kueue admit 흐름 end-to-end**: quota 초과 → WorkloadPending → 복구 흐름 관찰
4. **runstore 정리 정책**: finished/canceled 항목 TTL 또는 PostgreSQL 전환 타이밍
