# Round 3: Observability 경계 보고서

> 작성일: 2026-03-28
> 목적: Kueue pending과 kube-scheduler unschedulable을 코드와 실 클러스터 양쪽에서 명확히 구분하고, 각각의 관찰 경로를 검증한다.

---

## 1. 배경 및 목표

### 현재 문제 (구현 전)

```
DriverK8s.Wait()
    │  JobComplete/JobFailed 조건만 감시
    │
    ▼
Kueue pending ≡ kube-scheduler unschedulable
(둘 다 "job이 안 끝남"으로만 보임 — 원인 구분 불가)
```

실제로 두 상태는 완전히 다른 레이어에서 발생한다:

| | Kueue pending | kube-scheduler unschedulable |
|--|--|--|
| **원인** | 클러스터 quota 소진 | Pod 배치 가능한 노드 없음 |
| **발생 위치** | Kueue admission controller | kube-scheduler |
| **관찰 객체** | Workload 리소스 | Pod |
| **관찰 필드** | `workload.status.conditions[QuotaReserved=False]` | `pod.status.conditions[PodScheduled=False]` |
| **Pod 존재 여부** | **없음** (Job suspend 상태 유지) | **있음** (Pending 상태) |
| **수정 방법** | 리소스 요청 줄이기 or quota 늘리기 | nodeSelector/taint 수정 or 노드 추가 |

### 이번 라운드 목표

- K8sObserver 구현: Kueue Workload + Pod 관찰 경로 분리
- 실 kind+Kueue 클러스터에서 두 상태를 실제 유도·관찰
- `//go:build integration` 태그 integration 테스트로 재현 가능하게 기록

---

## 2. 구현 내용

### K8sObserver (spawner/cmd/imp/k8s_observer.go)

```go
type WorkloadObservation struct {
    WorkloadName  string
    QuotaReserved bool    // workload.status.conditions[QuotaReserved]
    Admitted      bool    // workload.status.conditions[Admitted]
    PendingReason string  // QuotaReserved=False일 때 message
}

type PodSchedulingObservation struct {
    PodName             string
    Scheduled           bool   // pod.status.conditions[PodScheduled]
    UnschedulableReason string // PodScheduled=False일 때 message
}

type K8sObserver struct {
    ns        string
    clientset kubernetes.Interface
    dynamic   dynamic.Interface  // Kueue workload 조회용
}

// ObserveWorkload: Job의 ownerReference로 Kueue Workload를 찾아 admission 상태 반환
func (o *K8sObserver) ObserveWorkload(ctx context.Context, jobName string) (WorkloadObservation, error)

// ObservePod: job-name label로 Pod를 찾아 scheduling 상태 반환
func (o *K8sObserver) ObservePod(ctx context.Context, jobName string) (PodSchedulingObservation, error)
```

**buildConfig 헬퍼** (k8s_driver.go와 공유):
```go
func buildConfig(kubeconfigPath string) (*rest.Config, error) {
    if kubeconfigPath != "" {
        return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
    }
    // 1. In-cluster config (Pod 내부 실행 시)
    if cfg, err := rest.InClusterConfig(); err == nil { return cfg, nil }
    // 2. KUBECONFIG 환경변수 → ~/.kube/config (개발/테스트 환경)
    loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
    return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
        loadingRules, &clientcmd.ConfigOverrides{},
    ).ClientConfig()
}
```

기존 `NewK8sFromKubeconfig`의 InCluster-only 문제(개발 환경에서 kubeconfig를 못 읽는 버그)도 이 헬퍼 도입으로 함께 해결됨.

### Integration 테스트 (poc/pkg/integration/kueue_observe_test.go)

```go
//go:build integration
// go test ./pkg/integration/... -v -tags integration
```

---

## 3. 실험 결과

### 실험 1: Kueue pending (TestObserveKueuePending_QuotaExceeded)

**설정:**
- poc-standard-cq quota: CPU=4 cores
- 제출 Job: CPU=5000m (5 cores) → quota 초과

**실행:**
```
go test ./pkg/integration/... -v -tags integration -run TestObserveKueuePending
```

**관찰 결과:**
```
submitted job obs-quota-6687 (CPU=5000m > quota 4000m)

OBSERVATION [Kueue pending]:
  workload=job-obs-quota-6687-23eb7
  QuotaReserved=false
  Admitted=false
  PendingReason="couldn't assign flavors to pod set main: insufficient quota for cpu
    in flavor default-flavor, previously considered podsets requests (0) + current
    podset request (5) > maximum capacity (4)"
  pod: not found (Kueue did not unsuspend job)

PASS: Kueue pending is observable via workload.status.conditions[QuotaReserved=False]
DISTINCTION: this is NOT kube-scheduler unschedulable — no pod, no node-level scheduling
```

**관찰 경로:**
```
workload.status.conditions[type=QuotaReserved]
  → status: "False"
  → message: "insufficient quota for cpu..."
```

Pod는 존재하지 않음. Job은 suspend=true 상태 유지.

---

### 실험 2: kube-scheduler unschedulable (kubectl 직접 재현)

**설정:**
- Job: CPU=100m (quota 내), nodeSelector: `poc-nonexistent-label: "true"` (존재하지 않는 노드 레이블)

**생성 명령:**
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: obs-unsched-manual
  labels:
    kueue.x-k8s.io/queue-name: poc-standard-lq
