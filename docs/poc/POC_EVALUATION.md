# POC_EVALUATION

> 작성일: 2026-03-28 (Day 10)
> PoC 기간: 2026-03-27 ~ 2026-03-28 (10 Day, 실제 2일 압축 진행)

---

## 1. 판정 요약

| 항목 | 결과 |
|------|------|
| **최종 판정** | **GO** — production 진입 자격 있음 |
| 필수 완료 기준 (6/6) | 모두 충족 |
| 폐기 기준 해당 | 없음 |
| 미결 리스크 | 4건 (production 단계 처리 가능) |

---

## 2. 완료 기준 체크 (Definition of Done)

POC_SCOPE.md §5의 필수 기준 전체 충족 여부.

### 2.1 End-to-end 실행 ✅

| 시나리오 | Day | 결과 |
|----------|-----|------|
| A→B→C 선형 파이프라인 | Day 5 | PASS |
| A→B1/B2/B3 fan-out → C 수렴 | Day 6 | PASS (B1/B2/B3 병렬 확인) |
| A→B1(std)/B2(highmem)→C 혼용 | Day 8 | PASS |

K8s Jobs 최종 상태 (클러스터 잔여 기록):
```
pipeline-abc-a/b/c   Complete
fanout-a/b1/b2/b3    Complete
ec-a/b1/b2/c         Complete
```

### 2.2 Kueue 적용 확인 ✅

- 모든 Job이 `suspend: true` + `kueue.x-k8s.io/queue-name` 레이블로 제출됨
- `kubectl get workloads` 로 admission 흐름 확인 (Day 2~8)
- standard Jobs → `poc-standard-cq`, highmem Job → `poc-highmem-cq` 라우팅 확인

### 2.3 Fast-fail 동작 ✅

- Day 7: ff-b2 `exit 1` → ff-c Job 미제출 확인
- dag-go의 부모 실패 전파(`parent channel returned Failed`)가 spawner Job 제출 자체를 차단함
- `kubectl get jobs` 에 ff-c 없음으로 증명

### 2.4 Shared PVC handoff ✅

- Day 5: A가 `/data/a.txt` 기록 → B가 읽어 `/data/b.txt` 생성 → C가 내용 검증 후 exit 0
- PVC: `poc-shared-pvc` (RWO, kind single-node 환경에서 순차 실행으로 RWO 충분)

### 2.5 executionClass 라우팅 ✅

- `adapter.ExecutionClass` → `QueueLabel()` → `kueue.x-k8s.io/queue-name` label injection
- standard → `poc-standard-lq` → `poc-standard-cq`
- highmem  → `poc-highmem-lq`  → `poc-highmem-cq`
- 동일 DAG 내 두 큐 동시 활성 확인 (Day 8)

### 2.6 재활용 가능한 산출물 ✅

아래 §5 참고.

---

## 3. 폐기 기준 점검 (Discard Criteria)

POC_SCOPE.md §6 기준 전체 해당 없음.

| 폐기 기준 | 결과 |
|-----------|------|
| dag-go ↔ spawner 구조적 연결 불가 | ❌ 해당 없음 — SpawnerNode 어댑터로 연결 성공 |
| Kueue admission ↔ spawner 충돌 | ❌ 해당 없음 — suspend=true 패턴으로 완전 통합 |
| Day 5 관통 실패 | ❌ 해당 없음 — Day 5 PASS |
| 복잡도 폭발 | ❌ 해당 없음 — PoC 수준 코드 유지 |

---

## 4. 컴포넌트별 평가

### 4.1 dag-go

**평가: 적합**

- `InitDag → CreateNode → AddEdge → FinishDag → ConnectRunner → GetReady → Start → Wait` 흐름이 명확하고 예측 가능함
- `Runnable` 인터페이스 하나(`RunE`)만 구현하면 어떤 실행 백엔드도 연결 가능
- 부모 실패 전파, 병렬 fan-out, 수렴 노드 모두 별도 코드 없이 DAG 구조만으로 동작
- **주의**: `SetNodeRunner` vs `node.SetRunner` 두 API 경로 존재 — 팀 컨벤션으로 통일 필요

### 4.2 spawner (DriverK8s)

**평가: 적합, 일부 production 대비 필요**

- `Prepare → Start → Wait → Cancel` 4단계 lifecycle이 K8s Job과 1:1 매핑됨
- Kueue 통합 패턴(`suspend=true` + label)이 단순하고 안정적
- **제약**: Wait가 2초 폴링 → production에서는 `Watch` 기반으로 교체 필요 (§5.1)
- **제약**: Job 이름이 RunID 직접 사용 → 동일 RunID 재실행 시 수동 삭제 필요 (§5.2)

### 4.3 Kueue

**평가: 적합**

- `ClusterQueue` quota, `LocalQueue` 라우팅, `ResourceFlavor` 추상화 모두 예상대로 동작
- `executionClass` → `queue-name` 매핑이 label injection만으로 충분히 구현됨
- pending → quota 회복 후 자동 admit 동작 확인 (Day 9 testPending)
- **확인됨**: 같은 DAG 내 두 ClusterQueue 동시 활성 가능

