# PROGRESS_LOG

## Day 1 - 2026-03-27

### 오늘 목표
- POC_SCOPE.md 작성
- PIPELINE_LITE_SPEC.md 작성
- PROGRESS_LOG.md 생성
- caleb 참고 지점 정리
- 목표/비목표/완료 기준/폐기 기준 고정
- 코드 변경 없음

### 변경 파일 목록
- poc/docs/poc/POC_SCOPE.md (신규)
- poc/docs/poc/PIPELINE_LITE_SPEC.md (신규)
- poc/docs/poc/PROGRESS_LOG.md (신규)
- poc/docs/poc/KUEUE_LAB.md (placeholder 신규)
- poc/docs/poc/POC_EVALUATION.md (placeholder 신규)
- poc/.gitignore (신규)
- poc/README.md (신규)

### 구현한 내용
[Describe: directory structure created, documents written, GitHub repo created at HeaInSeo/pipeline-lite-poc, git initialized and first commit pushed]

### caleb 참고 지점
List the key caleb files referenced:
- caleb/pipeline_v11.jsonc: node 구조, dependsOn, resource limits 참조
- caleb/pipelone_v12.jsonc: executionClass, resourceProfile 개념 참조
- caleb/director.md: 디렉토리 정책 참조
- caleb/thinking.md: enhancement proposals 참조

Key extractions used:
- executionClass → standard/highmem → Kueue LocalQueue 매핑
- resourceProfile → K8s resource requests/limits
- dependsOn → dag-go edge definition
- fan-out → dag-go parallel nodes
- fast-fail → dag-go built-in parent failure propagation

### 검증 명령
None (Day 1 is documentation only)

### 아직 안 된 것
- 모든 Day 2~10 작업

### 리스크 / 막힌 점
- kind가 sudo wrapper로 실행됨 → sudoers SETENV 설정으로 해결 예정 (Day 2)
- spawner go.mod에 client-go 없음 → Day 3에 추가 예정

### 내일 첫 작업 제안
kind 클러스터 생성 (kind-up.sh 작성 및 실행)