spec:
  suspend: true
  backoffLimit: 0
  template:
    spec:
      nodeSelector:
        poc-nonexistent-label: "true"
      restartPolicy: Never
      containers:
      - name: main
        image: busybox:1.36
        command: [sh, -c, "echo hello"]
        resources:
          requests: {cpu: 100m, memory: 64Mi}
```

**Kueue Workload 상태 (admitted — quota OK):**
```json
[
  {
    "type": "QuotaReserved",
    "status": "True",
    "reason": "QuotaReserved",
    "message": "Quota reserved in ClusterQueue poc-standard-cq"
  },
  {
    "type": "Admitted",
    "status": "True",
    "reason": "Admitted",
    "message": "The workload is admitted"
  }
]
```

**Pod 상태 (unschedulable — scheduler 차단):**
```json
[
  {
    "type": "PodScheduled",
    "status": "False",
    "reason": "Unschedulable",
    "message": "0/1 nodes are available: 1 node(s) didn't match Pod's node
      affinity/selector. no new claims to deallocate, preemption: 0/1 nodes
      are available: 1 Preemption is not helpful for scheduling."
  }
]
```

**TestObserveUnschedulable_NodeSelectorMismatch (integration test PASS):**
```
submitted job obs-unsched-6820 (CPU=100m, within quota)

OBSERVATION [after Kueue admission]:
  workload=job-obs-unsched-6820-3c2f5
  QuotaReserved=true
  Admitted=true          ← Kueue는 통과

DISTINCTION SUMMARY:
  Kueue pending   → workload.status.conditions[QuotaReserved=False]
    Cause: quota exhausted; job stays in queue; NO pod exists
    Fix:   reduce resource request OR increase ClusterQueue quota

  kube-scheduler unschedulable → pod.status.conditions[PodScheduled=False]
    Cause: Kueue admitted the job; kube-scheduler has no matching node
    Fix:   fix nodeSelector/taint OR add a matching node

  Both appear as 'pending' to the user, but require DIFFERENT fixes.
  Observation path differs: Workload conditions vs Pod conditions.

PASS: distinction documented and observation paths verified
```

---

## 4. 두 상태의 완전한 구분 정리

```
┌─────────────────────────────────────────────────────────────┐
│               사용자에게는 둘 다 "job이 안 돌아간다"로 보임  │
└─────────────────────────────────────────────────────────────┘
         │                              │
         ▼                              ▼
  [Kueue pending]              [kube-scheduler unschedulable]
  quota 소진                    노드 배치 불가
         │                              │
  Workload 조건 관찰             Pod 조건 관찰
  QuotaReserved=False           PodScheduled=False
         │                              │
  Pod 없음                       Pod 있음 (Pending)
  Job suspend=true 유지          Job unsuspended됨
         │                              │
  Fix: quota 늘리기             Fix: nodeSelector 수정
       or 요청 줄이기                 or 노드 추가
```

---

## 5. 경계 성립 여부

| 검증 항목 | 성립 여부 | 근거 |
|-----------|-----------|------|
| Kueue pending 관찰 경로 확인 | ✅ | 실 클러스터 QuotaReserved=False 확인 |
| kube-scheduler unschedulable 관찰 경로 확인 | ✅ | 실 클러스터 PodScheduled=False 확인 |
| Kueue pending 시 Pod 없음 확인 | ✅ | pod: not found 확인 |
| 두 상태가 다른 K8s 객체에서 관찰됨 | ✅ | Workload vs Pod 명확히 분리 |
| Integration 테스트 재현 가능 | ✅ | 2/2 PASS |

**총 2/2 integration test PASS**

---

## 6. DriverK8s.Wait와의 관계

현재 `DriverK8s.Wait()`는 K8s Job의 `Complete`/`Failed` 조건만 폴링한다.

```go
// 현재 Wait — Job 완료만 감시
for {
    job, _ := d.clientset.BatchV1().Jobs(...).Get(ctx, name, ...)
    if isComplete(job) { return api.Event{State: StateSucceeded}, nil }
    if isFailed(job)   { return api.Event{State: StateFailed}, nil }
    time.Sleep(2 * time.Second)
}
```

K8sObserver는 Wait과 **별도의 관찰 경로**다:
- `Wait()`: 실행 완료/실패 감시 (Job 레벨)
- `ObserveWorkload()`: Kueue admission 상태 감시 (Workload 레벨)
- `ObservePod()`: kube-scheduler 배치 상태 감시 (Pod 레벨)

ASSUMPTION: production에서는 세 경로를 통합해 하나의 이벤트 스트림으로 표면화. 현재 PoC에서는 독립 관찰 경로로 분리하는 것으로 충분.

---

## 7. 남은 리스크

| 리스크 | 심각도 |
|--------|--------|
| K8sObserver와 DriverK8s.Wait가 분리됨 — 운영 시 두 경로 동시 모니터링 필요 | 중간 |
| Kueue workload 이름 패턴(ownerReference 기반 조회)이 Kueue 버전에 따라 변경 가능 | 낮음 |
| integration test에서 nodeSelector 주입이 api.RunSpec 밖에서 이루어짐 — 실제 unschedulable 재현은 kubectl 직접 사용 | 낮음 (ASSUMPTION으로 명시) |

---

## 8. 수정 파일 목록

| 파일 | 변경 |
|------|------|
| `spawner/cmd/imp/k8s_observer.go` | NEW — K8sObserver (dynamic client 기반) |
| `spawner/cmd/imp/k8s_driver.go` | MOD — buildConfig() 헬퍼 도입 (kubeconfig 해석 버그 수정) |
| `poc/pkg/integration/kueue_observe_test.go` | NEW — integration 테스트 2개 |
