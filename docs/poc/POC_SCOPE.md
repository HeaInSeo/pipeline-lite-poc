# POC_SCOPE.md

> PoC 기간: 2026-03-27 (Day 1) ~ 2026-04-05 (Day 10)
> 문서 작성일: 2026-03-27

---

## 1. 목적 (Purpose)

본 PoC의 목적은 다음 기술 조합이 10일 안에 실용적으로 동작함을 검증하는 것이다.

```
Pipeline-Lite + dag-go + spawner + Kueue + kube-scheduler
```

구체적인 검증 목표는 아래와 같다.

- `kind` + `Kubernetes` + `Kueue` 환경 위에서 실제 실행 흐름이 동작하는지 확인한다.
- DAG 실행 엔진(`dag-go`)과 K8s Job lifecycle 관리(`spawner`)의 연결 가능성을 검증한다.
- Kueue의 queue / quota / admission 기능이 실제로 Job 스케줄링에 개입하는지 확인한다.
- 이 조합이 production 진입 자격이 있는지 판단하기 위한 최소한의 증거를 수집한다.

본 PoC는 **production-grade 구현이 아니다.** 정리된 코드, 완전한 에러 핸들링, 범용 설계는 목표가 아니다. 동작하는 최소 증거를 빠르게 확보하는 것이 유일한 목표다.

---

## 2. 검증 대상 조합 (Components Under Validation)

| 컴포넌트 | 역할 | 비고 |
|---|---|---|
| **Pipeline-Lite** | 경량 파이프라인 명세 | YAML 또는 Go struct로 정의 |
| **dag-go** | DAG 실행 엔진 | `github.com/seoyhaein/dag-go` |
| **spawner** | K8s Job lifecycle 관리 | `github.com/seoyhaein/spawner`, 자유롭게 수정 가능 |
| **Kueue** | K8s 워크로드 큐잉 및 quota 관리 | `LocalQueue` / `ClusterQueue` / `ResourceFlavor` 사용 |
| **kube-scheduler** | 표준 K8s 스케줄러 | 별도 플러그인 없이 표준 스케줄러 사용 |

### 2.1 dag-go

- DAG 노드 간 의존성(`A → B → C`)을 표현하고 실행 순서를 결정한다.
- 노드 실행의 실제 구현은 spawner에 위임한다.
- PoC에서는 참조 사용이 원칙이며, 수정이 필요한 경우 보고 후 승인을 받는다.

### 2.2 spawner

- DAG 노드 하나에 대응하는 K8s `Job`을 생성하고, 완료/실패 여부를 감시한다.
- `WorkloadPriorityClass` 또는 `LocalQueue` 레이블을 Job에 부착하여 Kueue로 라우팅한다.
- PoC 기간 중 자유롭게 수정 가능한 유일한 컴포넌트다.

### 2.3 Kueue

- `ClusterQueue`: 클러스터 전체 quota를 정의한다.
- `LocalQueue`: namespace 단위 큐. spawner가 Job을 제출할 때 참조한다.
- `ResourceFlavor`: 노드 리소스 특성을 추상화한다.
- `executionClass`(`standard` / `highmem`)를 서로 다른 `LocalQueue`로 라우팅하는 것이 핵심 검증 포인트다.

---

## 3. 반드시 포함할 것 (In-Scope)

아래 6가지는 PoC 완료의 필수 조건이다. 하나라도 동작하지 않으면 완료로 인정하지 않는다.

### 3.1 DAG dependency: 순차 실행

```
A → B → C
```

- `A`가 완료된 후에만 `B`가 시작되어야 한다.
- `B`가 완료된 후에만 `C`가 시작되어야 한다.
- dag-go의 dependency resolution이 spawner의 Job 생성 순서를 제어해야 한다.

### 3.2 Fan-out: 병렬 실행

- `B` 단계에서 3개 이상 5개 이하의 sub-job이 병렬로 실행된다.
- 모든 fan-out Job이 완료된 후에 `C`가 시작된다.
- fan-out의 수는 PoC에서 하드코딩으로 고정한다.

