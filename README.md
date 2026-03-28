# pipeline-lite-poc

Pipeline-Lite + dag-go + spawner + Kueue + kube-scheduler 조합 검증 PoC

## 목적

10일 안에 이 조합이 실제로 동작하는지 검증한다.
production-grade 구현이 아님.

## 검증 항목

- DAG dependency (A→B→C)
- fan-out + collect (A → B1/B2/B3 → C, shared PVC file handoff)
- executionClass (standard / highmem)
- resourceProfile (small / medium)
- fast-fail
- ingress boundary (RunGate + JsonRunStore)
- release queue burst control (BoundedDriver semaphore)

## 환경

- kind + podman
- Kubernetes (kind 클러스터)
- Kueue
- Go 1.25+

## 구조

```
poc/
├── cmd/
│   ├── poc-pipeline/    # PoC 핵심: A→(B1,B2,B3)→C fan-out+collect (runId PVC 네임스페이싱)
│   ├── pipeline-abc/    # 선형 A→B→C + shared PVC handoff
│   ├── fanout/          # A→B1/B2/B3 fan-out 구조 검증
│   ├── fastfail/        # fast-fail 동작 검증
│   ├── execclass/       # executionClass 분리 검증
│   ├── dag-runner/      # dag-go 기본 동작 검증
│   ├── runner/          # 단일 노드 실행 검증
│   └── operational/     # 운영 감각 검증
├── docs/poc/            # PoC 문서 (스펙, 진행 로그, 스프린트 리포트)
├── hack/                # kind 클러스터 관리 스크립트
├── deploy/
│   ├── kueue/           # Kueue 설치 매니페스트
│   └── poc/             # 샘플 Job, Queue, PVC 매니페스트
├── pkg/
│   ├── adapter/         # dag-go ↔ spawner 연결 glue
│   ├── ingress/         # RunGate + JsonRunStore (ingress boundary)
│   ├── observe/         # 운영자 중단 상태 구분 (6개 StopCause)
│   ├── queue/           # Redis Streams 큐 후보 (//go:build redis)
│   ├── session/         # DagSession (실험적 경로, production 불가)
│   └── store/           # JsonRunStore (durable-lite 상태 저장)
└── caleb/               # 참조 전용 (spec 추출)
```

## cmd/poc-pipeline (핵심 검증 파이프라인)

```
start → poc-a → poc-b1 ─┐
               → poc-b2 ─┼→ poc-c → end
               → poc-b3 ─┘
```

- **A(ingest)**: 디렉터리 트리 준비 + seed.txt 생성
- **B1/B2/B3(worker)**: seed.txt 읽어 shard-{0,1,2}/result.txt 병렬 생성
- **C(collect)**: 3개 shard 검증 후 c-output/report.txt 통합

**PVC 경로 (runId 네임스페이싱)**:
```
/data/poc-pipeline/{runId}/a-output/seed.txt
/data/poc-pipeline/{runId}/b-output/shard-{0,1,2}/result.txt
/data/poc-pipeline/{runId}/c-output/report.txt
```

**실행 방법**:
```bash
go run ./cmd/poc-pipeline/
go run ./cmd/poc-pipeline/ --run-id my-test-run
```

## 아키텍처 (Sprint 3 확정)

```
cmd/* → RunGate → dag-go → SpawnerNode.RunE() → BoundedDriver(sem=3) → DriverK8s → K8s API
```

- **RunGate**: 모든 실행 전 ingress boundary 등록 (Q1 fix)
- **BoundedDriver**: concurrent K8s Job creation 상한 제어 (Q2 fix)
- **dag-go**: 유일한 readiness/dependency source of truth
- **DagSession**: 실험적 경로 (production 불가)

## 참조 Repo

- dag-go: github.com/seoyhaein/dag-go
- spawner: github.com/seoyhaein/spawner
- caleb: 참조 전용 (spec 추출)

## 진행 상황

docs/poc/ 참조:
- `SPRINT3_FINAL_REPORT.md` — Q1/Q2 PARTIAL 해소, BoundedDriver, Redis Streams 평가
- `PROGRESS_LOG.md` — 전체 진행 로그

## Sprint 3 판정 요약

| 질문 | 판정 |
|------|------|
| Q1: Ingress boundary main path? | **PASS** — 7/7 cmd/* RunGate 적용 |
| Q2: BoundedDriver/release queue 실효성? | **PASS** — sem=3, main path 배치 |
| Q3: DagSession 역할 명확성? | **PASS** — 실험적 경로 레이블링 |
| Q4: 운영자 중단 상태 구분 가능? | **PASS** — 6개 StopCause 모두 observable |
| Q5: 내구성 저장소 수준? | **PASS** — Redis Streams 평가 완료 |
