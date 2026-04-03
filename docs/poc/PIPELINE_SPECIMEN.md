# Pipeline Specimen v0.1

**Date**: 2026-04-03
**Sprint**: 7
**Status**: Baseline confirmed (end-to-end complete)

---

## 1. 목적

**specimen v0.1은 dummy app이 반복 제출할 canonical workload baseline이다.**

- Sprint 8 이후 dummy app / scheduler 실험에서 "표준 run 1건"의 정의가 된다.
- PoC main path(Redis ingress → RunGate → dag-go → BoundedDriver → K8s Job)의 동작을
  고정된 shape와 이름으로 반복 재현 가능하게 만든다.
- 새로운 의미론이나 content dependency를 도입하지 않는다.
- specimen은 이후 별도 기준선으로 계속 업데이트될 예정이다.

---

## 2. Shape

```
start → prepare → worker-1 ─┐
                  worker-2 ─┼→ collect → end
                  worker-3 ─┘
```

- **prepare**: 선행 단계. 모든 output 디렉토리를 생성하고 done.txt를 기록한다.
- **worker-1 / worker-2 / worker-3**: 병렬 실행되는 3개의 워커 노드.
  각각 독립적으로 자신의 output 디렉토리에 done.txt를 기록한다.
- **collect**: 수렴 단계. worker-N-output/done.txt 존재를 검증하고 report.txt를 작성한다.

노드 수(workers=3)는 v0.1 고정값이다. 파라미터화는 이후 버전에서 결정한다.

---

## 3. 각 노드의 역할

### prepare
- 역할: 파이프라인 실행 전 환경 준비
- 하는 일: `pBase/prepare-output/`, `pBase/worker-N-output/`, `pBase/collect-output/` 생성
- 기록: `prepare-output/done.txt` (node=prepare, runID, 타임스탬프)
- K8s 이미지: `busybox:1.36`
- Kueue 큐: `poc-standard-lq`

### worker-1 / worker-2 / worker-3
- 역할: 병렬 처리 워크로드
- 하는 일: 각자 `worker-N-output/done.txt` 기록
- 실행 시점: prepare 완료 후 3개 동시 시작 (dag-go fan-out)
- content dependency 없음 — 워커 간 데이터 교환 없음
- K8s 이미지: `busybox:1.36`

### collect
- 역할: 수렴 및 결과 취합
- 하는 일: worker-N-output/done.txt 존재 검증 → collect-output/report.txt 작성
- 실행 시점: worker-1/2/3 전체 완료 후 시작 (dag-go fan-in)
- collect가 실패하면 K8s Job exit 1 → dag-go가 실패 전파 → XACK 미호출 → PEL 잔류

---

## 4. Naming 규칙

### DAG 노드 ID

| 역할 | 노드 ID |
|------|---------|
| 준비 단계 | `prepare` |
| 워커 1 | `worker-1` |
| 워커 2 | `worker-2` |
| 워커 3 | `worker-3` |
| 수렴 단계 | `collect` |

### K8s Job 이름

```
i-{runID}-{nodeID}
```

예시 (runID=20260403-083318-075):
- `i-20260403-083318-075-prepare`
- `i-20260403-083318-075-worker-1`
- `i-20260403-083318-075-worker-2`
- `i-20260403-083318-075-worker-3`
- `i-20260403-083318-075-collect`

prefix `i-` = ingress. 최대 63자 제한 적용 (`ingressNodeID()` 함수).

### 출력 디렉토리

```
/data/ingress/{runID}/
  prepare-output/done.txt
  worker-1-output/done.txt
  worker-2-output/done.txt
  worker-3-output/done.txt
  collect-output/report.txt
```

모두 `poc-shared-pvc` (`/data` 마운트) 위에 위치한다.

---

## 5. 실행 방법

### 전제 조건

```bash
# Redis (127.0.0.1 바인딩)
podman run -d --name poc-redis -p 127.0.0.1:6379:6379 redis:7-alpine

# kind 클러스터 + PVC 확인
kubectl get nodes
kubectl get pvc poc-shared-pvc -n default
```