### 3.3 executionClass: 2가지 이상

- `standard`: 일반 CPU/메모리 Job
- `highmem`: 높은 메모리 요구량을 가진 Job
- 각 `executionClass`는 서로 다른 `LocalQueue`로 라우팅되어야 한다.
- spawner가 Job 생성 시 `executionClass`를 기반으로 올바른 `LocalQueue`를 선택해야 한다.

### 3.4 resourceProfile: 명시적 리소스 지정

- 각 Job의 `cpu` / `memory` 요청량을 명시적으로 지정한다.
- Pipeline-Lite 명세에서 `resourceProfile`을 정의하고, spawner가 이를 Job의 `resources.requests`에 반영한다.

### 3.5 Shared PVC handoff: 단일 PVC 공유

- 하나의 `PersistentVolumeClaim`을 파이프라인의 모든 단계(A, B fan-out, C)가 공유한다.
- `A`가 PVC에 출력 파일을 기록하고, `B`가 그 파일을 읽는 것을 실제로 확인한다.
- PVC는 `ReadWriteMany` 또는 `ReadWriteOnce`(순차 실행 보장 시) 중 동작하는 것을 선택한다.

### 3.6 Fast-fail: 실패 전파

- `A`가 실패하면 `B`와 `C`가 실행되지 않아야 한다.
- `B`(또는 fan-out 중 하나)가 실패하면 `C`가 실행되지 않아야 한다.
- dag-go의 실행 제어 또는 spawner의 상태 감시를 통해 fast-fail을 구현한다.
- 실패 케이스를 실제로 시뮬레이션하여 동작을 확인한다.

---

## 4. 하지 말 것 (Out-of-Scope)

아래 항목은 10일 PoC 기간 중 절대 손대지 않는다. 범위를 넓히려는 충동이 생기면 이 목록을 다시 읽는다.

| 항목 | 이유 |
|---|---|
| Full pipeline DSL | PoC에서는 Go struct 또는 최소 YAML로 충분하다 |
| Full binding semantics | 입출력 바인딩의 완전한 구현은 필요하지 않다 |
| CAS / S3 / ORAS 스토리지 | 공유 PVC로 충분히 handoff를 검증할 수 있다 |
| Retries / resume / recovery | 재시도 로직은 PoC 범위 밖이다 |
| UI | 시각화 도구는 필요 없다 |
| Generalized storage architecture | 범용 스토리지 추상화는 production 단계에서 결정한다 |
| Broad refactor | 동작 확인 없는 대규모 코드 정리는 금지한다 |
| Production-grade observability | 로그 확인 수준으로 충분하다. 별도 모니터링 스택 불필요 |

---

## 5. 완료 기준 (Definition of Done)

아래 기준을 모두 충족해야 PoC를 완료로 선언할 수 있다.

1. **End-to-end 실행**: `kind` 클러스터 위에서 `A → B(fan-out) → C` 흐름이 실제로 실행되고, 모든 Job이 `Completed` 상태로 종료된다.

2. **Kueue 적용 확인**: `ClusterQueue` / `LocalQueue` / `ResourceFlavor`가 실제로 적용되어 Job의 admission이 Kueue를 거친다는 것을 `kubectl get workload` 또는 Kueue 이벤트로 확인한다.

3. **Fast-fail 동작**: `A`를 의도적으로 실패시켰을 때 `B`와 `C`의 Job이 생성되지 않음을 확인한다.

4. **Shared PVC handoff**: `A`가 PVC에 기록한 파일을 `B`가 실제로 읽는다는 것을 Job 로그로 확인한다.

5. **executionClass 라우팅**: `standard` Job과 `highmem` Job이 서로 다른 `LocalQueue`로 제출되어 각각의 `ClusterQueue`를 통해 처리됨을 확인한다.

6. **재활용 가능한 산출물**: 각 Day의 산출물(스크립트, 매니페스트, 코드)이 독립적으로 재사용 가능한 형태로 repo에 남는다.

---

## 6. 폐기 기준 (Discard Criteria)

