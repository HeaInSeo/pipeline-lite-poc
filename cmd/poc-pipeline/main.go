// cmd/poc-pipeline: A→(B1,B2,B3) fan-out → C collect 파이프라인 (shared PVC, runId 네임스페이싱)
//
// PoC 핵심 검증 파이프라인: 실제 파일 handoff가 있는 fan-out+collect 구조.
//
// 파이프라인 구조:
//
//	start → poc-a → poc-b1 ─┐
//	               → poc-b2 ─┼→ poc-c → end
//	               → poc-b3 ─┘
//
// PVC 경로 (runId 기반 네임스페이싱):
//
//	/data/poc-pipeline/{runId}/a-output/seed.txt         (A 생성)
//	/data/poc-pipeline/{runId}/b-output/shard-0/result.txt (B1 생성)
//	/data/poc-pipeline/{runId}/b-output/shard-1/result.txt (B2 생성)
//	/data/poc-pipeline/{runId}/b-output/shard-2/result.txt (B3 생성)
//	/data/poc-pipeline/{runId}/c-output/report.txt       (C 생성)
//
// A 노드가 전체 디렉터리 트리를 준비하므로 B/C 노드는 파일만 작성.
//
// 실행 방법: go run ./cmd/poc-pipeline/ [--run-id <id>]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
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
	pvcName              = "poc-shared-pvc"
	mountPath            = "/data"
	queueName            = "poc-standard-lq"
	namespace            = "default"
	maxConcurrentK8sJobs = 3
	runStoreFile         = "/tmp/poc-pipeline-runstore.json"
	dataRoot             = "/data/poc-pipeline"
)

