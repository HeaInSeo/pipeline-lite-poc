# PIPELINE_LITE_SPEC.md

> 작성일: 2026-03-27
> 대상: 10일 PoC (poc-pipeline)
> 참조 원본: caleb reference repo (pipeline_v11.jsonc, pipeline_v12.jsonc)

---

## 1. Pipeline-Lite란

### 1.1 개요

Pipeline-Lite는 caleb 프로젝트의 full pipeline specification에서 10일 PoC 검증에
필요한 최소 부분집합(minimal subset)만 추출하여 단순화한 경량 명세이다.

caleb의 full spec은 OverlayFS 기반 레이어드 스토리지, 복잡한 DSL binding semantics,
CAS/S3/ORAS 아티팩트 레지스트리, retries/resume 등 프로덕션 수준의 기능을 포함한다.
PoC에서는 이 기능들을 배제하고, DAG 실행 흐름 자체의 동작만 검증하는 데 집중한다.

### 1.2 목적

- DAG 의존성 해소(dependency resolution) 동작 확인
- fan-out(병렬 분기) / fan-in(결과 집계) 흐름 검증
- fast-fail: 상위 노드 실패 시 하위 노드 실행 차단 확인
- executionClass / resourceProfile 개념의 K8s Job 매핑 확인
- shared PVC를 통한 노드 간 데이터 핸드오프 확인

### 1.3 PoC 범위 밖 (Explicitly Out of Scope)

| 제외 기능 | 이유 |
|-----------|------|
| OverlayFS 레이어 스토리지 | K8s PVC로 대체, 10일 내 구현 불필요 |
| full DSL binding semantics | command 직접 지정으로 대체 |
| CAS / S3 / ORAS 아티팩트 | PVC 직접 읽기/쓰기로 대체 |
| retries / resume / checkpoint | PoC에서는 단순 실패 전파만 검증 |
| schemaVersion 버전 관리 | pipeline-lite/v1alpha1 단일 버전 고정 |
| 멀티 테넌트 / RBAC | 단일 네임스페이스 poc 고정 |

---

## 2. PoC용 DAG 구조

### 2.1 그래프 정의

```
A (ingest)
    |
    v
  [fan-out]
  /   |   \
B1  B2  B3   (parallel process shards)
  \   |   /
   [fan-in]
       |
       v
    C (report)
```

노드 설명:

| 노드 ID | 역할 | 의존 노드 | executionClass |
|---------|------|-----------|----------------|
| A | 데이터 준비 (ingest) | 없음 (entry point) | standard |
| B1 | 샤드 0 병렬 처리 | A | standard |
| B2 | 샤드 1 병렬 처리 | A | standard |
| B3 | 샤드 2 병렬 처리 | A | standard |
| C | 결과 집계 (report) | B1, B2, B3 | highmem |

### 2.2 실행 순서 (토폴로지 정렬 기준)

1. A 실행 시작 → 완료 대기
2. A 성공 시: B1, B2, B3 동시 실행 (fan-out)
3. B1, B2, B3 모두 성공 시: C 실행 (fan-in)
4. 어느 단계에서든 실패 발생 시: 하위 노드 전체 Skip (fast-fail)

### 2.3 데이터 흐름 (shared PVC 기준)

```
A       writes  /data/a-output/result.txt
B1      reads   /data/a-output/result.txt
        writes  /data/b-output/shard-0/result.txt
B2      reads   /data/a-output/result.txt
        writes  /data/b-output/shard-1/result.txt
B3      reads   /data/a-output/result.txt
        writes  /data/b-output/shard-2/result.txt
C       reads   /data/b-output/shard-{0,1,2}/result.txt
        writes  /data/c-output/report.txt
```

---

## 3. Pipeline-Lite 명세 구조 (YAML)

### 3.1 전체 예시

