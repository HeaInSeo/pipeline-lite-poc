# Pipeline Representation Spec v0.1

> 작성일: 2026-04-04
> 이전 문서: PIPELINE_LITE_SPEC.md (2026-03-27, 10일 PoC 전용)
> 상태: 설계 기준선 (미구현 항목 포함)

---

## 읽기 전에

이 문서는 파이프라인 기반 유전체 분석 시스템의 **파이프라인 표현 모델**을 정의한다.

이 시스템에서 "파이프라인"은 단 하나의 고정된 형태가 아니다. 사용자가 작성한 파이프라인은
단계를 거치면서 다른 정보가 붙고, 일부 정보는 소모되어 사라진다. 같은 파이프라인이지만
**각 단계에서 필요한 정보가 다르기 때문에**, 단계별로 명시적인 표현(representation)을 두고
변환을 거치는 방식을 택한다.

이 문서를 처음 읽는 사람을 위해 핵심 흐름을 먼저 보면:

```
사용자가 분석 도구와 순서를 DAG로 작성한다    →  AuthoredPipeline
실제 분석할 파일 데이터가 파이프라인에 붙는다  →  BoundPipeline
어떤 자원을 쓸지, 어느 큐로 보낼지 결정된다   →  ScheduledPipeline
K8s가 실제로 실행할 수 있는 최종 형태가 된다  →  ExecutablePipeline
                                             →  K8s Jobs 실행
```

---

## 1. 범위와 목적

이 문서는 파이프라인 표현을 4단계로 구분하고, 각 단계의 책임·필드·변환 계약·금지 항목을
고정한다.

```
AuthoredPipeline → BoundPipeline → ScheduledPipeline → ExecutablePipeline
```

### 1.1 이 문서가 해결하려는 문제

**문제 1: 하나의 거대한 Pipeline struct**

흔히 저지르는 실수는 파이프라인에 필요한 모든 정보를 하나의 큰 구조체에 넣는 것이다.
예를 들어 UI용 좌표값, 스케줄링 우선순위, 실제 파일 경로, K8s Job 이름이 모두 같은
struct에 들어가면:

- 어느 단계에서 어떤 필드를 채워야 하는지 알 수 없다
- 아직 확정되지 않은 필드가 nil/empty로 돌아다닌다
- 파이프라인 로직만 바꿔도 리소스 설정이 함께 버전업된다
- 재현성 기록에 불필요한 정보가 섞인다

**문제 2: 분석 로직과 데이터가 섞임**

이전 PoC(PIPELINE_LITE_SPEC.md)에서는 노드의 command에 파일 경로를 직접 썼다:
```sh
bwa mem /data/cohort1/sample1_R1.fastq.gz ...
```

이렇게 하면 같은 분석 로직을 다른 데이터로 실행할 때마다 command를 수정해야 한다.
분석 로직(bwa mem)과 데이터(파일 경로)가 섞여 있어 재사용과 재현성이 깨진다.

**이 문서의 해결 방식:**
- 단계별로 명시적인 표현을 두고 각 단계의 책임을 고정한다
- 분석 로직(스크립트)과 데이터(파일 경로)를 명시적으로 다른 단계에서 결합한다
- "편집 원본"과 "파생 표현"을 구분한다

### 1.2 이전 문서(PIPELINE_LITE_SPEC.md)와의 관계

PIPELINE_LITE_SPEC.md는 10일 PoC 검증을 위한 최소 단순화 명세였다. 의도적으로
생략한 것들이 있었고, 그 한계를 문서 자체에서 명시했다.

이 문서는 PoC에서 검증된 실행 체인(DAG + K8s Jobs + Kueue)을 기반으로,
실제 시스템 설계로 진입하기 위한 표현 모델을 정의한다.

| 항목 | PIPELINE_LITE_SPEC.md | 이 문서 |
|------|----------------------|---------|
| 리소스 설정 위치 | 노드에 inline | 독립 엔티티(ResourceProfile)로 분리 |
| 파일 경로 처리 | command에 직접 기술 | 논리 변수(${R1})로 작성, 단계적 치환 |
| 단계 구분 | 없음 | 4단계 명시적 분리 |
| 재현성 | 미정의 | BoundPipeline을 기준선으로 명시 |
| 설계 목적 | PoC 동작 검증 | 실제 시스템 설계 기준선 |

---

## 2. 배경: 이 시스템이 다루는 문제

### 2.1 사용자와 시스템 구성

이 시스템의 주요 사용자는 **bioinformatics 연구자 / 기술 지원을 받는 의사**다.
이들은 프로그래머가 아니지만, 분석 파이프라인의 구조와 데이터를 이해한다.

시스템 구성:
- **UI**: 사용자가 DAG 형태로 파이프라인을 작성하고, 분석할 데이터를 선택한다
- **tori**: 파일 시스템을 모니터링하고, 파일들을 분류하여 클라이언트에 제공한다
- **pipeline service**: 사용자의 파이프라인과 데이터를 결합하여 실행 가능한 형태로 만든다
- **scheduler**: 어떤 자원으로, 어느 큐로 보낼지 결정한다
- **K8s / Kueue**: 실제 Job을 실행하고 자원을 관리한다

### 2.2 tori란

tori는 분석에 사용할 **파일 데이터를 관리하고 분류하는 서비스**다.

