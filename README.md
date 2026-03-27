# pipeline-lite-poc

Pipeline-Lite + dag-go + spawner + Kueue + kube-scheduler 조합 검증 PoC

## 목적

10일 안에 이 조합이 실제로 동작하는지 검증한다.
production-grade 구현이 아님.

## 검증 항목

- DAG dependency (A→B→C)
- sample fan-out (3~5개 병렬)
- executionClass (standard / highmem)
- resourceProfile (small / medium)
- shared PVC handoff
- fast-fail

## 환경

- kind + podman
- Kubernetes (kind 클러스터)
- Kueue
- Go 1.25+

## 구조

poc/
├── docs/poc/        # PoC 문서 (스펙, 진행 로그, 평가)
├── hack/            # kind 클러스터 관리 스크립트
├── deploy/
│   ├── kueue/       # Kueue 설치 매니페스트
│   └── poc/         # 샘플 Job, Queue, PVC 매니페스트
├── pkg/adapter/     # dag-go ↔ spawner 연결 glue
└── cmd/runner/      # Pipeline-Lite 실행 진입점

## 참조 Repo

- dag-go: github.com/seoyhaein/dag-go
- spawner: github.com/seoyhaein/spawner
- caleb: 참조 전용 (spec 추출)

## 진행 상황

docs/poc/PROGRESS_LOG.md 참조

## 10일 일정

| Day | 내용 |
|-----|------|
| 1 | 문서 작성, 목표/기준 확정 |
| 2 | kind + Kueue 환경 구성 |
| 3 | spawner K8s driver 최소 구현 |
| 4 | dag-go + spawner 단일 노드 연결 |
| 5 | A→B→C 관통 + shared PVC handoff |
| 6 | fan-out + Kueue queue label 주입 |
| 7 | fast-fail 구현 |
| 8 | executionClass 분리 |
| 9 | 운영 감각 검증 |
| 10 | POC_EVALUATION.md 작성 |