func main() {
	var runIDOverride string
	flag.StringVar(&runIDOverride, "run-id", "", "custom run ID (default: auto-generated timestamp YYYYMMDD-HHMMSS)")
	flag.Parse()

	runID := runIDOverride
	if runID == "" {
		runID = generateRunID()
	}
	pBase := pathBase(runID)
	storeID := "poc-pipeline-" + runID
	log.Printf("[poc-pipeline] runId=%s pBase=%s", runID, pBase)

	// ── 1. Durable RunStore ──────────────────────────────────────────────────
	runStore, err := store.NewJsonRunStore(runStoreFile)
	if err != nil {
		log.Fatalf("runstore init: %v", err)
	}
	bootstrapRecovery(runStore)

	// ── 2. Driver: DriverK8s wrapped with BoundedDriver ─────────────────────
	var innerDrv driver.Driver
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig(namespace, kubeconfig)
	if k8sErr != nil {
		log.Printf("[poc-pipeline] WARN: K8s unavailable (%v) — using NopDriver, runs will be held", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}
	drv := imp.NewBoundedDriver(innerDrv, maxConcurrentK8sJobs)
	log.Printf("[poc-pipeline] BoundedDriver(sem=%d) initialized, K8sAvailable=%v",
		maxConcurrentK8sJobs, k8sErr == nil)

	// ── 3. RunGate: ingress boundary ─────────────────────────────────────────
	gate := ingress.NewRunGate(runStore)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Printf("[poc-pipeline] starting run %s: A → (B1,B2,B3) → C\n", runID)

	// ── 4. RunGate.Admit: all execution happens inside this call ─────────────
	// NopDriver path: Prepare() → ErrK8sUnavailable → dag.Wait false →
	// gate.Admit returns error → os.Exit(1). PASS line is never printed.
	if err := gate.Admit(ctx, storeID, func(ctx context.Context) error {
		return runFanoutCollect(ctx, drv, runID, pBase)
	}); err != nil {
		log.Printf("[poc-pipeline] FAIL (K8s path): %v", err)
		os.Exit(1)
	}
	// Reached only when real K8s Jobs ran and dag.Wait() returned true.
	fmt.Printf("[poc-pipeline] PASS (K8s path): run %s dag completed — report at %s/c-output/report.txt\n", runID, pBase)
}

// runFanoutCollect builds and executes the A→(B1,B2,B3)→C dag-go pipeline.
func runFanoutCollect(ctx context.Context, drv driver.Driver, runID, pBase string) error {
	dag, err := daggo.InitDag()
	if err != nil {
		return fmt.Errorf("dag init: %w", err)
	}

	nodeA := dag.CreateNode("poc-a")
	nodeB1 := dag.CreateNode("poc-b1")
	nodeB2 := dag.CreateNode("poc-b2")
	nodeB3 := dag.CreateNode("poc-b3")
	nodeC := dag.CreateNode("poc-c")
	if nodeA == nil || nodeB1 == nil || nodeB2 == nil || nodeB3 == nil || nodeC == nil {
		return fmt.Errorf("CreateNode returned nil")
	}

	for _, edge := range [][2]string{
		{daggo.StartNode, "poc-a"},
		{"poc-a", "poc-b1"},
		{"poc-a", "poc-b2"},
		{"poc-a", "poc-b3"},
		{"poc-b1", "poc-c"},
		{"poc-b2", "poc-c"},
		{"poc-b3", "poc-c"},
	} {
		if err := dag.AddEdge(edge[0], edge[1]); err != nil {
			return fmt.Errorf("AddEdge %s→%s: %w", edge[0], edge[1], err)
		}
	}

	mount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}

	if !nodeA.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specA(runID, pBase, mount)}) {
		return fmt.Errorf("SetRunner failed for poc-a")
	}
	if !nodeB1.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specWorker(1, runID, pBase, mount)}) {
		return fmt.Errorf("SetRunner failed for poc-b1")
	}
	if !nodeB2.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specWorker(2, runID, pBase, mount)}) {
		return fmt.Errorf("SetRunner failed for poc-b2")
	}
	if !nodeB3.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specWorker(3, runID, pBase, mount)}) {
		return fmt.Errorf("SetRunner failed for poc-b3")
	}
	if !nodeC.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specCollect(runID, pBase, mount)}) {
		return fmt.Errorf("SetRunner failed for poc-c")
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

	fmt.Println("[poc-pipeline] starting DAG...")
	if !dag.Start() {
		return fmt.Errorf("dag Start failed")
	}
	if !dag.Wait(ctx) {
		return fmt.Errorf("dag Wait: pipeline did not succeed")
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// generateRunID returns a timestamp-based run ID in YYYYMMDD-HHMMSS-mmm format.
// Millisecond precision reduces collision risk when the binary is invoked
// multiple times within the same second (e.g., scripted test loops).
// Format: 20260328-123456-007 (19 chars)
func generateRunID() string {
	t := time.Now().UTC()
	return fmt.Sprintf("%s-%03d", t.Format("20060102-150405"), t.Nanosecond()/1e6)
}

// pathBase returns the PVC root directory for a given runId.
func pathBase(runID string) string {
	return dataRoot + "/" + runID
}

// nodeRunID returns the K8s Job name for a given runId + node label.
// Format: "poc-{runId}-{node}".
// If the full name exceeds 63 chars (K8s limit), the runId portion is
// truncated while preserving the node suffix, so all nodes remain
// distinguishable. With auto-generated 19-char runIds the limit is never hit.
func nodeRunID(runID, node string) string {
	id := "poc-" + runID + "-" + node
	if len(id) <= 63 {
		return id
	}
	// Preserve "-{node}" suffix; truncate runId to fit within 63.
	suffix := "-" + node
	prefix := "poc-"
	maxRunIDLen := 63 - len(prefix) - len(suffix)
	if maxRunIDLen < 1 {
		maxRunIDLen = 1
	}
	return prefix + runID[:maxRunIDLen] + suffix
}

func bootstrapRecovery(s store.RunStore) {
	ctx := context.Background()
	queued, _ := s.ListByState(ctx, store.StateQueued)
	held, _ := s.ListByState(ctx, store.StateHeld)
	if len(queued)+len(held) > 0 {
		log.Printf("[poc-pipeline] BOOTSTRAP: recovered %d queued + %d held runs", len(queued), len(held))
	}
}

// ── RunSpec builders ──────────────────────────────────────────────────────────

// specA prepares the full directory tree and writes seed.txt.
// All B and C subdirectories are created here so downstream nodes only write files.
func specA(runID, pBase string, mount api.Mount) api.RunSpec {
	lines := []string{
		"set -e",
		fmt.Sprintf("mkdir -p %s/a-output", pBase),
		fmt.Sprintf("mkdir -p %s/b-output/shard-0", pBase),
		fmt.Sprintf("mkdir -p %s/b-output/shard-1", pBase),
		fmt.Sprintf("mkdir -p %s/b-output/shard-2", pBase),
		fmt.Sprintf("mkdir -p %s/c-output", pBase),
		fmt.Sprintf("echo 'poc-pipeline seed' > %s/a-output/seed.txt", pBase),
		fmt.Sprintf("echo 'runId=%s' >> %s/a-output/seed.txt", runID, pBase),
		`echo "created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >> ` + pBase + `/a-output/seed.txt`,
		"echo '[poc-a] directory tree prepared, seed.txt written'",
	}
	return api.RunSpec{
		RunID:     nodeRunID(runID, "a"),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// specWorker builds a worker RunSpec for worker n (1-indexed).
// Reads seed.txt from a-output and writes result.txt to b-output/shard-{n-1}.
func specWorker(n int, runID, pBase string, mount api.Mount) api.RunSpec {
	shard := n - 1
	lines := []string{
		"set -e",
		fmt.Sprintf("SEED=$(cat %s/a-output/seed.txt)", pBase),
		fmt.Sprintf("echo 'worker-b%d result' > %s/b-output/shard-%d/result.txt", n, pBase, shard),
		fmt.Sprintf("echo 'shard=%d' >> %s/b-output/shard-%d/result.txt", shard, pBase, shard),
		fmt.Sprintf(`echo "seed_content=$SEED" >> %s/b-output/shard-%d/result.txt`, pBase, shard),
		fmt.Sprintf("echo '[poc-b%d] shard-%d/result.txt written'", n, shard),
	}
	return api.RunSpec{
		RunID:     nodeRunID(runID, fmt.Sprintf("b%d", n)),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// specCollect verifies all 3 shards exist and writes a consolidated report.txt.
func specCollect(runID, pBase string, mount api.Mount) api.RunSpec {
	lines := []string{
		"set -e",
		fmt.Sprintf("test -f %s/b-output/shard-0/result.txt || { echo '[poc-c] missing shard-0/result.txt'; exit 1; }", pBase),
		fmt.Sprintf("test -f %s/b-output/shard-1/result.txt || { echo '[poc-c] missing shard-1/result.txt'; exit 1; }", pBase),
		fmt.Sprintf("test -f %s/b-output/shard-2/result.txt || { echo '[poc-c] missing shard-2/result.txt'; exit 1; }", pBase),
		fmt.Sprintf("REPORT=%s/c-output/report.txt", pBase),
		fmt.Sprintf(`echo '=== poc-pipeline report ===' > "$REPORT"`),
		fmt.Sprintf(`echo 'runId=%s' >> "$REPORT"`, runID),
		`echo "generated_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >> "$REPORT"`,
		fmt.Sprintf(`echo '--- shard-0 ---' >> "$REPORT" && cat %s/b-output/shard-0/result.txt >> "$REPORT"`, pBase),
		fmt.Sprintf(`echo '--- shard-1 ---' >> "$REPORT" && cat %s/b-output/shard-1/result.txt >> "$REPORT"`, pBase),
		fmt.Sprintf(`echo '--- shard-2 ---' >> "$REPORT" && cat %s/b-output/shard-2/result.txt >> "$REPORT"`, pBase),
		`echo '=== end ===' >> "$REPORT"`,
		"echo '[poc-c] report.txt written'",
	}
	return api.RunSpec{
		RunID:     nodeRunID(runID, "c"),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}