```yaml
apiVersion: pipeline-lite/v1alpha1
kind: Pipeline
metadata:
  name: poc-pipeline
  namespace: poc

spec:
  # executionClass 기본값 (노드에서 override 가능)
  executionClass: standard

  # 모든 노드가 공유하는 PVC 설정
  sharedPVC:
    claimName: poc-shared-pvc
    mountPath: /data

  nodes:
    - id: A
      image: busybox:1.36
      executionClass: standard
      resourceProfile: small
      command:
        - sh
        - -c
        - |
          mkdir -p /data/a-output
          echo "data from node A" > /data/a-output/result.txt
          echo "[A] ingest done"
          sleep 2

    - id: B1
      image: busybox:1.36
      executionClass: standard
      resourceProfile: small
      dependsOn: [A]
      command:
        - sh
        - -c
        - |
          mkdir -p /data/b-output/shard-0
          cat /data/a-output/result.txt
          echo "shard-0" > /data/b-output/shard-0/result.txt
          echo "[B1] shard-0 done"
          sleep 2

    - id: B2
      image: busybox:1.36
      executionClass: standard
      resourceProfile: small
      dependsOn: [A]
      command:
        - sh
        - -c
        - |
          mkdir -p /data/b-output/shard-1
          cat /data/a-output/result.txt
          echo "shard-1" > /data/b-output/shard-1/result.txt
          echo "[B2] shard-1 done"
          sleep 2

    - id: B3
      image: busybox:1.36
      executionClass: standard
      resourceProfile: small
      dependsOn: [A]
      command:
        - sh
        - -c
        - |
          mkdir -p /data/b-output/shard-2
          cat /data/a-output/result.txt
          echo "shard-2" > /data/b-output/shard-2/result.txt
          echo "[B3] shard-2 done"
          sleep 2

    - id: C
      image: busybox:1.36
      executionClass: highmem
      resourceProfile: medium
      dependsOn: [B1, B2, B3]
      command:
        - sh
        - -c
        - |
          mkdir -p /data/c-output
          ls /data/b-output/
          cat /data/b-output/shard-0/result.txt
          cat /data/b-output/shard-1/result.txt
          cat /data/b-output/shard-2/result.txt
          echo "done" > /data/c-output/report.txt
          echo "[C] report done"
```

### 3.2 최상위 필드 설명

| 필드 | 타입 | 필수 여부 | 설명 |
|------|------|-----------|------|
| `apiVersion` | string | 필수 | `pipeline-lite/v1alpha1` 고정 |
| `kind` | string | 필수 | `Pipeline` 고정 |
| `metadata.name` | string | 필수 | 파이프라인 식별자. K8s Job 이름 prefix로 사용됨 |
| `metadata.namespace` | string | 선택 | 기본값 `poc` |
| `spec.executionClass` | string | 선택 | 전체 기본값. 노드별 override 가능 |
| `spec.sharedPVC.claimName` | string | 필수 | 모든 노드가 마운트할 PVC 이름 |
| `spec.sharedPVC.mountPath` | string | 필수 | 마운트 경로. 기본값 `/data` |
| `spec.nodes` | list | 필수 | 노드 목록 |

### 3.3 노드(node) 필드 설명

| 필드 | 타입 | 필수 여부 | 설명 |
|------|------|-----------|------|
| `id` | string | 필수 | 노드 고유 식별자. DAG 의존성 참조 키 |
| `image` | string | 필수 | 컨테이너 이미지 |
| `executionClass` | string | 선택 | `standard` 또는 `highmem`. 미지정 시 spec 기본값 사용 |
| `resourceProfile` | string | 선택 | `small` 또는 `medium`. 미지정 시 `small` |
| `dependsOn` | list[string] | 선택 | 이 노드 실행 전에 완료되어야 하는 노드 ID 목록 |
| `command` | list[string] | 필수 | K8s Job의 `spec.template.spec.containers[0].command` 에 직접 매핑 |

---

## 4. executionClass 정의

executionClass는 caleb reference의 동일 개념을 PoC에 맞게 단순화한 것이다.
caleb에서는 executionClass가 스케줄러 큐, 노드 셀렉터, 우선순위 등을 결정한다.
PoC에서는 K8s Job이 제출될 논리적 큐(실제로는 네임스페이스 내 Job 분류 레이블)로 매핑한다.

| Class | 논리 큐 레이블 | 용도 | 비고 |
|-------|---------------|------|------|
| `standard` | `poc-standard-queue` | 일반 데이터 처리 노드 | A, B1, B2, B3 |
| `highmem` | `poc-highmem-queue` | 메모리 집약적 집계/분석 노드 | C |

### 4.1 K8s Job 레이블 매핑

spawner가 Job을 생성할 때 다음 레이블을 부착한다:

```yaml
labels:
  pipeline-lite/execution-class: standard   # 또는 highmem
  pipeline-lite/pipeline-name: poc-pipeline
  pipeline-lite/node-id: B1
```

PoC에서는 실제 큐 분리(별도 노드 풀, PriorityClass 등)를 구현하지 않는다.
레이블 부착만으로 caleb의 executionClass 개념을 표현하며, 향후 확장 시 노드 셀렉터
또는 RuntimeClass로 연결할 수 있도록 인터페이스를 유지한다.

---

## 5. resourceProfile 정의

