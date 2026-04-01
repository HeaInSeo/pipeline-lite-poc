//go:build redis

// cmd/ingress: Redis Streams ingress → RunGate → dag-go → BoundedDriver → K8s Job
//
// Sprint 6 PoC: 기존 main path(RunGate → dag-go → BoundedDriver → K8s) 앞단에
// Redis Streams ingress를 얇게 붙인다.
//
// 핵심 흐름:
//
//	XADD(poc:runs) → Dispatcher.Consume() → gate.Admit(runID, runFn)
//	  → dag-go → SpawnerNode → BoundedDriver → DriverK8s → K8s Job
//	  → gate.Admit() returns → q.Ack()
//
// Ack 의미론:
//
//	gate.Admit()는 runFn(dag.Start + dag.Wait)이 완료될 때까지 블로킹한다.
//	따라서 Admit()가 nil을 반환 = 파이프라인 전체 완료를 의미한다.
//	q.Ack()는 Admit() 성공 반환 후에만 호출한다.
//	Admit() 실패 시 Ack 하지 않으므로 PEL에 메시지가 남아 XCLAIM 복구가 가능하다.
//
// 사용법:
//
//	# Redis 기동
//	docker run -d --name poc-redis -p 6379:6379 redis:7-alpine
//
//	# 수동 run 주입
//	redis-cli XADD poc:runs '*' run_id my-run-001 payload '{}'
//
//	# Dispatcher 실행 (한 건 처리 후 종료)
//	go run -tags redis ./cmd/ingress/ --once
//
//	# run 주입 + Dispatcher 동시 실행 (테스트 편의)
//	go run -tags redis ./cmd/ingress/ --produce --once
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	daggo "github.com/seoyhaein/dag-go"
	"github.com/seoyhaein/poc/pkg/adapter"
	"github.com/seoyhaein/poc/pkg/ingress"
	"github.com/seoyhaein/poc/pkg/queue"
	"github.com/seoyhaein/poc/pkg/store"
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
	"github.com/seoyhaein/spawner/pkg/driver"
)

const (
	redisAddr  = "localhost:6379"
	streamKey  = "poc:runs"
	groupName  = "poc-workers"
	consumerID = "dispatcher-1"

	pvcName    = "poc-shared-pvc"
	mountPath  = "/data"
	queueLabel = "poc-standard-lq"
	namespace  = "default"
	dataRoot   = "/data/ingress"
	storeFile  = "/tmp/ingress-runstore.json"
	defaultSem = 4
)