예를 들어 Illumina 시퀀서에서 나온 파일들의 이름은 다음과 같은 패턴을 가진다:
```
sample1_S1_L001_R1_001.fastq.gz   ← sample1의 forward read
sample1_S1_L001_R2_001.fastq.gz   ← sample1의 reverse read
sample2_S1_L001_R1_001.fastq.gz   ← sample2의 forward read
sample2_S1_L001_R2_001.fastq.gz   ← sample2의 reverse read
```

tori는 이 파일들을 **rule.json**에 정의된 규칙으로 파싱하여 **FileBlock**이라는
2D 행렬로 구조화한다:

```
FileBlock (/data/cohort1/):
  column_headers: ["R1", "R2"]

  Row 0: { R1: "sample1_R1.fastq.gz", R2: "sample1_R2.fastq.gz" }
  Row 1: { R1: "sample2_R1.fastq.gz", R2: "sample2_R2.fastq.gz" }
  Row 2: { R1: "sample3_R1.fastq.gz", R2: "sample3_R2.fastq.gz" }
```

- **column_headers** = 파이프라인 스크립트에서 사용하는 **변수명**
- **각 Row** = 하나의 샘플(분석 단위). Row가 3개면 3개의 병렬 분석이 실행된다

이 FileBlock을 클라이언트(UI)에 gRPC로 제공하여 사용자가 선택할 수 있게 한다.

### 2.3 파이프라인 작성 방식

사용자는 UI에서 DAG 형태로 파이프라인을 작성한다. 각 노드의 스크립트에는
tori FileBlock의 column header와 동일한 이름의 **논리 변수**를 사용한다:

```sh
# worker 노드의 스크립트 예시
bwa mem /ref/hg38.fa ${R1} ${R2} > /output/${SAMPLE_ID}/aligned.sam
```

여기서 `${R1}`, `${R2}`는 나중에 실제 파일 경로로 치환될 **자리표시자**다.
사용자는 데이터 경로를 직접 쓰지 않고, FileBlock의 header명을 변수처럼 사용한다.

---

## 3. 전체 흐름

### 3.1 단계 흐름

```
사용자 (UI)
  │
  │  1. 파이프라인 작성 (DAG + shell script + ${변수})
  │  2. FileBlock 선택 (tori에서 받은 목록 중 하나)
  │  3. ResourceProfile 선택 (노드별 CPU/메모리 설정)
  │
  ▼
[ AuthoredPipeline ]
  - 분석 의도 + DAG + script template
  - 아직 실제 데이터 없음
  │
  │  { pipeline_spec + fileblock_id + resource_profile_id }
  ▼
[ tori: semantic resolution ]
  - fileblock_id로 FileBlock 조회
  - ${R1} → 실제 파일 경로 매핑 계산
  - provenance 기록 (어느 snapshot인지)
  │
  ▼
[ pipeline service: BoundPipeline 조립 ]
  - Authored + tori resolution 결합
  - fan-out 수 확정 (= FileBlock row 수)
  - script는 아직 template 그대로 유지
  │
  ▼
[ BoundPipeline ]
  - 어떤 데이터로 실행하는지 확정됨
  - 재현성 기준선
  │
  ▼
[ ScheduledPipeline ]
  - queue / priority / retry 결정
  - ResourceProfile 적용
  - heavy 여부 판단
  │
  ▼
[ ExecutablePipeline ]
  - text substitution: ${R1} → 실제 경로로 script에 삽입
  - mount/env/path 최종 확정
  - runId / K8s Job 이름 발급
  │
  ▼
K8s Jobs → Kueue → 실행
```

### 3.2 왜 Bound가 Scheduled보다 먼저인가

scheduling을 결정하려면 다음을 알아야 한다:
- **worker가 몇 개나 생기는가?** (fan-out 수 = FileBlock row 수)
- **입력 데이터 규모가 어느 정도인가?** (heavy 여부 판단)
- **실제로 몇 개의 K8s Job이 제출되는가?** (자원 계획)

이 정보는 AuthoredPipeline만으로는 알 수 없다.
FileBlock이 붙어야(= Bound가 완성되어야) 비로소 생긴다.

따라서 **Bound → Scheduled 순서**가 필수다.

반대로 하면 (Scheduled → Bound): 스케줄러가 "worker 몇 개에 자원을 줄지"를
모르는 채로 결정을 내려야 한다. 불가능하다.

### 3.3 전제 조건

> **모든 파이프라인은 데이터가 붙어야 실행 가능하다.**

BoundPipeline이 완성되지 않은 파이프라인은 Scheduled로 넘어갈 수 없다.
scheduler는 항상 BoundPipeline만 입력으로 받는다.

---

## 4. 공통 원칙

### 4.1 source of truth — "편집 원본은 하나"

- 편집 가능한 유일한 원천은 **AuthoredPipeline**이다
- BoundPipeline / ScheduledPipeline / ExecutablePipeline은 모두 **파생 표현**이다
- **변환은 단방향 함수**다: Authored에서 Bound를 만들 수 있지만, Bound를 편집해서
  Authored로 역전파하지 않는다
- 파생 표현을 직접 편집하면 안 된다. 수정이 필요하면 Authored를 수정하고 다시 변환한다

### 4.2 semantic substitution vs text substitution

이 두 가지를 구분하는 것이 이 설계의 핵심이다.

**semantic substitution (의미 해석)**
> "`${R1}`이 무엇을 의미하는가?"
> → "Row 0의 R1 컬럼, 즉 `/data/cohort1/sample1_R1.fastq.gz`"