resourceProfile은 caleb의 resource limits 개념(cpu/memory in millicores/Mi)을
PoC용 프리셋으로 단순화한 것이다.

| Profile | CPU request | CPU limit | Memory request | Memory limit | 대상 노드 |
|---------|-------------|-----------|----------------|--------------|-----------|
| `small` | 100m | 200m | 128Mi | 256Mi | A, B1, B2, B3 |
| `medium` | 250m | 500m | 256Mi | 512Mi | C |

### 5.1 K8s Job resources 매핑

```yaml
resources:
  requests:
    cpu: "100m"
    memory: "128Mi"
  limits:
    cpu: "200m"
    memory: "256Mi"
```

spawner는 node.resourceProfile 값을 읽어 위 프리셋을 Job의
`spec.template.spec.containers[0].resources` 에 직접 주입한다.

### 5.2 kind 환경 고려사항

kind 단일 노드 클러스터에서는 실제로 리소스 격리가 발생하지 않는다.
그러나 명세에 resourceProfile을 명시함으로써:

- spawner 코드에서 profile → resources 변환 로직을 검증할 수 있다
- 실제 클러스터 이전 시 명세 변경 없이 동작함을 보장한다

---

## 6. Shared PVC Handoff

### 6.1 PVC 구성

PoC에서는 단일 PVC `poc-shared-pvc`를 모든 노드가 공유한다.
caleb의 OverlayFS 레이어 스토리지를 K8s PVC로 완전히 대체한다.

```yaml
# poc-shared-pvc PersistentVolumeClaim 예시 (kind 환경)
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: poc-shared-pvc
  namespace: poc
spec:
  accessModes:
    - ReadWriteOnce   # kind 단일 노드이므로 RWO 충분
  resources:
    requests:
      storage: 1Gi
  storageClassName: standard  # kind 기본 storageClass
```

멀티 노드 클러스터로 전환 시 accessMode를 `ReadWriteMany`로 변경하고
NFS 또는 CephFS 기반 StorageClass를 사용해야 한다.
PoC 기간에는 kind의 hostPath provisioner가 제공하는 RWO로 충분하다.

### 6.2 디렉토리 레이아웃

```
/data/
  a-output/
    result.txt          <- A가 생성, B1/B2/B3이 읽음
  b-output/
    shard-0/
      result.txt        <- B1이 생성, C가 읽음
    shard-1/
      result.txt        <- B2가 생성, C가 읽음
    shard-2/
      result.txt        <- B3가 생성, C가 읽음
  c-output/
    report.txt          <- C가 생성 (최종 산출물)
```

### 6.3 마운트 방식

모든 K8s Job은 동일한 PVC를 동일한 경로로 마운트한다.
spawner가 Job spec 생성 시 다음 볼륨 설정을 자동으로 주입한다:

```yaml
volumes:
  - name: shared-data
    persistentVolumeClaim:
      claimName: poc-shared-pvc

containers:
  - name: main
    volumeMounts:
      - name: shared-data
        mountPath: /data
```

### 6.4 디렉토리 생성 책임

각 노드의 command는 자신이 쓸 디렉토리를 `mkdir -p`로 직접 생성한다.
init container 또는 별도 setup Job을 두지 않는다 (PoC 단순화 원칙).

---

## 7. Fast-Fail 정의

### 7.1 동작 규칙

| 실패 노드 | 실행 차단 대상 | 동작 |
|-----------|---------------|------|
| A | B1, B2, B3, C | A Job 실패(exit code != 0) → spawner가 B1/B2/B3 Job 미생성 → C Job 미생성 |
| B1 (또는 B2 또는 B3) | C | 어느 하나라도 실패 → C Job 미생성 |
| B1 실패 시 B2/B3 처리 | B2, B3 (선택적) | PoC에서는 이미 실행 중인 B2/B3는 완료까지 대기. 신규 실행은 차단 |

### 7.2 구현 메커니즘

dag-go 라이브러리의 부모 실패 → 자식 Skip 내장 기능을 활용한다:

- dag-go는 각 노드의 실행 결과를 추적한다
- 부모 노드가 실패 상태로 기록되면 자식 노드의 Run 함수 호출 전에 자동으로 Skip 처리한다
- spawner는 dag-go의 node runner 함수 내에서 K8s Job을 생성하고 완료를 대기한다
- Job 실패(`.status.failed` >= 1) 감지 시 해당 node runner가 error를 반환 → dag-go가 실패 전파

### 7.3 실패 전파 흐름 예시 (A 실패 케이스)