func main() {
	var (
		produce bool
		once    bool
		sem     int
		runID   string
	)
	flag.BoolVar(&produce, "produce", false,
		"Dispatcher 실행 전 test run 1건을 Redis에 XADD한다")
	flag.BoolVar(&once, "once", false,
		"메시지 1건 처리 후 Dispatcher를 종료한다")
	flag.IntVar(&sem, "sem", defaultSem,
		"BoundedDriver max concurrent K8s Job creates")
	flag.StringVar(&runID, "run-id", "",
		"--produce 시 사용할 run ID (기본: auto-generated)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// ── Redis queue ───────────────────────────────────────────────────────────
	q, err := queue.NewRedisRunQueue(ctx, redisAddr, streamKey, groupName, consumerID)
	if err != nil {
		log.Fatalf("[ingress] redis: %v", err)
	}
	defer q.Close()
	log.Printf("[ingress] redis ready  addr=%s stream=%s group=%s", redisAddr, streamKey, groupName)

	// ── RunStore ──────────────────────────────────────────────────────────────
	rs, err := store.NewJsonRunStore(storeFile)
	if err != nil {
		log.Fatalf("[ingress] runstore: %v", err)
	}

	// ── Driver ────────────────────────────────────────────────────────────────
	var innerDrv driver.Driver
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig(namespace, kubeconfig)
	if k8sErr != nil {
		log.Printf("[ingress] WARN: K8s unavailable (%v) — NopDriver", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}
	drv := imp.NewBoundedDriver(innerDrv, sem)
	log.Printf("[ingress] driver ready  K8s=%v sem=%d", k8sErr == nil, sem)

	gate := ingress.NewRunGate(rs)

	// ── Optional: inject one test message ────────────────────────────────────
	if produce {
		if runID == "" {
			runID = generateRunID()
		}
		entryID, err := q.Enqueue(ctx, runID)
		if err != nil {
			log.Fatalf("[ingress] produce: %v", err)
		}
		log.Printf("[ingress] produced  runID=%s entryID=%s", runID, entryID)
	}

	// ── Dispatcher ────────────────────────────────────────────────────────────
	log.Printf("[ingress] dispatcher starting  once=%v", once)
	dispatch(ctx, q, gate, drv, once)
}

// dispatch consumes messages from Redis Streams and drives the pipeline.
//
// Ack 정책 (at-least-once):
//   - gate.Admit()는 dag.Wait()까지 블로킹하는 동기 경로이다.
//   - Admit() nil 반환 = 파이프라인 전체 완료.
//   - XACK는 Admit() 성공 후에만 호출한다.
//   - Admit() 실패 → Ack 하지 않음 → PEL 잔류 → 향후 XCLAIM 복구 가능.
func dispatch(ctx context.Context, q *queue.RedisRunQueue, gate *ingress.RunGate, drv driver.Driver, once bool) {
	for {
		if ctx.Err() != nil {
			log.Printf("[dispatcher] context done: %v", ctx.Err())
			return
		}

		entryID, msg, err := q.Consume(ctx)
		if errors.Is(err, queue.ErrStreamEmpty) {
			continue
		}
		if err != nil {
			log.Printf("[dispatcher] consume error: %v — retrying in 500ms", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		log.Printf("[dispatcher] received  runID=%s entryID=%s", msg.RunID, entryID)

		admitErr := gate.Admit(ctx, msg.RunID, func(ctx context.Context) error {
			return runWideFanout3(ctx, msg.RunID, drv)
		})

		if admitErr != nil {
			// 파이프라인 실패: Ack 하지 않는다.
			// PEL에 메시지가 남아 있으므로 XCLAIM으로 재시도 가능.
			log.Printf("[dispatcher] run FAILED  runID=%s err=%v — NOT acking (PEL retains)", msg.RunID, admitErr)
		} else {
			if ackErr := q.Ack(ctx, entryID); ackErr != nil {
				log.Printf("[dispatcher] WARN: ack failed  entryID=%s err=%v", entryID, ackErr)
			} else {
				log.Printf("[dispatcher] XACK ok  entryID=%s runID=%s", entryID, msg.RunID)
			}
		}

		// XPENDING 확인 (검증용 로그)
		if pending, pErr := q.Pending(ctx); pErr == nil {
			log.Printf("[dispatcher] XPENDING count=%d", pending)
		}

		if once {
			return
		}
	}
}

// runWideFanout3 runs:  setup → B1/B2/B3 → collect
//
// 파이프라인 구조:
//
//	start → setup → B1 ─┐
//	                B2 ─┼→ collect → end
//	                B3 ─┘
//
// cmd/stress/patterns.go의 wideFanout 로직을 workers=3으로 단순화한 인라인 버전.
// Sprint 6 목적(ingress 연결 검증)에 맞게 불필요한 계측 코드를 제거했다.
func runWideFanout3(ctx context.Context, runID string, drv driver.Driver) error {
	const workers = 3
	pBase := dataRoot + "/" + runID
	mount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}

	dag, err := initDag()
	if err != nil {
		return fmt.Errorf("InitDag: %w", err)
	}

	// ── Create nodes ──────────────────────────────────────────────────────────
	nodeSetup := dag.CreateNode("setup")
	if nodeSetup == nil {
		return fmt.Errorf("CreateNode(setup) nil")
	}
	bNodes := make([]interface{ SetRunner(daggo.Runnable) bool }, workers)
	for i := 1; i <= workers; i++ {
		n := dag.CreateNode(fmt.Sprintf("b%d", i))
		if n == nil {
			return fmt.Errorf("CreateNode(b%d) nil", i)
		}
		bNodes[i-1] = n
	}
	nodeCollect := dag.CreateNode("collect")
	if nodeCollect == nil {
		return fmt.Errorf("CreateNode(collect) nil")
	}

	// ── Edges ─────────────────────────────────────────────────────────────────
	if err := dag.AddEdge(daggo.StartNode, "setup"); err != nil {
		return fmt.Errorf("AddEdge start→setup: %w", err)
	}
	for i := 1; i <= workers; i++ {
		if err := dag.AddEdge("setup", fmt.Sprintf("b%d", i)); err != nil {
			return fmt.Errorf("AddEdge setup→b%d: %w", i, err)
		}
		if err := dag.AddEdge(fmt.Sprintf("b%d", i), "collect"); err != nil {
			return fmt.Errorf("AddEdge b%d→collect: %w", i, err)
		}
	}

	// ── Runners ───────────────────────────────────────────────────────────────
	// setup이 모든 디렉토리를 미리 생성한다 (B 노드들이 병렬 실행 전에 디렉토리 필요).
	var mkdirs []string
	mkdirs = append(mkdirs, pBase+"/setup-output")
	mkdirs = append(mkdirs, pBase+"/collect-output")
	for i := 1; i <= workers; i++ {
		mkdirs = append(mkdirs, fmt.Sprintf("%s/b-output/worker-%d", pBase, i))
	}

	if !nodeSetup.SetRunner(&adapter.SpawnerNode{
		Driver: drv,
		Spec:   ingressSpecSetup("setup", pBase+"/setup-output", mkdirs, runID, mount),
	}) {
		return fmt.Errorf("SetRunner(setup) failed")
	}

	for i := 1; i <= workers; i++ {
		id := fmt.Sprintf("b%d", i)
		outDir := fmt.Sprintf("%s/b-output/worker-%d", pBase, i)
		if !bNodes[i-1].SetRunner(&adapter.SpawnerNode{
			Driver: drv,
			Spec:   ingressSpecWorker(id, outDir, runID, mount),
		}) {
			return fmt.Errorf("SetRunner(%s) failed", id)
		}
	}

	if !nodeCollect.SetRunner(&adapter.SpawnerNode{
		Driver: drv,
		Spec:   ingressSpecCollect("collect", pBase, workers, runID, mount),
	}) {
		return fmt.Errorf("SetRunner(collect) failed")
	}

	return execDag(ctx, dag)
}

// ── DAG execution ─────────────────────────────────────────────────────────────

// initDag: DefaultTimeout=0 으로 설정하여 long-running 노드가 per-node deadline에
// 걸리지 않도록 한다. caller ctx(30분)가 전체 데드라인을 담당한다.
// (Sprint 5에서 발견된 dag-go DefaultTimeout=30s 버그 대응)
func initDag() (*daggo.Dag, error) {
	return daggo.InitDagWithOptions(daggo.WithDefaultTimeout(0))
}

func execDag(ctx context.Context, dag *daggo.Dag) error {
	if err := dag.FinishDag(); err != nil {
		return fmt.Errorf("FinishDag: %w", err)
	}
	if !dag.ConnectRunner() {
		return fmt.Errorf("ConnectRunner failed")
	}
	if !dag.GetReady(ctx) {
		return fmt.Errorf("GetReady failed")
	}
	if !dag.Start() {
		return fmt.Errorf("dag Start failed")
	}
	if !dag.Wait(ctx) {
		return fmt.Errorf("dag Wait: pipeline did not succeed")
	}
	return nil
}

// ── K8s Job spec builders ─────────────────────────────────────────────────────

// ingressNodeID returns a K8s Job name ≤63 chars.
// Format: "i-{runID}-{node}"  (prefix "i-" = ingress)
func ingressNodeID(runID, node string) string {
	id := "i-" + runID + "-" + node
	if len(id) <= 63 {
		return id
	}
	suffix := "-" + node
	prefix := "i-"
	maxLen := 63 - len(prefix) - len(suffix)
	if maxLen < 1 {
		maxLen = 1
	}
	return prefix + runID[:maxLen] + suffix
}

// ingressSpecSetup: 모든 디렉토리를 생성하고 setup-output/done.txt를 기록한다.
func ingressSpecSetup(nodeID, outDir string, mkdirs []string, runID string, mount api.Mount) api.RunSpec {
	var lines []string
	lines = append(lines, "set -e")
	for _, d := range mkdirs {
		lines = append(lines, "mkdir -p "+d)
	}
	lines = append(lines,
		`echo "node=`+nodeID+` started=$(date -u +%Y-%m-%dT%H:%M:%SZ)" > `+outDir+`/done.txt`,
		`echo "runID=`+runID+`" >> `+outDir+`/done.txt`,
		`echo "[`+nodeID+`] setup done"`,
	)
	return api.RunSpec{
		RunID:     ingressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueLabel},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// ingressSpecWorker: B 워커 노드. outDir/done.txt를 기록한다.
func ingressSpecWorker(nodeID, outDir string, runID string, mount api.Mount) api.RunSpec {
	lines := []string{
		"set -e",
		`echo "node=` + nodeID + ` started=$(date -u +%Y-%m-%dT%H:%M:%SZ)" > ` + outDir + `/done.txt`,
		`echo "runID=` + runID + `" >> ` + outDir + `/done.txt`,
		`echo "[` + nodeID + `] worker done"`,
	}
	return api.RunSpec{
		RunID:     ingressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueLabel},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// ingressSpecCollect: B 워커 3개의 done.txt를 검증하고 최종 report를 기록한다.
func ingressSpecCollect(nodeID, pBase string, workers int, runID string, mount api.Mount) api.RunSpec {
	outDir := pBase + "/collect-output"
	var lines []string
	lines = append(lines, "set -e")
	for i := 1; i <= workers; i++ {
		lines = append(lines, fmt.Sprintf(
			"test -f %s/b-output/worker-%d/done.txt || { echo '[%s] missing worker-%d'; exit 1; }",
			pBase, i, nodeID, i))
	}
	lines = append(lines,
		"REPORT="+outDir+"/report.txt",
		`echo '=== ingress collect ===' > "$REPORT"`,
		fmt.Sprintf(`echo 'runID=%s' >> "$REPORT"`, runID),
		fmt.Sprintf(`echo 'workers=%d' >> "$REPORT"`, workers),
		`echo "collected_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$REPORT"`,
	)
	for i := 1; i <= workers; i++ {
		lines = append(lines,
			fmt.Sprintf(`cat %s/b-output/worker-%d/done.txt >> "$REPORT"`, pBase, i))
	}
	lines = append(lines, `echo '[collect] report written'`)
	return api.RunSpec{
		RunID:     ingressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueLabel},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

func generateRunID() string {
	t := time.Now().UTC()
	return fmt.Sprintf("%s-%03d", t.Format("20060102-150405"), t.Nanosecond()/1e6)
}