이것은 **tori**가 답을 안다. FileBlock의 어느 snapshot, 어느 row, 어느 header인지를
알고 있기 때문이다.

**text substitution (문자열 삽입)**
> "그 값을 shell script 텍스트 안에 어떻게 집어넣는가?"
> → `bwa mem /ref/hg38.fa ${R1}` → `bwa mem /ref/hg38.fa /data/cohort1/sample1_R1.fastq.gz`

이것은 **Executable 단계**에서만 한다.

**왜 두 단계로 나누는가?**

text substitution을 늦게 하면:
1. Bound가 가벼워진다. fan-out이 100개여도 script를 100번 복사하지 않아도 된다
2. 스토리지 백엔드가 바뀌어도 (PVC → S3 → CAS) Bound는 그대로 유지된다. Executable만 달라진다
3. 재현성 기록이 깔끔하다. "무엇이 선택됐는가"(Bound)와 "어떻게 실행됐는가"(Executable)가 분리된다

### 4.3 tori의 역할 경계

tori는 파일 데이터의 **semantic resolution 정답 원천**이다.

**tori가 담당하는 것:**
- FileBlock/DataBlock 생성 및 클라이언트 제공
- 변수명(header)에 대응하는 실제 파일 경로 제공
- 어느 snapshot의 어느 row인지 provenance 기록

**tori가 담당하지 않는 것:**
- 파이프라인 topology 조립 (pipeline service 역할)
- script 문자열 치환 (Executable 단계 역할)
- 스케줄링 결정 (scheduler 역할)

tori는 "파일이 어디에 있는지"를 알지만, "그 파일을 어떻게 파이프라인에 넣을지"는
pipeline service가 결정한다.

### 4.4 1 FileBlock = 1 run 계약

- **하나의 FileBlock**은 **하나의 pipeline run**에만 대응한다
- 여러 FileBlock을 합쳐 하나의 run을 만들지 않는다
- FileBlock은 분석 입력의 **최소 단위이자 최대 단위**다

이 계약의 근거:
FileBlock은 하나의 코호트/데이터셋이다. 유전체 분석은 하나의 데이터셋 단위로 실행된다.
여러 코호트를 합치는 것은 분석의 의미가 달라지는 별도의 설계 문제다.

멀티 FileBlock 결합은 "아직 미설계"가 아니라 **의도적 제외**다.

### 4.5 재현성 기준선

**재현성(reproducibility)**은 이 시스템에서 최상위 요구사항이다.
같은 파이프라인 + 같은 데이터 = 같은 결과가 보장되어야 한다.

재현의 기준선은 **BoundPipeline**이다.

재현에 필요한 최소 정보:
```
pipeline_id + pipeline_version        → 동일한 분석 로직
resource_profile_id                   → 동일한 리소스 설정
fileblock_id + fileblock_updated_at   → 동일한 데이터
```

이 세 가지가 동일하면 BoundPipeline을 재생성할 수 있고, 동일한 ExecutablePipeline이 나온다.

**재현 시나리오:**
```
과거 실행 기록 조회
  → pipeline_id, pipeline_version, resource_profile_id,
    fileblock_id, fileblock_updated_at 확인
  → tori.ResolveBindings() 재호출 (동일 snapshot 기준)
  → pipeline service: BoundPipeline 재생성
  → 새 runId만 발급해서 재실행
  → 동일한 분석 결과 보장
```

운영 행태까지 재현하려면 Bound + Scheduled를 함께 기록한다.

### 4.6 node identity 연속성

파이프라인의 각 노드는 AuthoredPipeline에서 발급된 `nodeId`를 가진다.
이 ID는 모든 단계에서 변하지 않는다.

fan-out으로 노드가 분화될 때:
```
nodeId: "worker" (Authored에서 하나)
  →  "worker[0]", "worker[1]", "worker[2]" (Bound에서 row 수만큼 인스턴스화)
```

K8s Job 이름: `i-{runId}-{nodeId}-{rowIndex}`
예: `i-run-20260404-001-worker-0`

어느 단계에서든 원본 nodeId(`worker`)로 역추적 가능해야 한다.

---

## 5. 핵심 엔티티

### 5.1 ResourceProfile (독립 엔티티)

ResourceProfile은 파이프라인 로직과 **분리된 독립 엔티티**다.

**왜 분리하는가?**

파이프라인의 분석 로직(`bwa mem ...`)과 그것을 실행하는 데 필요한 자원(CPU, 메모리)은
변화 주기가 다르다.

```
분석 로직 (파이프라인):  변화 주기 느림 — 검증 후 안정화
리소스 설정:             변화 주기 빠름 — 데이터 크기/클러스터 상황에 따라 자주 조정
```

같은 struct에 묶으면:
- 리소스 설정만 바꿔도 파이프라인 버전이 올라간다 → 불필요한 버전 증가
- "이번 실패가 분석 로직 문제인가, 리소스 부족 문제인가"를 이력으로 구분할 수 없다

분리하면:
- 파이프라인 버전과 리소스 이력을 독립적으로 추적 가능
- 같은 파이프라인을 다른 클러스터/다른 비용 정책으로 실행할 때 로직을 건드리지 않아도 됨

