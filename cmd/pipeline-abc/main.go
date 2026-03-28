// cmd/pipeline-abc: A→B→C 선형 파이프라인 + shared PVC 파일 handoff
//
// Sprint 3 변경사항:
//   - RunGate + JsonRunStore: 실행 전 ingress boundary 등록 (Q1 fix)
//   - BoundedDriver(sem=3): K8s Job 생성 burst 제어 (Q2 fix)
//   - K8s init 실패 시 graceful degradation: NopDriver + StateHeld
//
// 파이프라인 구조:
//
//	start → A → B → C → end
//
// 파일 handoff (shared PVC /data):
//
//	A: /data/a.txt 기록 ("hello from node-A")
//	B: /data/a.txt 읽어 /data/b.txt 기록 ("B processed: hello from node-A")
//	C: /data/b.txt 읽어 내용 검증 후 종료 0
//
// 사용법: go run ./cmd/pipeline-abc/
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	daggo "github.com/seoyhaein/dag-go"
	"github.com/seoyhaein/poc/pkg/adapter"
	"github.com/seoyhaein/poc/pkg/ingress"
	"github.com/seoyhaein/poc/pkg/store"
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
	"github.com/seoyhaein/spawner/pkg/driver"
)

const (
	pvcName   = "poc-shared-pvc"
	mountPath = "/data"
	queueName = "poc-standard-lq"
	namespace = "default"

	// maxConcurrentK8sJobs caps simultaneous Job Create calls to the K8s API.
	// Increase for higher-throughput environments; decrease for rate-limited clusters.
	maxConcurrentK8sJobs = 3

	// runStoreFile is the durable-lite JsonRunStore backing file.
	// Survives process restart: queued/held runs are recovered on next start.
	runStoreFile = "/tmp/pipeline-abc-runstore.json"

	// pipelineRunID uniquely identifies this pipeline run in the store.
	pipelineRunID = "pipeline-abc-run-1"
)

func main() {
	// ── 1. Durable RunStore ─────────────────────────────────────────────────
	// Opens or recovers existing state. If the file doesn't exist, starts fresh.
	runStore, err := store.NewJsonRunStore(runStoreFile)
	if err != nil {
		log.Fatalf("runstore init: %v", err)
	}

	// Bootstrap: recover any runs that were held or queued before a restart.
	bootstrapRecovery(runStore)

	// ── 2. Driver: DriverK8s wrapped with BoundedDriver ─────────────────────
	// Sprint 3 wiring: BoundedDriver limits concurrent K8s Job creation to sem=3.
	// On K8s init failure: NopDriver (returns ErrK8sUnavailable) is used instead
	// of log.Fatalf. Runs stay in StateHeld until K8s is available.
	var innerDrv driver.Driver
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig(namespace, kubeconfig)
	if k8sErr != nil {
		log.Printf("[pipeline-abc] WARN: K8s unavailable (%v) — using NopDriver, runs will be held", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}

	// BoundedDriver wraps the inner driver and caps concurrent Start() calls.
	// This is the Q2 fix: release-queue burst control is now in the main path.
	drv := imp.NewBoundedDriver(innerDrv, maxConcurrentK8sJobs)
	log.Printf("[pipeline-abc] BoundedDriver(sem=%d) initialized, K8sAvailable=%v",
		maxConcurrentK8sJobs, k8sErr == nil)

	// ── 3. RunGate: ingress boundary ────────────────────────────────────────
	// RunGate wraps RunStore. Admit() transitions: queued→admitted→running→finished.
	// This is the Q1 fix: RunStore is populated BEFORE dag.Start().
	gate := ingress.NewRunGate(runStore)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Println("[pipeline-abc] building A→B→C DAG with shared PVC handoff")

	// ── 4. RunGate.Admit: all execution happens inside this call ────────────
	// The runFn is the existing dag-go execution path, unmodified.
	// RunGate records state transitions around it.
	gateErr := gate.Admit(ctx, pipelineRunID, func(ctx context.Context) error {
		return runPipeline(ctx, drv)
	})
	if gateErr != nil {
		log.Printf("[pipeline-abc] FAIL: gate returned error: %v", gateErr)
		os.Exit(1)
	}
	fmt.Println("[pipeline-abc] PASS: A→B→C pipeline succeeded with shared PVC handoff")
}

// runPipeline builds and executes the A→B→C dag-go pipeline.
// This function is unchanged from Sprint 2 — RunGate wraps it externally.
func runPipeline(ctx context.Context, drv driver.Driver) error {
	dag, err := daggo.InitDag()
	if err != nil {
		return fmt.Errorf("dag init: %w", err)
	}

	nodeA := dag.CreateNode("node-a")
	nodeB := dag.CreateNode("node-b")
	nodeC := dag.CreateNode("node-c")
	if nodeA == nil || nodeB == nil || nodeC == nil {
		return fmt.Errorf("CreateNode returned nil")
	}

	for _, edge := range [][2]string{
		{daggo.StartNode, "node-a"},
		{"node-a", "node-b"},
		{"node-b", "node-c"},
	} {
		if err := dag.AddEdge(edge[0], edge[1]); err != nil {
			return fmt.Errorf("AddEdge %s→%s: %w", edge[0], edge[1], err)
		}
	}

	pvcMount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}
	if !nodeA.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specA(pvcMount)}) {
		return fmt.Errorf("SetRunner failed for node-a")
	}
	if !nodeB.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specB(pvcMount)}) {
		return fmt.Errorf("SetRunner failed for node-b")
	}
	if !nodeC.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specC(pvcMount)}) {
		return fmt.Errorf("SetRunner failed for node-c")
	}

	if err := dag.FinishDag(); err != nil {
		return fmt.Errorf("FinishDag: %w", err)
	}
	if !dag.ConnectRunner() {
		return fmt.Errorf("ConnectRunner failed")
	}
	if !dag.GetReady(ctx) {
		return fmt.Errorf("GetReady failed")
	}

	fmt.Println("[pipeline-abc] starting DAG...")
	if !dag.Start() {
		return fmt.Errorf("dag Start failed")
	}

	if !dag.Wait(ctx) {
		return fmt.Errorf("dag Wait: pipeline did not succeed")
	}
	return nil
}

