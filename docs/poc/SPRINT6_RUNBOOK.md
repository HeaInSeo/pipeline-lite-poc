# Sprint 6 Runbook: Redis Streams Ingress PoC

**목적**: 이 runbook의 명령어를 순서대로 실행하면 Sprint 6 end-to-end 흐름을 재현할 수 있다.

재현 경로는 두 가지다:
- **경로 A** (`--produce --once`): Dispatcher가 run 주입과 실행을 한 프로세스에서 처리한다. 빠른 확인용.
- **경로 B** (manual XADD): Dispatcher와 Producer를 분리하여 Redis Streams ingress 경계를 선명하게 보여준다.

---

## 전제 조건

- kind cluster `poc` 가 실행 중이어야 한다
- `poc-shared-pvc` PVC가 `default` namespace에 존재해야 한다
- Kueue + LocalQueue (`poc-standard-lq`) 가 구성되어 있어야 한다
- Docker가 호스트에서 사용 가능해야 한다
- 원격 서버에서 실행 중인 경우 Tailscale 접속 후 SSH 터미널에서 수행

---

## Step 1: Redis 기동 확인

```bash
# Redis 컨테이너 실행 (이미 실행 중이면 skip)
# 127.0.0.1 바인딩: 동일 호스트 내부에서만 접근 가능 (불필요한 인터페이스 노출 방지)
docker run -d --name poc-redis -p 127.0.0.1:6379:6379 redis:7-alpine

# 연결 확인
redis-cli -h 127.0.0.1 -p 6379 ping
# 출력 예상: PONG

# 컨테이너 상태 확인
docker ps --filter name=poc-redis
```

Redis를 중지/재시작할 경우:

```bash
docker stop poc-redis && docker rm poc-redis
docker run -d --name poc-redis -p 127.0.0.1:6379:6379 redis:7-alpine
```

---

## Step 2: kind cluster 상태 확인

```bash
kubectl cluster-info --context kind-poc
kubectl get nodes
kubectl get pvc poc-shared-pvc -n default
kubectl get localqueue -n default
```

---

## 경로 A: --produce --once (통합 방식)

run 주입과 Dispatcher 실행을 한 프로세스에서 처리한다.

```bash
cd /opt/go/src/github.com/HeaInSeo/poc
go run -tags redis ./cmd/ingress/ --produce --once
```

정상 출력 예시:

```
[ingress] redis ready  addr=localhost:6379 stream=poc:runs group=poc-workers
[ingress] driver ready  K8s=true sem=4
[ingress] produced  runID=20260401-120000-000 entryID=1743476400000-0
[ingress] dispatcher starting  once=true fail=false
[dispatcher] received  runID=20260401-120000-000 entryID=1743476400000-0
[ingress] run 20260401-120000-000 → queued
[ingress] run 20260401-120000-000 → admitted-to-dag
[ingress] run 20260401-120000-000 → running
[ingress] run 20260401-120000-000 → finished
[dispatcher] XACK ok  entryID=1743476400000-0 runID=20260401-120000-000
[dispatcher] XPENDING count=0
```

---

## 경로 B: Dispatcher 대기 + manual XADD (분리 방식)

Redis Streams ingress 경계를 선명하게 보여준다.
터미널 두 개가 필요하다.

### B-1. 터미널 A: Dispatcher 대기 시작

```bash
cd /opt/go/src/github.com/HeaInSeo/poc
go run -tags redis ./cmd/ingress/ --once
```

`[ingress] dispatcher starting  once=true` 로그가 출력되면 메시지 대기 상태다.

### B-2. 터미널 B: redis-cli로 run 주입

`Consume()`은 Redis entry의 `payload` 필드를 `RunMessage` JSON으로 파싱한다.
따라서 XADD 시 `payload` 필드에 정확한 JSON 형식이 필요하다.

```bash
# run ID를 직접 지정하는 경우
RUN_ID="manual-$(date -u +%Y%m%d-%H%M%S)"

redis-cli -h 127.0.0.1 XADD poc:runs '*' \
  run_id "$RUN_ID" \
  payload "{\"run_id\":\"$RUN_ID\",\"created_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
```

고정 ID로 확인하는 경우 (복붙 가능):

```bash
redis-cli -h 127.0.0.1 XADD poc:runs '*' \
  run_id "manual-20260401-001" \
  payload '{"run_id":"manual-20260401-001","created_at":"2026-04-01T12:00:00Z"}'
```

### B-3. 터미널 A에서 Dispatcher 동작 확인

XADD 직후 터미널 A에서 다음이 출력되어야 한다:

```
[dispatcher] received  runID=manual-20260401-001 entryID=...
[ingress] run manual-20260401-001 → queued
[ingress] run manual-20260401-001 → admitted-to-dag
[ingress] run manual-20260401-001 → running
[ingress] run manual-20260401-001 → finished
[dispatcher] XACK ok  entryID=... runID=manual-20260401-001
[dispatcher] XPENDING count=0
```