```
dag-go: Run node A
  spawner: create K8s Job poc-pipeline-A
  spawner: watch Job poc-pipeline-A
  Job status: failed (exit code 1)
  spawner: return error to dag-go

dag-go: mark node A as FAILED
dag-go: evaluate dependents of A -> [B1, B2, B3]
dag-go: B1 parent FAILED -> Skip B1
dag-go: B2 parent FAILED -> Skip B2
dag-go: B3 parent FAILED -> Skip B3
dag-go: evaluate dependents of B1,B2,B3 -> [C]
dag-go: C all parents not SUCCESS -> Skip C

Pipeline result: FAILED (node A)
Skipped: B1, B2, B3, C
```

### 7.4 PoC에서 구현하지 않는 fast-fail 기능

- 실행 중인 sibling Job의 강제 종료 (B1 실패 시 실행 중 B2/B3 kill)
- 타임아웃 기반 fast-fail
- 부분 실패 허용 (예: B1 실패해도 B2/B3 결과가 있으면 C 실행)

---

## 8. caleb 참조 vs PoC 단순화 대응표

caleb reference repo의 pipeline_v11.jsonc 및 pipeline_v12.jsonc 기준으로
각 개념이 PoC에서 어떻게 단순화되는지를 명확히 대응시킨다.

| caleb 스펙 개념 | caleb 구현 방식 | PoC 단순화 | 비고 |
|----------------|----------------|-----------|------|
| 스토리지 레이어 | OverlayFS 기반 레이어드 CAS | K8s shared PVC (hostPath) | 단일 노드 kind 환경 전제 |
| 아티팩트 핸드오프 | CAS digest 참조, ORAS pull | PVC 공유 디렉토리 직접 읽기/쓰기 | 경로 규약으로 대체 |
| 원격 스토리지 | S3/GCS 백엔드 | 없음 | PVC 내 로컬 경로만 사용 |
| binding semantics | input/output binding DSL | node.command 직접 지정 | command에 경로 하드코딩 |
| schemaVersion | 버전별 스키마 진화 지원 | `pipeline-lite/v1alpha1` 고정 | PoC 기간 단일 버전 |
| executionClass | 스케줄러 큐, 노드 풀 배치 | Job 레이블로만 표현 | 실제 큐 분리 없음 |
| resourceProfile | 세분화된 CPU/메모리 프리셋 | small / medium 2단계 프리셋 | 밀리코어/Mi 단위 동일 |
| 노드 의존성 | `dependsOn` 배열 | `dependsOn` 배열 (동일) | 개념 직접 계승 |
| fan-out | parallel 노드 그룹 | dependsOn이 동일 부모인 노드들 | 명시적 병렬 선언 없음 |
| fan-in | 복수 부모 의존 | dependsOn에 복수 노드 나열 | caleb과 동일 방식 |
| fast-fail | 실패 전파 정책 설정 가능 | 전파 고정 (항상 fast-fail) | 옵션 없음 |
| retries | retry count, backoff 설정 | 없음 (0회 재시도) | Job 실패 = 파이프라인 실패 |
| resume / checkpoint | 중간 지점부터 재시작 | 없음 | 전체 재실행만 가능 |
| 멀티 테넌트 | 네임스페이스 + RBAC 격리 | 단일 네임스페이스 poc | 격리 없음 |

---

## 9. 더미 컨테이너 스크립트 (Day 5용 선행 정의)

Day 5에서 실제 파이프라인 실행 통합 테스트를 수행하기 전에,
각 노드에서 실행될 더미 스크립트를 이 문서에 미리 정의한다.
더미 컨테이너는 비즈니스 로직 없이 파일 I/O와 sleep만으로 구성된다.

### 9.1 노드 A (ingest)

```sh
#!/bin/sh
set -e

echo "[A] starting ingest"
mkdir -p /data/a-output

echo "data from node A at $(date)" > /data/a-output/result.txt
echo "[A] wrote /data/a-output/result.txt"

sleep 2
echo "[A] ingest complete"
```

성공 조건: exit code 0, `/data/a-output/result.txt` 존재

### 9.2 노드 B1 (shard-0 처리)

```sh
#!/bin/sh
set -e

echo "[B1] starting shard-0 processing"
mkdir -p /data/b-output/shard-0

echo "[B1] reading A output:"
cat /data/a-output/result.txt

echo "shard-0 processed by B1 at $(date)" > /data/b-output/shard-0/result.txt
echo "[B1] wrote /data/b-output/shard-0/result.txt"

sleep 2
echo "[B1] shard-0 complete"
```

### 9.3 노드 B2 (shard-1 처리)

