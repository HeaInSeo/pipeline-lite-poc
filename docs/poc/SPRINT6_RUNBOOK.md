# Sprint 6 Runbook: Redis Streams Ingress PoC

**목적**: 이 runbook의 명령어를 순서대로 실행하면 Sprint 6 end-to-end 흐름을 재현할 수 있다.

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
docker run -d --name poc-redis -p 6379:6379 redis:7-alpine

# 연결 확인
redis-cli -h localhost -p 6379 ping
# 출력 예상: PONG

# 컨테이너 상태 확인
docker ps --filter name=poc-redis
```

Redis를 중지/재시작할 경우:

```bash
docker stop poc-redis && docker rm poc-redis
docker run -d --name poc-redis -p 6379:6379 redis:7-alpine
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

## Step 3: Dispatcher 실행 (--produce --once 통합 방식)

`--produce`: Redis에 test run 1건을 XADD한다.  
`--once`: 메시지 1건 처리 후 종료한다.

```bash
cd /opt/go/src/github.com/HeaInSeo/poc
go run -tags redis ./cmd/ingress/ --produce --once
```

정상 출력 예시:

```
[ingress] redis ready  addr=localhost:6379 stream=poc:runs group=poc-workers
[ingress] driver ready  K8s=true sem=4
[ingress] produced  runID=20260401-120000-000 entryID=1743476400000-0
[ingress] dispatcher starting  once=true
[dispatcher] received  runID=20260401-120000-000 entryID=1743476400000-0
[ingress] run 20260401-120000-000 → queued
[ingress] run 20260401-120000-000 → admitted-to-dag
[ingress] run 20260401-120000-000 → running
[dispatcher] XACK ok  entryID=1743476400000-0 runID=20260401-120000-000
[dispatcher] XPENDING count=0
[ingress] run 20260401-120000-000 → finished
```

---

## Step 4: 수동 XADD 방식 (별도 run 테스트)

Dispatcher를 먼저 백그라운드로 실행하거나, `--once` 없이 실행한 뒤 별도 터미널에서 XADD:

```bash
# 터미널 A: Dispatcher 대기
go run -tags redis ./cmd/ingress/

# 터미널 B: run 주입
redis-cli XADD poc:runs '*' run_id my-run-manual-001 payload '{}'
```

---

## Step 5: K8s Job 완료 확인

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

## Step 6: XPENDING 검증

```bash
redis-cli XPENDING poc:runs poc-workers - + 10
# 정상 완주 후: (empty list or array)
# 또는: (integer) 0
```

Dispatcher 로그에서도 확인:

```
[dispatcher] XPENDING count=0
```

---

## Step 7: RunStore 상태 확인

```bash
cat /tmp/ingress-runstore.json | python3 -m json.tool
# 또는
cat /tmp/ingress-runstore.json
```

`state: "finished"` 항목이 있으면 RunGate 상태 전이 완료.

---

## Step 8: PVC 결과물 확인 (선택)

collect 노드가 report.txt를 기록했는지 확인:

```bash
# pod 이름으로 확인
kubectl get pods -n default | grep "i-"

# 완료된 collect pod의 로그
kubectl logs -n default job/i-{runID}-collect
# 예상: [collect] report written
```

---

## 환경 정리

```bash
# 이번 sprint run의 Job 삭제 (runID 부분을 실제 값으로 교체)
kubectl delete jobs -n default -l app=poc  # 또는 개별 삭제

# Redis 스트림 초기화 (다음 테스트를 위해)
redis-cli DEL poc:runs

# Redis 컨테이너 중지 (필요 시)
docker stop poc-redis
```

---

## 트러블슈팅

| 증상 | 원인 | 조치 |
|------|------|------|
| `redis connect: dial tcp` | Redis 미실행 | `docker run -d --name poc-redis -p 6379:6379 redis:7-alpine` |
| `K8s unavailable` | kubeconfig 없음/kind 미실행 | `kind get clusters`, `kubectl cluster-info` |
| `XPENDING count=1` after run | Admit() 실패로 Ack 미호출 | Dispatcher 로그에서 에러 확인 |
| Job이 `Pending` 상태 유지 | Kueue quota 부족 | `kubectl get workloads -n default` |
| `dag Wait: pipeline did not succeed` | K8s Job 실패 | `kubectl logs job/i-{runID}-{node}` |
