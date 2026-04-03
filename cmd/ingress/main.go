//go:build redis

// cmd/ingress: Redis Streams ingress → RunGate → dag-go → BoundedDriver → K8s Job
//
// Sprint 6 PoC: 기존 main path(RunGate → dag-go → BoundedDriver → K8s) 앞단에
// Redis Streams ingress를 얇게 붙인다.
//
// Sprint 7: --pipeline 플래그로 두 경로를 선택한다.
//
//	--pipeline fanout    : Sprint 6 happy path (wideFanout3 / setup→B1/B2/B3→collect)
//	--pipeline specimen  : canonical specimen v0.1 (prepare→worker-1/2/3→collect)
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
//	docker run -d --name poc-redis -p 127.0.0.1:6379:6379 redis:7-alpine
//
//	# 수동 run 주입
//	redis-cli XADD poc:runs '*' run_id my-run-001 payload '{}'
//
//	# Dispatcher 실행 (한 건 처리 후 종료)
//	go run -tags redis ./cmd/ingress/ --once
//
//	# Sprint 6 fanout 경로
//	go run -tags redis ./cmd/ingress/ --produce --once --pipeline fanout
//
//	# Sprint 7 specimen 경로
//	go run -tags redis ./cmd/ingress/ --produce --once --pipeline specimen
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
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
		produce        bool
		once           bool
		fail           bool
		sem            int
		runID          string
		pipeline       string
		maxRuns        int
		concurrentRuns int
	)
	flag.BoolVar(&produce, "produce", false,
		"Dispatcher 실행 전 test run 1건을 Redis에 XADD한다")
	flag.BoolVar(&once, "once", false,
		"메시지 1건 처리 후 Dispatcher를 종료한다")
	flag.BoolVar(&fail, "fail", false,
		"[dev] 수신한 run을 의도적으로 실패시켜 PEL 잔류를 검증한다 (K8s Job 미생성)")
	flag.IntVar(&sem, "sem", defaultSem,
		"BoundedDriver max concurrent K8s Job creates")
	flag.StringVar(&runID, "run-id", "",
		"--produce 시 사용할 run ID (기본: auto-generated)")
	flag.StringVar(&pipeline, "pipeline", "fanout",
		"실행할 파이프라인 경로: fanout (Sprint 6 기준선) | specimen (canonical specimen v0.1)")
	flag.IntVar(&maxRuns, "max-runs", 0,
		"N개 run 처리 후 Dispatcher를 정상 종료한다 (0 = 무제한, --once와 독립)")
	flag.IntVar(&concurrentRuns, "concurrent-runs", 1,
		"동시에 처리할 최대 run 수 (기본 1 = sequential, Sprint 8 동작 유지)")
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
	log.Printf("[ingress] dispatcher starting  once=%v fail=%v pipeline=%s max-runs=%d concurrent-runs=%d",
		once, fail, pipeline, maxRuns, concurrentRuns)
	dispatch(ctx, q, gate, drv, once, fail, pipeline, maxRuns, concurrentRuns)
}