```yaml
ResourceProfile:
  id:          "wgs-standard-2026-B"
  description: "WGS alignment 표준 설정 — 샘플당 8Gi (2026-04 기준)"
  pipeline_id: "wgs-alignment-v1"   # 권장 연결 파이프라인 (강제 아님)
  created_at:  "2026-04-01"
  node_resources:
    prepare: { cpu: "100m", memory: "128Mi" }
    worker:  { cpu: "4",    memory: "8Gi"   }
    collect: { cpu: "2",    memory: "16Gi"  }
```

ResourceProfile 이력 예시:
```
wgs-alignment-v1 파이프라인에 적용된 ResourceProfile 이력:
  rp-wgs-2026-A (2026-03-01): 초기 설정, worker 4Gi
  rp-wgs-2026-B (2026-04-01): 샘플 수 증가 → worker 8Gi로 증가
  rp-wgs-2026-C (2026-04-04): 신규 클러스터 이전 → CPU 조정
```

분석 로직(`wgs-alignment-v1`)은 하나인데, 리소스 이력은 독립적으로 쌓인다.

### 5.2 파이프라인 상태 머신

파이프라인은 안정성 수준에 따라 세 가지 상태를 가진다.

```
draft ──→ testing ──→ stable
            │
            └──→ draft  (검증 실패 시 롤백)
```

| 상태 | 의미 | 실패 발생 시 해석 방향 |
|------|------|---------------------|
| `draft` | 최초 작성, 아직 실행 미검증 | 파이프라인 로직/설정 오류 가능성 높음 |
| `testing` | 검증 중, 반복 실행으로 안정성 확인 중 | 로직 또는 데이터 문제 혼재 가능 |
| `stable` | 반복 검증 완료, 신뢰할 수 있는 상태 | **데이터/환경/리소스 문제일 가능성 높음** |

**이 상태 구분이 중요한 이유:**

stable 파이프라인은 이미 여러 번 정상 실행된 것이다. 여기서 갑자기 실패가 나면
파이프라인 코드보다 **입력 데이터 품질 문제, 리소스 부족, 클러스터 이상**을 먼저 의심해야 한다.
이 판단 기준을 코드가 아닌 **파이프라인 상태**로 명시하는 것이 목적이다.

상태 전이 조건:
- `draft → testing`: 최초 실행 성공
- `testing → stable`: 운영 정책으로 정의한 N회 이상 반복 실행 성공
- `stable → testing`: 분석 로직 수정 또는 명시적 롤백 요청

### 5.3 FileBlock (tori 제공)

FileBlock은 tori가 생성하고 관리한다. 여기서는 파이프라인과의 관계만 정의한다.

```
FileBlock (/data/cohort1/):
  block_id:        "/data/cohort1/"         # 디렉터리 경로 (run과 독립)
  updated_at:      "2026-04-04T10:00:00Z"   # 재현성 기록에 사용
  column_headers:  ["R1", "R2"]             # → 파이프라인 스크립트의 변수명
  rows:
    Row 0: { "R1": "sample1_R1.fastq.gz", "R2": "sample1_R2.fastq.gz" }
    Row 1: { "R1": "sample2_R1.fastq.gz", "R2": "sample2_R2.fastq.gz" }
    Row 2: { "R1": "sample3_R1.fastq.gz", "R2": "sample3_R2.fastq.gz" }
```

파이프라인과의 연결 규칙:
- `column_headers` ["R1", "R2"] = Authored script의 `${R1}`, `${R2}` 변수와 직접 매핑
- **각 Row = 하나의 worker 노드 실행 단위** (병렬 실행의 단위)
- **Row 수 = BoundPipeline에서 확정되는 fan-out worker 수**
  - 위 예시에서 Row가 3개 → worker 노드 3개가 병렬 실행됨

실제 파일 경로 재구성: `block_id + "/" + row.cells[header]`
예: `/data/cohort1/` + `sample1_R1.fastq.gz` = `/data/cohort1/sample1_R1.fastq.gz`

---

## 6. Stage별 정의

### 6.1 AuthoredPipeline

**한 줄 정의**: 사용자가 작성/편집하는 **분석 원본**. 분석 의도, DAG 구조, script template를 담는다.

**책임**: 분석 로직과 노드 구성을 정의한다. 실제 데이터 경로는 없고, 논리 변수만 있다.

**구조 예시:**

```yaml
AuthoredPipeline:
  # 식별 정보
  pipeline_id:      "wgs-alignment-v1"
  pipeline_version: 1
  name:             "WGS Alignment Pipeline"
  description:      "BWA + samtools 기반 WGS alignment. hg38 reference 사용."
  state:            draft          # draft | testing | stable

  # DAG 구조
  # prepare → worker → collect 순서로 실행
  # worker는 FileBlock row 수만큼 병렬로 실행됨 (fan-out)
  nodes:
    - node_id:    "prepare"
      description: "출력 디렉터리 준비"
      script:     "mkdir -p /output/${SAMPLE_ID}"
      depends_on: []

    - node_id:    "worker"
      description: "BWA mem으로 reference에 alignment"
      script:     "bwa mem /ref/hg38.fa ${R1} ${R2} > /output/${SAMPLE_ID}/aligned.sam"
      # ${R1}, ${R2}   : FileBlock의 R1, R2 컬럼에서 올 실제 파일 경로 (나중에 치환)
      # ${SAMPLE_ID}   : 샘플 식별자 (Row에서 파생)
      depends_on: ["prepare"]

    - node_id:    "collect"
      description: "aligned.sam을 정렬하여 bam 생성"
      script:     "samtools sort /output/${SAMPLE_ID}/aligned.sam -o /output/${SAMPLE_ID}/sorted.bam"
      depends_on: ["worker"]
```

