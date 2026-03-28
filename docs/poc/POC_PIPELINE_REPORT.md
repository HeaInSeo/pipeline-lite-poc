# poc-pipeline Implementation Report
**Date**: 2026-03-28
**Scope**: cmd/poc-pipeline — fan-out + collect PoC 핵심 검증 파이프라인 구현

---

## 배경

Sprint 3에서 구조적 핵심 질문(Q1–Q5)이 모두 PASS 판정됨.
다음 단계로 "실제 파일 handoff가 있는 fan-out+collect 파이프라인"을 구현하여
caleb 레포지토리의 설계 방향을 검증한다.

---

## Caleb 분석 → 설계 선택 근거

### caleb 파이프라인 진화 요약

| 버전 | 핵심 변경 |
|------|----------|
| v2–v8 | 초기 구조 정의 |
| v9 | OverlayFS 도입 (lowerDir/upperDir) |
| v9.1 | explicit lowerDirs 배열 |
| v10 | literal path 기반 (OverlayFS 없음) |
| v11 | template token 기반 경로 (`{inputDir}`, `{outputDir}`) |
| v12 | `inputsFromBlocks` + `stagingPolicy` + `inputsResolution` (tori 특화) |

### 첫 PoC에서 제외한 것

- **v12 stagingPolicy / inputsFromBlocks / inputsResolution**: tori 생물정보학 파이프라인 특화, 범용 PoC에 불필요
- **OverlayFS (v9)**: K8s Job 환경에서 권한 문제, 단순 PVC 디렉터리로 대체
- **executionClass 혼합 (standard/highmem)**: cmd/execclass에서 이미 검증됨, 이번 범위 제외
- **fast-fail 실험**: cmd/fastfail에서 이미 검증됨, 이번 범위 제외
- **Redis Streams / DagSession**: Sprint 3에서 평가/레이블링 완료

### 채택한 설계 (slim Option B)

```
A(ingest) → B1/B2/B3(worker 병렬) → C(collect)
```

v11의 "shared PVC + 경로 규칙 기반 handoff"를 직접 구현한 형태.
v12의 tori 특화 기능 없이, runId 네임스페이싱으로 다중 실행을 격리.

---

## 구현 내용

### 파일

```
poc/cmd/poc-pipeline/
├── main.go           (구현)
└── pipeline_test.go  (단위 테스트)
```

### DAG 구조

```
start → poc-a → poc-b1 ─┐
               → poc-b2 ─┼→ poc-c → end
               → poc-b3 ─┘
```

### PVC 경로 설계 (runId 네임스페이싱)

```
/data/poc-pipeline/{runId}/a-output/seed.txt
/data/poc-pipeline/{runId}/b-output/shard-0/result.txt
/data/poc-pipeline/{runId}/b-output/shard-1/result.txt
/data/poc-pipeline/{runId}/b-output/shard-2/result.txt
/data/poc-pipeline/{runId}/c-output/report.txt
```

**runId 자동 생성**: `YYYYMMDD-HHMMSS` (UTC 타임스탬프)
- 수동 runstore 삭제 없이 매 실행마다 새 runId 생성
- `--run-id` 플래그로 override 가능
- K8s Job 이름: `poc-{runId}-{node}` (최대 21자, 63자 제한 내)

### 노드 역할

| 노드 | 역할 | 입력 | 출력 |
|------|------|------|------|
| poc-a | 디렉터리 준비 + seed.txt 생성 | 없음 | a-output/seed.txt + 전체 디렉터리 트리 |
| poc-b1 | worker (shard-0) | seed.txt | b-output/shard-0/result.txt |
| poc-b2 | worker (shard-1) | seed.txt | b-output/shard-1/result.txt |
| poc-b3 | worker (shard-2) | seed.txt | b-output/shard-2/result.txt |
| poc-c | collect | shard-{0,1,2}/result.txt | c-output/report.txt |

### A 노드의 디렉터리 준비 전략

```bash
set -e
mkdir -p {pBase}/a-output
mkdir -p {pBase}/b-output/shard-0
mkdir -p {pBase}/b-output/shard-1
mkdir -p {pBase}/b-output/shard-2
mkdir -p {pBase}/c-output
```

A가 전체 트리를 준비 → B/C는 파일만 기록.
B 노드들이 동시에 `mkdir` 경합하지 않음.

### report.txt 형식

```
=== poc-pipeline report ===
runId={runId}
generated_at={ISO8601 timestamp}
--- shard-0 ---
{shard-0/result.txt 내용}
--- shard-1 ---
{shard-1/result.txt 내용}
--- shard-2 ---
{shard-2/result.txt 내용}
=== end ===
```

### 아키텍처 (Sprint 3 표준 패턴 준수)

```
main() → RunGate.Admit() → runFanoutCollect() → dag-go → SpawnerNode.RunE()
                                                            → BoundedDriver(sem=3)
                                                            → DriverK8s → K8s API
```

- K8s init 실패 → NopDriver graceful fallback (log.Fatalf 아님)
- bootstrapRecovery(): 재시작 시 queued/held 상태 로깅

---

## 테스트 결과

```
=== RUN   TestGenerateRunID_Format
--- PASS: TestGenerateRunID_Format (0.00s)
=== RUN   TestPathBase
--- PASS: TestPathBase (0.00s)
=== RUN   TestNodeRunID_Format
--- PASS: TestNodeRunID_Format (0.00s)
=== RUN   TestNodeRunID_Truncates
--- PASS: TestNodeRunID_Truncates (0.00s)
=== RUN   TestSpecA_Paths
--- PASS: TestSpecA_Paths (0.00s)
=== RUN   TestSpecWorker_Shards
--- PASS: TestSpecWorker_Shards (0.00s)
=== RUN   TestSpecCollect_Paths
--- PASS: TestSpecCollect_Paths (0.00s)
PASS
ok  github.com/seoyhaein/poc/cmd/poc-pipeline   0.011s
```

7/7 PASS.

---

## 검증 범위 및 한계

### 이 구현이 검증하는 것

1. **dag-go fan-out 구조**: A→B1/B2/B3 병렬 의존성 + B1/B2/B3→C 수렴 의존성
2. **shared PVC file handoff**: seed.txt → result.txt → report.txt 경로 체인
3. **runId 네임스페이싱**: 매 실행 격리, runstore 충돌 없음
4. **RunGate + BoundedDriver**: Sprint 3 표준 main path 패턴 적용

### 이 구현이 검증하지 않는 것 (향후 과제)

- 실제 K8s 클러스터 end-to-end 실행 (NopDriver 환경에서 구조만 검증)
- Kueue 어드미션 흐름 (cmd/execclass, deploy/poc/ 참조)
- B 노드 실패 시 C 대기 동작 (fast-fail 흐름은 cmd/fastfail 참조)
- Redis Streams ingress queue 통합 (pkg/queue, //go:build redis)

---

## 판정

**구현 완료**: slim Option B fan-out+collect 파이프라인이 코드와 테스트로 구현됨.

caleb v11의 "shared PVC + 경로 규칙" 패턴을 runId 네임스페이싱으로 확장.
Sprint 3에서 확정된 main path(RunGate → dag-go → BoundedDriver → DriverK8s)를
그대로 준수함.

다음 단계: 실제 kind 클러스터에서 `go run ./cmd/poc-pipeline/` end-to-end 실행.