```sh
#!/bin/sh
set -e

echo "[B2] starting shard-1 processing"
mkdir -p /data/b-output/shard-1

echo "[B2] reading A output:"
cat /data/a-output/result.txt

echo "shard-1 processed by B2 at $(date)" > /data/b-output/shard-1/result.txt
echo "[B2] wrote /data/b-output/shard-1/result.txt"

sleep 2
echo "[B2] shard-1 complete"
```

### 9.4 노드 B3 (shard-2 처리)

```sh
#!/bin/sh
set -e

echo "[B3] starting shard-2 processing"
mkdir -p /data/b-output/shard-2

echo "[B3] reading A output:"
cat /data/a-output/result.txt

echo "shard-2 processed by B3 at $(date)" > /data/b-output/shard-2/result.txt
echo "[B3] wrote /data/b-output/shard-2/result.txt"

sleep 2
echo "[B3] shard-2 complete"
```

### 9.5 노드 C (report / 집계)

```sh
#!/bin/sh
set -e

echo "[C] starting report aggregation"
mkdir -p /data/c-output

echo "[C] listing b-output shards:"
ls /data/b-output/

echo "[C] reading shard results:"
for shard in /data/b-output/shard-*/result.txt; do
  echo "  --- $shard ---"
  cat "$shard"
done

{
  echo "=== PoC Pipeline Report ==="
  echo "generated at: $(date)"
  echo ""
  echo "shard results:"
  for shard in /data/b-output/shard-*/result.txt; do
    cat "$shard"
  done
  echo ""
  echo "status: done"
} > /data/c-output/report.txt

echo "[C] wrote /data/c-output/report.txt"
echo "[C] report complete"
```

성공 조건: exit code 0, `/data/c-output/report.txt` 존재

### 9.6 fast-fail 테스트용 실패 스크립트

A 노드 실패를 시뮬레이션하여 fast-fail 동작을 검증할 때 사용한다.

```sh
#!/bin/sh
# A 노드 실패 시뮬레이션 (fast-fail 테스트용)
echo "[A-FAIL] simulating ingest failure"
sleep 1
echo "[A-FAIL] exiting with code 1"
exit 1
```

### 9.7 Day 5 통합 테스트 체크리스트

| 검증 항목 | 기대 결과 |
|-----------|-----------|
| 정상 실행: A → B1/B2/B3 → C | 모든 Job completed, `/data/c-output/report.txt` 생성됨 |
| fast-fail: A 실패 | B1/B2/B3, C Job이 생성되지 않음 |
| fast-fail: B2 실패 | C Job이 생성되지 않음, B1/B3는 완료 |
| PVC 데이터 지속성 | Job 삭제 후에도 PVC에 파일 존재 |
| executionClass 레이블 | Job에 `pipeline-lite/execution-class` 레이블 부착 확인 |
| resourceProfile 주입 | Job spec에 CPU/Memory request/limit 값 확인 |

---

## 10. 참고: caleb DSL vs Pipeline-Lite YAML 비교

caleb의 pipeline_v12.jsonc 방식(참조용 의사 코드):

```jsonc
{
  "schemaVersion": "1.2",
  "nodes": {
    "A": {
      "executionClass": "standard",
      "resourceProfile": "small",
      "outputs": {
        "result": { "type": "file", "path": "result.txt" }
      }
    },
    "B1": {
      "executionClass": "standard",
      "dependsOn": ["A"],
      "inputs": {
        "upstream": { "$ref": "A.outputs.result" }
      }
    }
  }
}
```

Pipeline-Lite YAML 방식 (이 문서의 명세):

```yaml
nodes:
  - id: A
    executionClass: standard
    resourceProfile: small
    command:
      - sh
      - -c
      - "mkdir -p /data/a-output && echo data > /data/a-output/result.txt"

  - id: B1
    executionClass: standard
    resourceProfile: small
    dependsOn: [A]
    command:
      - sh
      - -c
      - "cat /data/a-output/result.txt && echo shard-0 > /data/b-output/shard-0/result.txt"
```

핵심 차이: caleb은 input/output binding을 DSL로 추상화하여 스토리지 백엔드와 분리한다.
Pipeline-Lite는 이 추상화를 제거하고 command에 PVC 경로를 직접 기술한다.
이는 PoC 기간 동안의 의도적인 단순화이며, 프로덕션 전환 시 binding DSL을 도입해야 한다.

---

*이 문서는 10일 PoC 범위에서만 유효하며 caleb full spec을 대체하지 않는다.*
*작성일: 2026-03-27*