### specimen 실행 (produce + dispatch + once, 통합 방식)

```bash
go run -tags redis ./cmd/ingress/ --produce --once --pipeline specimen
```

### 분리 방식

```bash
# 터미널 A: Dispatcher 대기
go run -tags redis ./cmd/ingress/ --once --pipeline specimen

# 터미널 B: 수동 XADD
redis-cli -h 127.0.0.1 XADD poc:runs '*' \
  run_id "specimen-001" \
  payload '{"run_id":"specimen-001","created_at":"2026-04-03T00:00:00Z"}'
```

### 실패 경로 검증 (PEL 잔류 확인)

```bash
go run -tags redis ./cmd/ingress/ --produce --once --pipeline specimen --fail
# 예상: XPENDING count=1 (XACK 미호출)
```

### 검증

```bash
# K8s Job 5개 Complete
kubectl get jobs -n default | grep "^i-"

# XACK 완료
podman exec poc-redis redis-cli XPENDING poc:runs poc-workers - + 10
# → (empty)

# runstore finished
cat /tmp/ingress-runstore.json
```

---

## 6. Acceptance Criteria (v0.1 기준)

| # | 항목 | 기준 |
|---|------|------|
| 1 | `--pipeline specimen` 완주 | state=finished, XPENDING count=0 |
| 2 | K8s Jobs | prepare/worker-1/worker-2/worker-3/collect 5개 Complete |
| 3 | XACK | 성공 후 XACK ok 로그 |
| 4 | runstore | state=finished |
| 5 | `--fail` regression | XPENDING count=1, XACK 미호출 |
| 6 | 기본 빌드 | `go build ./...` (redis 태그 없음) PASS |
| 7 | `--pipeline fanout` regression | Sprint 6 경로 동일하게 완주 |

---

## 7. Non-Goals (v0.1)

- **content dependency**: worker가 prepare output을 읽는 의미론 없음
- **workers N 파라미터화**: workers=3 고정. `--workers N` 플래그 없음
- **sequential topology**: 이번 shape는 fan-out/fan-in만 포함
- **pkg/ 추출**: `runSpecimenPipeline()`은 `cmd/ingress/main.go` 내 inline. 별도 패키지 없음
- **XCLAIM 자동 복구**: PEL 잔류만 보장. 자동 재시도 없음
- **Redis AOF/RDB 내구성**: 재시작 후 stream 복구 미검증
- **다중 run 동시 제출**: Dispatcher는 동기 단일 처리
- **dummy app 구현**: specimen은 dummy app이 제출할 workload 정의일 뿐, dummy app 자체가 아님
- **스케줄러/front-end 확장**: Sprint 8+ 범위

---

## 8. Dummy app과의 연결 맥락

Sprint 8 이후 dummy app은 이 specimen을 반복 제출하는 역할을 맡는다.

```
dummy app → XADD(poc:runs) → Dispatcher → runSpecimenPipeline() → 5 K8s Jobs → XACK
```

- dummy app은 run 1건 = specimen 1회 완주를 의미하는 것으로 가정한다.
- specimen shape(prepare/worker-N/collect)와 naming 규칙이 dummy app / 스케줄러 실험의
  공통 기준선이 된다.
- v0.1이 안정화되면 이 문서를 업데이트하여 v0.2, v0.3으로 진화시킨다.

---

## 9. 코드 위치

| 항목 | 위치 |
|------|------|
| 파이프라인 함수 | `cmd/ingress/main.go` — `runSpecimenPipeline()` |
| prepare spec | `cmd/ingress/main.go` — `specimenSpecPrepare()` |
| worker spec | `cmd/ingress/main.go` — `specimenSpecWorker()` |
| collect spec | `cmd/ingress/main.go` — `specimenSpecCollect()` |
| Job naming | `cmd/ingress/main.go` — `ingressNodeID()` (fanout과 공유) |
| fanout 기준선 | `cmd/ingress/main.go` — `runWideFanout3()` (Sprint 6, 변경 없음) |