// bootstrapRecovery logs any runs that were queued/held before restart.
// In production, this would re-submit them. For PoC, we just log the recovery.
func bootstrapRecovery(s store.RunStore) {
	ctx := context.Background()
	queued, _ := s.ListByState(ctx, store.StateQueued)
	held, _ := s.ListByState(ctx, store.StateHeld)
	if len(queued)+len(held) > 0 {
		log.Printf("[pipeline-abc] BOOTSTRAP: recovered %d queued + %d held runs from previous run",
			len(queued), len(held))
		for _, r := range queued {
			log.Printf("[pipeline-abc]   queued: %s (created %s)", r.RunID, r.CreatedAt.Format(time.RFC3339))
		}
		for _, r := range held {
			log.Printf("[pipeline-abc]   held: %s (created %s)", r.RunID, r.CreatedAt.Format(time.RFC3339))
		}
	}
}

// ── RunSpec builders ──────────────────────────────────────────────────────────

func specA(mount api.Mount) api.RunSpec {
	return api.RunSpec{
		RunID:    "pipeline-abc-a",
		ImageRef: "busybox:1.36",
		Command: []string{"sh", "-c",
			`echo '[node-A] writing a.txt' && echo 'hello from node-A' > /data/a.txt && echo '[node-A] done'`,
		},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

func specB(mount api.Mount) api.RunSpec {
	return api.RunSpec{
		RunID:    "pipeline-abc-b",
		ImageRef: "busybox:1.36",
		Command: []string{"sh", "-c",
			`echo '[node-B] reading a.txt' && CONTENT=$(cat /data/a.txt) && echo "B processed: $CONTENT" > /data/b.txt && echo '[node-B] wrote b.txt'`,
		},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

func specC(mount api.Mount) api.RunSpec {
	return api.RunSpec{
		RunID:    "pipeline-abc-c",
		ImageRef: "busybox:1.36",
		Command: []string{"sh", "-c",
			`echo '[node-C] verifying b.txt' && CONTENT=$(cat /data/b.txt) && echo "[node-C] got: $CONTENT" && echo "$CONTENT" | grep -q 'B processed: hello from node-A' && echo '[node-C] verification PASSED' || { echo '[node-C] verification FAILED'; exit 1; }`,
		},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}