아래 조건 중 하나라도 확인되면 PoC를 중단하고 기술 조합을 재검토한다.

1. **구조적 연결 불가**: `dag-go`의 실행 모델과 `spawner`의 Job lifecycle 관리 방식이 구조적으로 연결할 수 없는 형태임이 밝혀진 경우.

2. **Kueue 충돌**: Kueue admission 흐름과 `spawner`의 Job 생성 방식이 근본적으로 충돌하여 우회가 불가능한 경우.

3. **Day 5 관통 실패**: Day 5 종료 시점까지 `A → B → C` 순차 실행 흐름이 단 한 번도 end-to-end로 성공하지 못한 경우.

4. **복잡도 폭발**: PoC 구현의 복잡도가 production 수준과 동일해져서 PoC로서의 의미를 잃은 경우.

---

## 7. Repo 정책 (Repository Policy)

| Repo | 정책 | 비고 |
|---|---|---|
| `spawner` | 자유롭게 수정 가능 | PoC의 주요 수정 대상 |
| `dag-go` | 참조 사용 원칙, 수정 필요 시 보고 후 승인 | API는 그대로 사용 |
| `caleb` | 참조 전용, 수정 금지 | 구조 및 설계 참고용 |
| `poc` | 신규 폴더, 모든 PoC 작업의 주 작업 공간 | 이 repo가 PoC의 홈 |

- `poc` repo의 디렉토리 구조는 아래를 기준으로 한다.

```
poc/
  docs/poc/          # PoC 문서
  hack/              # 클러스터 셋업 및 실행 스크립트
  deploy/            # Kubernetes / Kueue 매니페스트
  internal/          # PoC 전용 Go 코드
  cmd/               # 실행 진입점
```

---

## 8. 작업 원칙 (Working Principles)

1. **범위 확대 금지**: Out-of-Scope 항목은 손대지 않는다. 필요해 보여도 PoC 기간 내에는 추가하지 않는다.

2. **Small diff 우선**: 한 번에 큰 변경을 만들지 않는다. 동작을 확인하면서 작게 쌓는다.

3. **먼저 관통, 나중에 정리**: 코드가 지저분해도 동작하는 것이 먼저다. 정리는 관통 이후에 한다.

4. **막히면 최소 우회 구현**: 설계 이슈로 막히면 범용 해결책을 찾으려 하지 말고, 동작하는 최소 우회 구현을 먼저 찾는다.

5. **동작 확인 없는 큰 리팩터 금지**: 동작이 확인되지 않은 상태에서의 대규모 코드 정리는 PoC를 망친다. 금지한다.

6. **재활용 가능한 자산**: 각 Day의 결과물은 나중에 재사용할 수 있는 형태로 남긴다. 일회용 스크립트도 정리하여 `hack/`에 보관한다.

---

## 9. 10일 일정 요약 (10-Day Schedule)

| Day | 날짜 | 목표 | 핵심 산출물 |
|---|---|---|---|
| **Day 1** | 2026-03-27 | PoC 범위 확정 및 환경 셋업 계획 수립 | `POC_SCOPE.md`, `DAILY_LOG_D1.md`, kind 클러스터 셋업 스크립트 초안 |
| **Day 2** | 2026-03-28 | `kind` 클러스터 기동, Kueue 설치, 기본 매니페스트 작성 | `hack/setup-cluster.sh`, `deploy/kueue-config.yaml`, 클러스터 동작 확인 |
| **Day 3** | 2026-03-29 | spawner로 단일 K8s Job 생성 및 완료 감시 동작 확인 | spawner 최소 연동 코드, Job 생성 + 상태 감시 동작 로그 |
| **Day 4** | 2026-03-30 | dag-go와 spawner 연결: `A → B` 순차 실행 구현 | dag-go 노드 실행 핸들러, `A → B` 실행 성공 로그 |
| **Day 5** | 2026-03-31 | `A → B → C` 전체 관통, fast-fail 구현 | end-to-end 실행 성공 로그, fast-fail 동작 확인 로그 |
| **Day 6** | 2026-04-01 | Fan-out 구현: B 단계에서 3~5개 병렬 Job 실행 | fan-out Job 병렬 실행 확인, 전체 fan-out 완료 후 C 시작 확인 |
| **Day 7** | 2026-04-02 | Shared PVC handoff 구현 및 확인 | PVC 매니페스트, A 출력 → B 입력 연결 확인 로그 |
| **Day 8** | 2026-04-03 | executionClass 라우팅 구현: standard / highmem → 서로 다른 LocalQueue | executionClass별 LocalQueue 라우팅 확인, Kueue workload 이벤트 로그 |
| **Day 9** | 2026-04-04 | 전체 시나리오 통합 테스트 및 엣지 케이스 확인 | 통합 실행 로그, fast-fail / fan-out / PVC / executionClass 모두 포함 |
| **Day 10** | 2026-04-05 | 결과 정리, 폐기/진행 판단, 산출물 최종 정리 | `POC_RESULT.md`, 산출물 정리 완료, 진행/폐기 권고안 |