// dispatch consumes messages from Redis Streams and drives the pipeline.
//
// Ack 정책 (at-least-once) — concurrent drain에서도 동일하게 유지됨:
//   - gate.Admit()는 dag.Wait()까지 블로킹하는 동기 경로이다.
//   - Admit() nil 반환 = 파이프라인 전체 완료.
//   - XACK는 Admit() 성공 후에만 호출한다 (goroutine 내부에서도 동일).
//   - Admit() 실패 → Ack 하지 않음 → PEL 잔류 → 향후 XCLAIM 복구 가능.
//
// concurrent drain (--concurrent-runs N > 1):
//   - 메시지 Consume은 main goroutine 단독 수행.
//   - 각 run은 독립 goroutine에서 gate.Admit() → pipelineFn → XACK 수행.
//   - semaphore(chan struct{}, N)로 동시 실행 run 수를 N으로 제한.
//   - sync.WaitGroup으로 모든 in-flight goroutine이 완료될 때까지 대기 후 종료.
//   - entryID/runID는 goroutine 인자로 명시 전달 (loop variable capture 방지).
func dispatch(ctx context.Context, q *queue.RedisRunQueue, gate *ingress.RunGate, drv driver.Driver, once, fail bool, pipeline string, maxRuns, concurrentRuns int) {
	// pipeline 라우팅: fanout = Sprint 6 기준선, specimen = canonical specimen v0.1
	var pipelineFn func(ctx context.Context, runID string, drv driver.Driver) error
	switch pipeline {
	case "specimen":
		pipelineFn = runSpecimenPipeline
	default: // "fanout" 또는 미지정
		pipelineFn = runWideFanout3
	}

	// semaphore: 동시에 실행 중인 run 수를 concurrentRuns 이하로 제한한다.
	// concurrentRuns=1(기본값)이면 항상 1개 슬롯만 존재 → sequential 동작.
	sem := make(chan struct{}, concurrentRuns)
	var wg sync.WaitGroup
	var dispatched int // Consume → goroutine 시작 수. main goroutine에서만 접근.

	for {
		if ctx.Err() != nil {
			log.Printf("[dispatcher] context done: %v", ctx.Err())
			break
		}
		if maxRuns > 0 && dispatched >= maxRuns {
			break
		}

		// 슬롯을 Consume 전에 획득한다.
		// 이렇게 하면 in-flight goroutine 수가 concurrentRuns에 도달했을 때
		// 다음 메시지를 소비하지 않으므로 "소비했지만 처리 못 한" 메시지가 PEL에
		// 추가로 쌓이지 않는다. concurrentRuns=1이면 sequential과 동일한
		// XPENDING 패턴을 유지한다.
		select {
		case sem <- struct{}{}:
			// 슬롯 획득 성공
		case <-ctx.Done():
			log.Printf("[dispatcher] context done waiting for slot: %v", ctx.Err())
			break
		}

		// maxRuns 재확인: 슬롯 대기 중 dispatched가 변하지 않지만(main goroutine 단독)
		// ctx 만료 등으로 break한 뒤 여기에 도달하면 슬롯을 즉시 반환하고 종료한다.
		if ctx.Err() != nil {
			<-sem
			break
		}
		if maxRuns > 0 && dispatched >= maxRuns {
			<-sem
			break
		}

		entryID, msg, err := q.Consume(ctx)
		if errors.Is(err, queue.ErrStreamEmpty) {
			<-sem // 메시지 없음: 슬롯 즉시 반환
			continue
		}
		if err != nil {
			<-sem // 에러: 슬롯 즉시 반환
			log.Printf("[dispatcher] consume error: %v — retrying in 500ms", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		log.Printf("[dispatcher] received  runID=%s entryID=%s", msg.RunID, entryID)

		dispatched++
		wg.Add(1)

		// goroutine 인자로 명시 전달 → loop variable capture 버그 방지.
		go func(runID, eid string) {
			defer wg.Done()
			defer func() { <-sem }() // 완료 시 슬롯 반환

			admitErr := gate.Admit(ctx, runID, func(ctx context.Context) error {
				if fail {
					// [dev] --fail: K8s Job을 생성하지 않고 즉시 에러 반환.
					// concurrent 경로에서도 PEL 잔류 의미론 동일하게 유지된다.
					return fmt.Errorf("synthetic failure (--fail flag): PEL retention test")
				}
				return pipelineFn(ctx, runID, drv)
			})

			if admitErr != nil {
				// 실패: Ack 하지 않는다. PEL 잔류 → XCLAIM 복구 가능.
				log.Printf("[dispatcher] run FAILED  runID=%s err=%v — NOT acking (PEL retains)", runID, admitErr)
			} else {
				if ackErr := q.Ack(ctx, eid); ackErr != nil {
					log.Printf("[dispatcher] WARN: ack failed  entryID=%s err=%v", eid, ackErr)
				} else {
					log.Printf("[dispatcher] XACK ok  entryID=%s runID=%s", eid, runID)
				}
			}

			// XPENDING 확인 (검증용 로그)
			// concurrent 모드에서는 여러 goroutine이 동시에 출력할 수 있음.
			// 값은 "현재 순간의 스냅샷"이므로 interleave 로그를 주의해서 해석한다.
			if pending, pErr := q.Pending(ctx); pErr == nil {
				log.Printf("[dispatcher] XPENDING count=%d", pending)
			}
		}(msg.RunID, entryID)

		if once {
			break
		}
	}

	// 모든 in-flight goroutine이 완료될 때까지 대기한다.
	// main이 먼저 종료되면 XACK 없이 프로세스가 끝나는 문제를 방지한다.
	wg.Wait()
	if maxRuns > 0 && dispatched >= maxRuns {
		log.Printf("[dispatcher] max-runs=%d reached — exiting normally", maxRuns)
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

// ── Canonical specimen pipeline v0.1 ─────────────────────────────────────────

// runSpecimenPipeline: canonical specimen pipeline v0.1
//
// Shape: prepare → worker-1/worker-2/worker-3 → collect
//
//	start → prepare → worker-1 ─┐
//	                  worker-2 ─┼→ collect → end
//	                  worker-3 ─┘
//
// Sprint 7 목적: dummy app이 반복 제출할 canonical workload baseline.
// content dependency 없음 — 각 노드는 독립적으로 자신의 output dir에 done.txt를 기록한다.
func runSpecimenPipeline(ctx context.Context, runID string, drv driver.Driver) error {
	const workers = 3
	pBase := dataRoot + "/" + runID
	mount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}

	dag, err := initDag()
	if err != nil {
		return fmt.Errorf("InitDag: %w", err)
	}

	// ── Create nodes ──────────────────────────────────────────────────────────
	nodePrepare := dag.CreateNode("prepare")
	if nodePrepare == nil {
		return fmt.Errorf("CreateNode(prepare) nil")
	}
	wNodes := make([]interface{ SetRunner(daggo.Runnable) bool }, workers)
	for i := 1; i <= workers; i++ {
		n := dag.CreateNode(fmt.Sprintf("worker-%d", i))
		if n == nil {
			return fmt.Errorf("CreateNode(worker-%d) nil", i)
		}
		wNodes[i-1] = n
	}
	nodeCollect := dag.CreateNode("collect")
	if nodeCollect == nil {
		return fmt.Errorf("CreateNode(collect) nil")
	}

	// ── Edges ─────────────────────────────────────────────────────────────────
	if err := dag.AddEdge(daggo.StartNode, "prepare"); err != nil {
		return fmt.Errorf("AddEdge start→prepare: %w", err)
	}
	for i := 1; i <= workers; i++ {
		if err := dag.AddEdge("prepare", fmt.Sprintf("worker-%d", i)); err != nil {
			return fmt.Errorf("AddEdge prepare→worker-%d: %w", i, err)
		}
		if err := dag.AddEdge(fmt.Sprintf("worker-%d", i), "collect"); err != nil {
			return fmt.Errorf("AddEdge worker-%d→collect: %w", i, err)
		}
	}

	// ── Runners ───────────────────────────────────────────────────────────────
	// prepare가 모든 output 디렉토리를 미리 생성한다.
	var mkdirs []string
	mkdirs = append(mkdirs, pBase+"/prepare-output")
	mkdirs = append(mkdirs, pBase+"/collect-output")
	for i := 1; i <= workers; i++ {
		mkdirs = append(mkdirs, fmt.Sprintf("%s/worker-%d-output", pBase, i))
	}

	if !nodePrepare.SetRunner(&adapter.SpawnerNode{
		Driver: drv,
		Spec:   specimenSpecPrepare("prepare", pBase+"/prepare-output", mkdirs, runID, mount),
	}) {
		return fmt.Errorf("SetRunner(prepare) failed")
	}

	for i := 1; i <= workers; i++ {
		id := fmt.Sprintf("worker-%d", i)
		outDir := fmt.Sprintf("%s/worker-%d-output", pBase, i)
		if !wNodes[i-1].SetRunner(&adapter.SpawnerNode{
			Driver: drv,
			Spec:   specimenSpecWorker(id, outDir, runID, mount),
		}) {
			return fmt.Errorf("SetRunner(%s) failed", id)
		}
	}

	if !nodeCollect.SetRunner(&adapter.SpawnerNode{
		Driver: drv,
		Spec:   specimenSpecCollect("collect", pBase, workers, runID, mount),
	}) {
		return fmt.Errorf("SetRunner(collect) failed")
	}

	return execDag(ctx, dag)
}

// specimenSpecPrepare: 모든 output 디렉토리를 생성하고 prepare-output/done.txt를 기록한다.
func specimenSpecPrepare(nodeID, outDir string, mkdirs []string, runID string, mount api.Mount) api.RunSpec {
	var lines []string
	lines = append(lines, "set -e")
	for _, d := range mkdirs {
		lines = append(lines, "mkdir -p "+d)
	}
	lines = append(lines,
		`echo "node=`+nodeID+` started=$(date -u +%Y-%m-%dT%H:%M:%SZ)" > `+outDir+`/done.txt`,
		`echo "runID=`+runID+`" >> `+outDir+`/done.txt`,
		`echo "[`+nodeID+`] prepare done"`,
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

// specimenSpecWorker: 워커 노드. worker-N-output/done.txt를 기록한다.
func specimenSpecWorker(nodeID, outDir string, runID string, mount api.Mount) api.RunSpec {
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

// specimenSpecCollect: worker-N-output/done.txt를 검증하고 최종 report를 기록한다.
func specimenSpecCollect(nodeID, pBase string, workers int, runID string, mount api.Mount) api.RunSpec {
	outDir := pBase + "/collect-output"
	var lines []string
	lines = append(lines, "set -e")
	for i := 1; i <= workers; i++ {
		lines = append(lines, fmt.Sprintf(
			"test -f %s/worker-%d-output/done.txt || { echo '[%s] missing worker-%d'; exit 1; }",
			pBase, i, nodeID, i))
	}
	lines = append(lines,
		"REPORT="+outDir+"/report.txt",
		`echo '=== specimen collect ===' > "$REPORT"`,
		fmt.Sprintf(`echo 'runID=%s' >> "$REPORT"`, runID),
		fmt.Sprintf(`echo 'workers=%d' >> "$REPORT"`, workers),
		`echo "collected_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$REPORT"`,
	)
	for i := 1; i <= workers; i++ {
		lines = append(lines,
			fmt.Sprintf(`cat %s/worker-%d-output/done.txt >> "$REPORT"`, pBase, i))
	}
	lines = append(lines, `echo '[collect] specimen report written'`)
	return api.RunSpec{
		RunID:     ingressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueLabel},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
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