> **주의**: `payload` 형식이 틀리면 `redis: unmarshal payload` 에러가 발생한다.
> 이 경우 메시지는 PEL에 남고 Dispatcher는 루프를 계속 돌지 않는다 (소비 자체가 실패).
> `redis-cli DEL poc:runs` 로 스트림을 초기화한 후 재시도한다.

---

## 실패 경로 검증: PEL 잔류 확인

**목적**: 파이프라인 실패 시 XACK가 호출되지 않아 메시지가 PEL에 남는다는 것을 검증한다.

`--fail` 플래그는 K8s Job을 생성하지 않고 즉시 synthetic 에러를 반환하는 dev-only 경로다.

### 실행

```bash
# 스트림 초기화 (이전 상태 제거)
redis-cli -h 127.0.0.1 DEL poc:runs

# 실패 run 1건 주입 후 Dispatcher 실행 (실패 경로)
go run -tags redis ./cmd/ingress/ --produce --once --fail
```

### 예상 출력

```
[ingress] produced  runID=20260401-130000-000 entryID=...
[ingress] dispatcher starting  once=true fail=true
[dispatcher] received  runID=20260401-130000-000 entryID=...
[ingress] run 20260401-130000-000 → queued
[ingress] run 20260401-130000-000 → admitted-to-dag
[ingress] run 20260401-130000-000 → running
[ingress] run 20260401-130000-000 → canceled: synthetic failure (--fail flag): PEL retention test
[dispatcher] run FAILED  runID=20260401-130000-000 err=... — NOT acking (PEL retains)
[dispatcher] XPENDING count=1
```

`XACK ok` 로그가 **없어야** 하고, `XPENDING count=1` 이어야 한다.

### redis-cli로 직접 확인

```bash
redis-cli -h 127.0.0.1 XPENDING poc:runs poc-workers - + 10
# 예상 출력:
# 1) 1) "1743476400000-0"
#       2) "dispatcher-1"
#       3) (integer) <idle_ms>
#       4) (integer) 1
```

PEL에 entry가 1개 남아 있으면 검증 완료다.
XCLAIM을 구현하지 않아 자동 재시도는 없지만, "유실되지 않음"은 이 결과로 증명된다.

### 정리 (다음 테스트를 위해)

```bash
redis-cli -h 127.0.0.1 DEL poc:runs
```

---

## Step 5: K8s Job 완료 확인 (happy path 전용)

```bash
# Job 목록 (i- prefix = ingress run)
kubectl get jobs -n default | grep "^i-"

# 예상 출력:
# i-{runID}-setup    Complete   1/1   ...
# i-{runID}-b1       Complete   1/1   ...
# i-{runID}-b2       Complete   1/1   ...
# i-{runID}-b3       Complete   1/1   ...
# i-{runID}-collect  Complete   1/1   ...

# Kueue workload 상태
kubectl get workloads -n default | grep "^job-i-"
```

---

## Step 6: XPENDING 최종 검증 (happy path)

```bash
redis-cli -h 127.0.0.1 XPENDING poc:runs poc-workers - + 10
# 정상 완주 후: (empty list or array)
```

Dispatcher 로그에서도 확인:

```
[dispatcher] XPENDING count=0
```

---

## Step 7: RunStore 상태 확인

```bash
cat /tmp/ingress-runstore.json
```

`"state":"finished"` 항목이 있으면 RunGate 상태 전이 완료.

---

## Step 8: PVC 결과물 확인 (선택)

```bash
# 완료된 collect job 로그
kubectl logs -n default job/i-{runID}-collect
# 예상: [collect] report written
```

---

## 환경 정리

```bash
# Job 삭제
kubectl delete jobs -n default $(kubectl get jobs -n default -o name | grep "job/i-" | xargs)

# Redis 스트림 초기화
redis-cli -h 127.0.0.1 DEL poc:runs

# Redis 컨테이너 중지 (필요 시)
docker stop poc-redis
```

---

## 트러블슈팅

| 증상 | 원인 | 조치 |
|------|------|------|
| `redis connect: dial tcp` | Redis 미실행 | `docker run -d --name poc-redis -p 127.0.0.1:6379:6379 redis:7-alpine` |
| `redis: unmarshal payload` | XADD payload JSON 형식 오류 | `redis-cli DEL poc:runs` 후 올바른 형식으로 재시도 |
| `K8s unavailable` | kubeconfig 없음/kind 미실행 | `kind get clusters`, `kubectl cluster-info` |
| `XPENDING count=1` (happy path 후) | Admit() 실패로 Ack 미호출 | Dispatcher 로그에서 에러 확인 |
| Job이 `Pending` 상태 유지 | Kueue quota 부족 | `kubectl get workloads -n default` |
| `dag Wait: pipeline did not succeed` | K8s Job 실패 | `kubectl logs job/i-{runID}-{node}` |