### Day별 상세 설명

**Day 1 (2026-03-27) - 범위 확정 및 계획**
- `POC_SCOPE.md` 작성으로 PoC의 경계를 명확히 한다.
- 환경 요구사항(`kind`, `kubectl`, `kueuectl` 등) 목록을 정리한다.
- Day 2에 사용할 클러스터 셋업 스크립트 초안을 작성한다.
- 각 컴포넌트의 현재 상태와 PoC에서의 역할을 파악한다.

**Day 2 (2026-03-28) - 환경 구축**
- `kind` 클러스터를 기동하고 `Kueue`를 설치한다.
- `ClusterQueue`, `LocalQueue`, `ResourceFlavor` 기본 매니페스트를 작성한다.
- 수동으로 Job을 제출하여 Kueue admission이 동작하는지 확인한다.
- 이후 모든 Day에서 재사용 가능한 클러스터 환경을 확립한다.

**Day 3 (2026-03-29) - spawner 단독 검증**
- spawner의 현재 코드를 파악하고 PoC에 필요한 최소 인터페이스를 정의한다.
- spawner로 K8s Job을 생성하고 완료까지 감시하는 최소 코드를 작성한다.
- Kueue와의 기본 연동(LocalQueue 레이블 부착)을 확인한다.

**Day 4 (2026-03-30) - dag-go + spawner 연결**
- dag-go의 노드 실행 핸들러에서 spawner를 호출하는 연결 코드를 작성한다.
- `A → B` 2단계 DAG를 정의하고 실행하여 순차 실행을 확인한다.
- dag-go의 dependency resolution이 spawner Job 생성 순서를 올바르게 제어하는지 검증한다.

**Day 5 (2026-03-31) - 전체 관통 + Fast-fail**
- `A → B → C` 3단계 전체를 관통한다. 이것이 PoC의 핵심 마일스톤이다.
- `A`를 실패시켜 `B`, `C`가 실행되지 않음을 확인한다.
- Day 5까지 관통 실패 시 폐기 기준에 따라 PoC 중단을 검토한다.

**Day 6 (2026-04-01) - Fan-out**
- `B` 단계를 fan-out으로 확장하여 3~5개 Job을 병렬 실행한다.
- 모든 fan-out Job 완료 후 `C`가 시작되는 것을 확인한다.
- fan-out 중 하나가 실패할 때의 fast-fail 동작도 확인한다.

**Day 7 (2026-04-02) - Shared PVC**
- `ReadWriteMany` 또는 순차 보장 방식으로 단일 PVC를 구성한다.
- `A`가 PVC에 파일을 쓰고 `B`가 그 파일을 읽는 것을 Job 로그로 확인한다.
- PVC 매니페스트를 `deploy/` 에 정리한다.

**Day 8 (2026-04-03) - executionClass 라우팅**
- `standard`와 `highmem` 두 가지 `executionClass`를 정의한다.
- 각 `executionClass`에 대응하는 `LocalQueue`와 `ClusterQueue`를 구성한다.
- spawner가 `executionClass`를 읽어 올바른 `LocalQueue`에 Job을 제출하는지 확인한다.
- `kubectl get workload` 출력으로 각 Job이 의도한 queue를 통해 처리됨을 검증한다.