### 4.4 SpawnerNode 어댑터 (poc/pkg/adapter)

**평가: 재사용 가능**

- dag-go `Runnable` ↔ spawner `Driver` 브릿지가 ~50줄로 완성됨
- `Driver driver.Driver` 인터페이스 필드 → mock driver 주입 가능 (테스트 용이)
- `ExecutionClass.QueueLabel()` 헬퍼가 라우팅 로직을 단순화

---

## 5. 미결 리스크 (production 단계 처리 항목)

### 5.1 Wait 폴링 → Watch 교체 [중요도: 높음]

- 현재: 2초 간격 `BatchV1().Jobs().Get()` 폴링
- 문제: 대규모 Job 수에서 API server 부하 증가, 반응 지연
- 해결: `BatchV1().Jobs().Watch()` + `informer` 패턴으로 교체
- 영향 범위: `spawner/cmd/imp/k8s_driver.go` Wait 메서드

### 5.2 RunID 재사용 시 Job 이름 충돌 [중요도: 중간]

- 현재: `sanitizeName(RunID)` → K8s Job 이름 직접 사용
- 문제: 동일 RunID 재실행 시 이전 Job 수동 삭제 필요 (Day 9 testRerun에서 확인)
- 해결: `{runID}-{uuid[:8]}` 패턴 또는 Job TTL 설정으로 자동 정리
- 영향 범위: `buildJob()` 함수, Handle 설계

### 5.3 RWO PVC → multi-node 미검증 [중요도: 중간]

- 현재: kind single-node 환경 → RWO PVC로 순차 실행 충분
- 문제: multi-node 클러스터에서 fan-out 노드 간 PVC 동시 접근 시 스케줄링 제약 발생 가능
- 해결: RWX StorageClass(NFS, CephFS 등) 또는 노드 어피니티 정책 검토 필요
- 영향 범위: deploy/poc/poc-pvc.yaml, 실제 클러스터 StorageClass

### 5.4 dag-go API 변경 시 어댑터 영향 [중요도: 낮음]

- 현재: dag-go v0.0.9 고정
- 문제: dag-go가 `Runnable` 인터페이스 시그니처를 변경하면 SpawnerNode 재작성 필요
- 해결: dag-go 버전 고정 + 변경 시 어댑터 레이어만 수정으로 격리 가능
- 영향 범위: `poc/pkg/adapter/spawner_node.go`

---

## 6. 재사용 가능한 자산

| 자산 | 위치 | 설명 |
|------|------|------|
| K8s Job driver | `spawner/cmd/imp/k8s_driver.go` | Prepare/Start/Wait/Cancel + Kueue 통합 |
| dag-go 어댑터 | `poc/pkg/adapter/spawner_node.go` | Runnable 구현, Driver 인터페이스 사용 |
| ExecutionClass 매핑 | `poc/pkg/adapter/execution_class.go` | executionClass → LocalQueue label |
| Kueue 설정 매니페스트 | `poc/deploy/kueue/queues.yaml` | ResourceFlavor, ClusterQueue, LocalQueue |
| kind 클러스터 스크립트 | `poc/hack/kind-up.sh`, `kind-down.sh` | rootless podman kind 클러스터 관리 |
| PVC 매니페스트 | `poc/deploy/poc/poc-pvc.yaml` | shared PVC |
| operational 검증 | `poc/cmd/operational/main.go` | timeout/pending/rerun 시나리오 |

---

## 7. 후속 작업 제안

### 즉시 (production 진입 전 필수)
1. `Wait` → Watch 기반으로 교체 (spawner)
2. Job 이름 충돌 방지 정책 결정 (UUID suffix 또는 TTL)
3. RWX StorageClass 확보 또는 PVC 전략 확정

### 단기 (production v0.1)
4. spawner gRPC 서버 완성 (현재 main.go는 skeleton 수준)
5. Pipeline-Lite 명세를 Go struct → YAML 파싱으로 확장
6. dag-go 버전 고정 및 어댑터 테스트 추가

### 장기 (production v1.0)
7. Kueue WorkloadPriorityClass 연동 (우선순위 큐잉)
8. 멀티 네임스페이스 지원
9. Job 실패 시 재시도/resume 정책

---

## 8. 결론

**Pipeline-Lite + dag-go + spawner + Kueue + kube-scheduler 조합은 10일 PoC에서 모든 필수 기준을 충족했다.**

핵심 발견:
- dag-go의 `Runnable` 인터페이스가 spawner와의 연결점으로 충분히 유연함
- Kueue의 `suspend=true` + `queue-name label` 패턴이 spawner와 마찰 없이 통합됨
- `executionClass → LocalQueue` 매핑이 단순 label injection으로 완결됨
- fast-fail, fan-out, PVC handoff 모두 추가 인프라 없이 dag-go + spawner 조합만으로 구현됨

미결 리스크 4건은 모두 production 단계에서 처리 가능한 수준이며, PoC의 핵심 가정을 뒤집는 구조적 문제는 발견되지 않았다.