**반드시 포함해야 할 것:**

| 항목 | 설명 |
|------|------|
| pipeline_id / pipeline_version | 파이프라인 고유 식별자와 버전 |
| state | draft / testing / stable |
| nodes (nodeId, script, depends_on) | DAG 구조와 분석 로직 |
| script 내 논리 변수 (`${R1}` 등) | FileBlock header와 매핑될 자리표시자 |

**들어가면 안 되는 것:**

| 금지 항목 | 왜 금지인가 |
|----------|-----------|
| 실제 파일 경로 (`/data/cohort1/...`) | 데이터는 Bound 단계에서 붙는다 |
| fileblock_id / snapshot 식별자 | Bound 단계 |
| resolved bindings | Bound 단계 |
| resourceProfileRef | Scheduled 단계 |
| queue / priority / retry 설정 | Scheduled 단계 |
| runId / K8s Job 이름 | Executable 단계 |
| rendered command (치환 완료 script) | Executable 단계 |
| UI 레이아웃 (x,y 좌표, 그룹 등) | UI 레이어 전용, 실행 계열에는 불필요 |

**validation (다음 단계로 넘어가기 전 검사):**
- DAG에 cycle(순환 참조)이 없는지
- depends_on에서 참조하는 nodeId가 실제로 존재하는지
- script template 문법 최소 검증
- 같은 nodeId가 중복으로 정의되지 않았는지

---

### 6.2 BoundPipeline

**한 줄 정의**: 실제 입력 데이터가 붙어 **"이번 실행에 어떤 데이터를 쓰는가"가 확정된** 표현.

**책임**: Authored의 논리 변수를 실제 값에 매핑한다. fan-out 수를 확정한다. script는 아직 template 그대로다.

**생성 과정:**
```
사용자가 fileblock_id 선택
  → tori.ResolveBindings(fileblock_id) 호출
  → tori: FileBlock 조회, header → path 매핑 계산, provenance 기록
  → pipeline service: Authored + tori 결과 결합 → BoundPipeline 조립
```

**구조 예시:**

위의 Authored에서 `fileblock_id: "/data/cohort1/"` 선택 시:

```yaml
BoundPipeline:
  # 원본 참조 (어떤 파이프라인을 바탕으로 만들었는가)
  source_pipeline_id:      "wgs-alignment-v1"
  source_pipeline_version: 1

  # binding provenance (어떤 데이터를 선택했는가, 재현성의 핵심)
  fileblock_id:         "/data/cohort1/"
  fileblock_updated_at: "2026-04-04T10:00:00Z"   # ← 이 시점의 snapshot 기준
  bound_at:             "2026-04-04T11:00:00Z"

  # fan-out 확정: FileBlock에 row가 3개 → worker 3개 병렬 실행
  fanout_cardinality: 3

  # instance별 resolved bindings
  # 각 instance = FileBlock의 하나의 row = 하나의 샘플
  instances:
    - instance_id: "worker[0]"      # nodeId[rowIndex] 형식
      row_index:   0
      bindings:
        R1:
          resolved_path: "/data/cohort1/sample1_R1.fastq.gz"
          source_header: "R1"
          row_index:     0
          # 나중에 artifactRef 확장 가능 (현재는 path 기반)
        R2:
          resolved_path: "/data/cohort1/sample1_R2.fastq.gz"
          source_header: "R2"
          row_index:     0
        SAMPLE_ID:
          resolved_value: "sample1"   # 파일명이 아닌 문자열 값

    - instance_id: "worker[1]"
      row_index:   1
      bindings:
        R1: { resolved_path: "/data/cohort1/sample2_R1.fastq.gz", ... }
        R2: { resolved_path: "/data/cohort1/sample2_R2.fastq.gz", ... }
        SAMPLE_ID: { resolved_value: "sample2" }

    - instance_id: "worker[2]"
      row_index:   2
      bindings:
        R1: { resolved_path: "/data/cohort1/sample3_R1.fastq.gz", ... }
        R2: { resolved_path: "/data/cohort1/sample3_R2.fastq.gz", ... }
        SAMPLE_ID: { resolved_value: "sample3" }

  # script는 여전히 template 그대로 유지 (text substitution 아직 안 함)
  script_templates:
    prepare: "mkdir -p /output/${SAMPLE_ID}"
    worker:  "bwa mem /ref/hg38.fa ${R1} ${R2} > /output/${SAMPLE_ID}/aligned.sam"
    collect: "samtools sort /output/${SAMPLE_ID}/aligned.sam ..."

  # scheduler가 결정에 참고할 힌트
  scheduling_hints:
    fanout_count:      3
    total_input_files: 6
```

**현재 미해소 변수 처리 규칙:**

script에 `${REF}` 같이 FileBlock header에 없는 변수가 있을 수 있다.

```
현재 규칙:
- FileBlock header에 있는 변수 (${R1}, ${R2}): Bound에서 해소됨
- header에 없는 변수 (${REF} 등): 현재 미지원
  → Bound validation 시 "unresolved variable" 목록으로 반환
  → Executable에서도 치환이 안 되면 최종 validation 실패
  → reference 데이터 바인딩은 후속 설계 단계 (Section 10 참조)
```

**반드시 포함해야 할 것:**