**Day 9 (2026-04-04) - 통합 테스트**
- 전체 시나리오를 하나의 실행으로 통합한다.
- 성공 케이스: `A → B(fan-out) → C`, executionClass 라우팅, PVC handoff 모두 포함.
- 실패 케이스: fast-fail 시나리오 재확인.
- 통합 실행 스크립트를 `hack/`에 정리한다.

**Day 10 (2026-04-05) - 결과 정리 및 판단**
- `POC_RESULT.md`를 작성하여 검증 결과, 발견된 제약, 진행/폐기 권고를 기록한다.
- 모든 산출물을 최종 정리하고 repo를 정돈한다.
- 다음 단계(production 구현 진행 또는 기술 조합 재검토)에 대한 권고안을 작성한다.

---

## 10. 반드시 남겨야 하는 산출물 (Required Deliverables)

PoC 종료 시점(Day 10)에 아래 산출물이 모두 repo에 존재해야 한다.

### 10.1 docs/poc/ 문서 (5개)

| 파일 | 내용 |
|---|---|
| `docs/poc/POC_SCOPE.md` | 본 문서. PoC 범위, 기준, 일정 |
| `docs/poc/ARCH_SKETCH.md` | 컴포넌트 연결 구조 스케치. dag-go / spawner / Kueue 인터페이스 정의 |
| `docs/poc/DAILY_LOG_D1.md` ~ `DAILY_LOG_D10.md` | 일별 작업 로그. 결정 사항, 발견 사항, 다음 날 계획 포함 |
| `docs/poc/CONSTRAINTS.md` | PoC 진행 중 발견된 제약 및 가정 목록 |
| `docs/poc/POC_RESULT.md` | Day 10 최종 결과. 검증 완료 항목, 미완료 항목, 진행/폐기 판단 |

### 10.2 hack/ 스크립트 (2개 이상)

| 파일 | 내용 |
|---|---|
| `hack/setup-cluster.sh` | `kind` 클러스터 생성, Kueue 설치, 기본 매니페스트 적용을 자동화하는 스크립트 |
| `hack/run-poc.sh` | PoC 전체 시나리오(A→B→C + fan-out)를 실행하는 스크립트 |

### 10.3 deploy/ 매니페스트

| 파일 / 디렉토리 | 내용 |
|---|---|
| `deploy/kueue/` | `ClusterQueue`, `LocalQueue`, `ResourceFlavor` 매니페스트 |
| `deploy/pvc.yaml` | Shared PVC 매니페스트 |
| `deploy/jobs/` | 테스트용 Job 매니페스트 샘플 (A, B, C 각각) |

### 10.4 코드 자산 (4가지)

| 자산 | 위치 | 내용 |
|---|---|---|
| DAG 노드 실행 핸들러 | `internal/runner/` | dag-go 노드에서 spawner를 호출하는 연결 코드 |
| Pipeline-Lite 명세 구조체 | `internal/pipeline/` | `executionClass`, `resourceProfile`, fan-out 명세를 담는 Go struct |
| spawner PoC 확장 코드 | `spawner` repo 또는 `internal/spawner/` | Kueue LocalQueue 레이블 부착, executionClass 라우팅 로직 |
| 통합 실행 진입점 | `cmd/poc-runner/main.go` | PoC 전체 시나리오를 실행하는 Go main 패키지 |

---

## 참고 (References)

- Kueue 공식 문서: https://kueue.sigs.k8s.io/
- dag-go: https://github.com/seoyhaein/dag-go
- spawner: https://github.com/seoyhaein/spawner
- kind: https://kind.sigs.k8s.io/

---

*이 문서는 Day 1에 작성되었으며, PoC 진행 중 발견되는 사실에 따라 `CONSTRAINTS.md`에 보완 기록을 남긴다. 본 문서 자체는 가급적 수정하지 않는다.*