| 항목 | 설명 |
|------|------|
| source_pipeline_id / version | 어떤 파이프라인 기반인지 |
| fileblock_id + fileblock_updated_at | 어떤 데이터를 선택했는지 (재현성) |
| bound_at | 바인딩 시점 기록 |
| fanout_cardinality | worker 수 확정값 |
| instances (instance_id, row_index, bindings) | 각 worker의 resolved 값 |
| script_templates | template 그대로 유지 (치환 전) |
| scheduling_hints | scheduler 결정 근거 |

**들어가면 안 되는 것:**

| 금지 항목 | 왜 금지인가 |
|----------|-----------|
| resourceProfileRef 최종 선택 | Scheduled 단계 |
| queue / priority / retry | Scheduled 단계 |
| rendered command (치환 완료 script) | Executable 단계 |
| runId / K8s Job 이름 | Executable 단계 |
| runtime-specific mount path | Executable 단계 |

**핵심 불변식:**
- semantic substitution은 완료, **text substitution은 아직 아니다**
- script는 template 그대로 유지된다
- **이 단계의 snapshot이 재현성 기준선이다**

**validation:**
- script에 필요한 변수가 모두 resolved bindings에 있는지
- FileBlock/header/row 선택이 유효한지
- duplicate collision (tori Phase A-2 관련) 없는지
- unresolved variable 목록 반환

---

### 6.3 ScheduledPipeline

**한 줄 정의**: BoundPipeline을 바탕으로 **"어떻게 실행할 것인가"에 대한 운영 결정**이 추가된 표현.

**책임**: queue 배정, 우선순위, retry 정책, 자원 할당을 결정한다. 분석 로직과 데이터는 건드리지 않는다.

scheduler는 BoundPipeline의 `scheduling_hints`(fan-out 수, 입력 파일 수 등)를 보고
운영 결정을 내린다.

**구조 예시:**

```yaml
ScheduledPipeline:
  # BoundPipeline 참조
  bound_pipeline_ref: "bound-20260404-wgs-cohort1"

  # 스케줄링 결정
  priority:      normal        # low | normal | high
  queue:         "dna-default" # Kueue LocalQueue 이름
  retry_policy:  none          # none | retry-deprioritized | retry-immediate

  # 자원 결정 (ResourceProfile에서 가져옴)
  resource_profile_id: "wgs-standard-2026-B"
  node_resources:       # ResourceProfile에서 복사 (실행 시점 고정)
    prepare: { cpu: "100m", memory: "128Mi" }
    worker:  { cpu: "4",    memory: "8Gi"   }
    collect: { cpu: "2",    memory: "16Gi"  }

  # 운영 정책
  heavy_classification: false   # heavy job 분류 여부
  concurrency_cap:      3       # 동시 실행 worker 수 제한
  admission_class:      "poc-standard-lq"  # Kueue admission

  # 스케줄링 메타데이터
  scheduled_at:      "2026-04-04T11:05:00Z"
  scheduler_version: "v0.1"
```

**반드시 포함해야 할 것:**

| 항목 | 설명 |
|------|------|
| bound_pipeline_ref | 어떤 BoundPipeline 기반인지 |
| priority / queue / retry_policy | 스케줄링 핵심 결정 |
| resource_profile_id + node_resources | 자원 할당 (이 시점에 고정) |
| heavy_classification / concurrency_cap | 운영 정책 |
| scheduled_at | 스케줄링 시점 기록 |

**들어가면 안 되는 것:**

| 금지 항목 | 왜 금지인가 |
|----------|-----------|
| rendered command | Executable 단계 |
| mount path / env 구체값 | Executable 단계 |
| runId / K8s Job 이름 | Executable 단계 |
| 분석 로직 변경 | Authored만 편집 가능 |
| bound data 변경 | BoundPipeline만 변경 가능 |

**핵심 불변식:**
- 분석 로직은 바꾸지 않는다
- 실제 데이터를 바꾸지 않는다
- Bound를 입력으로 운영 결정만 추가한다

**validation:**
- resource profile이 모든 노드에 대응되는지
- queue/priority/retry가 운영 정책 범위 안에 있는지
- concurrency cap과 fan-out cardinality가 일관되는지

---

### 6.4 ExecutablePipeline

**한 줄 정의**: K8s가 바로 실행할 수 있는 **최종 형태**. 이 단계에서만 text substitution과 runtime-specific 정보 생성이 일어난다.

**책임**: script의 변수를 실제 값으로 치환하고, mount/env/path를 확정하고, K8s Job spec을 생성한다.

**구조 예시 (worker[0] 인스턴스):**

```yaml
ExecutablePipeline:
  # 실행 식별자
  run_id: "run-20260404-001"

  # worker[0] 인스턴스의 최종 실행 스펙
  instances:
    - instance_id: "worker[0]"
      job_name:    "i-run-20260404-001-worker-0"  # K8s Job 이름

      # text substitution 완료된 최종 command
      # ${R1} → /in/R1.fastq.gz (mount된 경로)
      # ${R2} → /in/R2.fastq.gz (mount된 경로)
      # ${SAMPLE_ID} → sample1
      rendered_command: >
        bwa mem /ref/hg38.fa /in/R1.fastq.gz /in/R2.fastq.gz
        > /output/sample1/aligned.sam

      # mount mapping: 원본 경로 → container 내부 경로
      # rendered_command는 container 내부 경로(/in/R1.fastq.gz)를 사용
      mounts:
        - src: "/data/cohort1/sample1_R1.fastq.gz"
          dst: "/in/R1.fastq.gz"
        - src: "/data/cohort1/sample1_R2.fastq.gz"
          dst: "/in/R2.fastq.gz"

      # 환경변수로 주입
      env:
        SAMPLE_ID: "sample1"

      # K8s Job spec
      k8s_job_spec:
        image:         "biocontainers/bwa:0.7.17"
        resources:
          requests: { cpu: "4", memory: "8Gi" }
          limits:   { cpu: "4", memory: "8Gi" }
        backoff_limit: 0          # fast-fail: 실패 시 재시도 없음, 즉시 자원 반환
        queue_label:   "poc-standard-lq"
        restart_policy: Never

  # 실행 추적
  created_at:    "2026-04-04T11:10:00Z"
  scheduled_ref: "scheduled-20260404-wgs-cohort1"
```

**핵심 불변식:**
- **text substitution은 이 단계에서만** 수행한다
- runtime-specific 정보(path, env, mount)는 여기서 처음 확정된다
- storage backend가 바뀌어도(PVC → S3) Bound는 그대로, **Executable만 달라진다**
- `backoff_limit: 0`: fast-fail 원칙 — Job 실패 시 재시도 없이 즉시 Kueue에 자원 반환

**validation:**
- 모든 `${변수}`가 실제 값으로 치환됐는지
- mount/env/args가 runtime 규칙에 맞는지
- runId/jobName 중복이 없는지
- K8s Job spec이 제출 가능한 형태인지

---

## 7. 필드 소유권 표

각 필드가 어느 stage에서 처음 생기고 canonical하게 관리되는지를 고정한다.

`✅` = 이 단계에서 생성/소유  
`유지` = 이전 단계에서 가져와 참조  
`참조` = ID/key만 참조 (내용은 원본 단계 소유)  
`❌` = 이 단계에 존재하면 안 됨

| 필드/개념 | Authored | Bound | Scheduled | Executable | 비고 |
|----------|:--------:|:-----:|:---------:|:----------:|------|
| pipelineId / version | **✅** | 참조 | 참조 | 참조 | 원본 식별자 |
| pipeline state | **✅** | 참조 | 참조 | — | draft/testing/stable |
| nodeId / edge 구조 | **✅** | 유지 | 유지 | 유지 | 전 단계 불변 |
| script template | **✅** | 유지 | 유지 | 참조 | rendered 아님 |
| 논리 변수 (`${R1}`) | **✅** | 해석 완료 | 유지 | 참조 | semantic resolution |
| UI 레이아웃 정보 | UI 전용 | ❌ | ❌ | ❌ | 실행 계열 진입 금지 |
| fileblock_id / snapshot ref | ❌ | **✅** | 유지 | 유지 | 재현성 핵심 |
| resolved bindings | ❌ | **✅** | 유지 | 참조 | path + provenance |
| fan-out cardinality | 논리 규칙 | **✅** 확정 | 유지 | 유지 | Bound에서 구체화 |
| instance_id 목록 | ❌ | **✅** | 유지 | 유지 | row → instance |
| scheduling_hints | ❌ | **✅** 생성 | 소비 | — | 스케줄러 결정 근거 |
| resourceProfileRef | ❌ | ❌ | **✅** | 유지 | 운영 결정 |
| queue / priority / retry | ❌ | ❌ | **✅** | 유지 | 운영 결정 |
| heavy classification | ❌ | 근거만 | **✅** 결정 | 유지 | |
| rendered command/script | ❌ | ❌ | ❌ | **✅** | text substitution 결과 |
| mount path / env injection | ❌ | ❌ | ❌ | **✅** | runtime lowering |
| runId / jobName / podName | ❌ | ❌ | ❌ | **✅** | 실행 식별자 |

---

## 8. 변환 계약

각 단계 간 변환의 입력/출력/규칙을 고정한다.

### 8.1 Authored → Bound

```
입력:
  AuthoredPipeline
  fileblock_id (사용자가 UI에서 선택)
  tori semantic resolution 결과 (header → resolved path 매핑)

출력:
  BoundPipeline

규칙:
  1. 논리 변수를 resolved binding으로 매핑한다
     ${R1} → { resolved_path: "/data/cohort1/sample1_R1.fastq.gz", ... }
  2. script는 template 그대로 유지한다 (text substitution 안 함)
  3. fan-out cardinality를 확정한다 (FileBlock row 수)
  4. provenance를 반드시 기록한다 (fileblock_id + fileblock_updated_at)
  5. unresolved variable이 있으면 BoundPipeline 생성 실패, 오류 반환
```

### 8.2 Bound → Scheduled

```
입력:
  BoundPipeline
  scheduler policy (운영 팀이 설정)
  cluster/runtime 운영 정책
  ResourceProfile (사용자 선택 또는 pipeline 권장값)

출력:
  ScheduledPipeline

규칙:
  1. priority / queue / retry / resourceProfile을 결정한다
  2. heavy classification과 concurrency cap을 적용한다
  3. 분석 로직(script template)은 바꾸지 않는다
  4. bound input(파일 경로, provenance)은 바꾸지 않는다
```

### 8.3 Scheduled → Executable

```
입력:
  ScheduledPipeline
  runtime lowering 규칙 (어떻게 mount/env로 변환할지)
  실행 시점 metadata (runId 발급 등)

출력:
  ExecutablePipeline

규칙:
  1. text substitution 수행: ${변수} → 실제 값으로 script에 삽입
  2. mount/env/path concretization: resolved_path를 container 내부 경로로 변환
  3. K8s Job spec 최종 생성
  4. runId / jobName 발급 (형식: i-{runId}-{nodeId}-{rowIndex})
  5. backoff_limit: 0 적용 (fast-fail 원칙)
```

---

## 9. 컴포넌트 책임 분리

### 역할 요약

| 컴포넌트 | 핵심 역할 | 담당하지 않는 것 |
|---------|----------|----------------|
| **tori** | 파일 분류 + FileBlock 제공 + binding key의 semantic resolution | script 조립, text substitution, 스케줄링 결정 |
| **pipeline service** | Authored 수용, tori 결과 수신, BoundPipeline 조립, fan-out instance 구성 | 스케줄링 결정, K8s 직접 제출 |
| **scheduler** | Bound 기반 운영 결정 (queue/priority/retry/resource), ScheduledPipeline 생성 | 분석 로직 변경, 데이터 조회 |
| **executable builder** | text substitution, mount/env 확정, K8s Job spec 생성, runId 발급 | 데이터/정책 결정 |
| **K8s / Kueue** | Job 실행, 자원 관리, admission control | — |

### tori API 현재 구현 상태

| API | 상태 | 설명 |
|-----|------|------|
| `FetchDataBlock` | ✅ 구현됨 | 클라이언트에 DataBlock 제공 (타임스탬프 기반 증분) |
| `ResolveBindings` (또는 `BindPipeline`) | ❌ **미구현** | 이 spec이 설계 기준선 |

`ResolveBindings` API가 구현되면:
```
요청: { fileblock_id, variable_names: ["R1", "R2", "SAMPLE_ID"] }
응답: { bindings: { R1: [...], R2: [...], SAMPLE_ID: [...] }, provenance: {...} }
```

---

## 10. 아직 열어둘 항목 (의도적 미결)

이 문서는 v0.1 기준선이다. 아래는 후속 단계에서 설계할 항목이다.

| 항목 | 현재 처리 방식 | 후속 설계 방향 |
|------|-------------|-------------|
| reference 데이터 (`${REF}` 등) | unresolved로 처리, validation 실패 | 별도 reference catalog 또는 파이프라인에 고정값으로 관리 |
| artifactRef / CAS / S3 / ORAS | resolved path (PVC 경로) 사용 | Bound의 binding value를 URI 추상화로 확장 |
| Scheduled의 fair-share / reserved slot | 기본 FIFO | scheduler 정책 설계 단계 |
| state 전이 N 횟수 (testing → stable 조건) | 미정 | 운영 정책으로 결정 |
| 파이프라인 버전 관리 전략 | 정수 버전 | semantic versioning 또는 content hash 검토 |

**멀티 FileBlock 결합**은 "미설계"가 아니라 **의도적 제외**다.
현재 1 FileBlock = 1 run 계약은 설계 원칙이며, 변경 시 별도 결정이 필요하다.

---

## 11. 현재 구현 상태

| 항목 | 상태 | 관련 컴포넌트 |
|------|:----:|-------------|
| FileBlock 생성 및 FetchDataBlock gRPC | ✅ | tori |
| K8s Job 실행 체인 (DAG + Kueue) | ✅ | spawner + cmd/ingress |
| fast-fail (backoff_limit: 0) | ✅ | spawner |
| concurrent drain (semaphore) | ✅ | cmd/ingress |
| Redis Streams ingress (at-least-once) | ✅ | cmd/ingress + tori |
| AuthoredPipeline 포맷 정의 | ⚠️ 부분 | UI 연동 미정, PIPELINE_LITE_SPEC.md 기반 |
| ResourceProfile 독립 엔티티 | ❌ 미구현 | 현재 노드에 inline |
| tori ResolveBindings API | ❌ 미구현 | tori |
| BoundPipeline 조립 | ❌ 미구현 | pipeline service |
| 변수 치환 엔진 (text substitution) | ❌ 미구현 | executable builder |
| 파이프라인 state 관리 | ❌ 미구현 | pipeline service |
| 재현성 기록 저장소 | ❌ 미구현 | pipeline service |

---

## 12. PoC에서 검증된 것과 이 문서의 관계

PoC(PIPELINE_LITE_SPEC.md)에서 검증한 실행 체인은 이 문서의 **ExecutablePipeline 단계의 기반**으로 유지된다.

| PoC 검증 항목 | 이 문서에서의 위치 |
|-------------|-----------------|
| DAG fan-out / fan-in (prepare → workers → collect) | Authored → Executable 전 단계에 걸쳐 유지 |
| fast-fail (backoff_limit: 0) | Executable 단계 불변식 |
| Kueue admission control / quota | Scheduled → Executable |
| Redis Streams at-least-once | pipeline service 계층 |
| concurrent drain + semaphore | scheduler / pipeline service |
| K8s Job naming (i-{runId}-{nodeId}) | Executable, node identity continuity 계승 |

---

*이 문서는 PIPELINE_LITE_SPEC.md (2026-03-27)의 다음 버전이다.*
*PoC에서 검증된 실행 체인을 기반으로, 실제 시스템으로 진입하기 위한*
*파이프라인 표현 모델의 설계 기준선을 정의한다.*
*작성일: 2026-04-04*
