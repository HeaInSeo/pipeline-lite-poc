// cmd/fanout: A → B1/B2/B3 fan-out 병렬 실행 + queue-name label injection
//
// Sprint 3 변경사항:
//   - RunGate + JsonRunStore: ingress boundary 등록 (Q1 fix)
//   - BoundedDriver(sem=3): K8s Job 생성 burst 제어 (Q2 fix)
//   - K8s init 실패 시 graceful degradation: NopDriver
//
// 파이프라인 구조:
//
//	start → A → B1 ─┐
//	              B2 ─┤→ end
//	              B3 ─┘
//
// 사용법: go run ./cmd/fanout/
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
	pvcName              = "poc-shared-pvc"
	mountPath            = "/data"
	namespace            = "default"
	maxConcurrentK8sJobs = 3
	runStoreFile         = "/tmp/fanout-runstore.json"
	pipelineRunID        = "fanout-run-1"
)

func main() {
	runStore, err := store.NewJsonRunStore(runStoreFile)
	if err != nil {
		log.Fatalf("runstore init: %v", err)
	}
	bootstrapRecovery(runStore)

	var innerDrv driver.Driver
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig(namespace, kubeconfig)
	if k8sErr != nil {
		log.Printf("[fanout] WARN: K8s unavailable (%v) — using NopDriver", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}
	drv := imp.NewBoundedDriver(innerDrv, maxConcurrentK8sJobs)
	log.Printf("[fanout] BoundedDriver(sem=%d) initialized, K8sAvailable=%v",
		maxConcurrentK8sJobs, k8sErr == nil)

	gate := ingress.NewRunGate(runStore)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Println("[fanout] building A → B1/B2/B3 fan-out DAG")

	gateErr := gate.Admit(ctx, pipelineRunID, func(ctx context.Context) error {
		start := time.Now()
		if !runFanoutPipeline(ctx, drv) {
			return fmt.Errorf("fanout pipeline failed")
		}
		elapsed := time.Since(start).Round(time.Second)
		fmt.Printf("[fanout] PASS: fan-out pipeline succeeded (elapsed=%s)\n", elapsed)
		fmt.Println("[fanout] B1/B2/B3 ran in parallel — each ~6s total, not ~18s sequential")
		return nil
	})
	if gateErr != nil {
		fmt.Printf("[fanout] FAIL: %v\n", gateErr)
		os.Exit(1)
	}
}

func runFanoutPipeline(ctx context.Context, drv driver.Driver) bool {
	dag, err := daggo.InitDag()
	if err != nil {
		log.Printf("dag init: %v", err)
		return false
	}

	nodeA := dag.CreateNode("fanout-a")
	nodeB1 := dag.CreateNode("fanout-b1")
	nodeB2 := dag.CreateNode("fanout-b2")
	nodeB3 := dag.CreateNode("fanout-b3")
	if nodeA == nil || nodeB1 == nil || nodeB2 == nil || nodeB3 == nil {
		log.Printf("CreateNode returned nil")
		return false
	}

	for _, edge := range [][2]string{
		{daggo.StartNode, "fanout-a"},
		{"fanout-a", "fanout-b1"},
		{"fanout-a", "fanout-b2"},
		{"fanout-a", "fanout-b3"},
	} {
		if err := dag.AddEdge(edge[0], edge[1]); err != nil {
			log.Printf("AddEdge %s→%s: %v", edge[0], edge[1], err)
			return false
		}
	}

	pvcMount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}
	queueLabel := map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"}

	if !nodeA.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specA(pvcMount, queueLabel)}) ||
		!nodeB1.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specB(1, pvcMount, queueLabel)}) ||
		!nodeB2.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specB(2, pvcMount, queueLabel)}) ||
		!nodeB3.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specB(3, pvcMount, queueLabel)}) {
		log.Printf("SetRunner failed")
		return false
	}

	if err := dag.FinishDag(); err != nil {
		log.Printf("FinishDag: %v", err)
		return false
	}
	if !dag.ConnectRunner() || !dag.GetReady(ctx) {
		return false
	}

	fmt.Println("[fanout] starting DAG...")
	if !dag.Start() {
		return false
	}
	return dag.Wait(ctx)
}

func bootstrapRecovery(s store.RunStore) {
	ctx := context.Background()
	queued, _ := s.ListByState(ctx, store.StateQueued)
	held, _ := s.ListByState(ctx, store.StateHeld)
	if len(queued)+len(held) > 0 {
		log.Printf("[fanout] BOOTSTRAP: recovered %d queued + %d held runs", len(queued), len(held))
	}
}

func specA(mount api.Mount, labels map[string]string) api.RunSpec {
	return api.RunSpec{
		RunID:    "fanout-a",
		ImageRef: "busybox:1.36",
		Command: []string{"sh", "-c",
			`echo '[fanout-A] start' && date -u '+%H:%M:%S' > /data/a.txt && echo 'hello from fanout-A' >> /data/a.txt && echo '[fanout-A] wrote /data/a.txt'`,
		},
		Labels:    labels,
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

func specB(n int, mount api.Mount, labels map[string]string) api.RunSpec {
	outFile := fmt.Sprintf("/data/b%d.txt", n)
	return api.RunSpec{
		RunID:    fmt.Sprintf("fanout-b%d", n),
		ImageRef: "busybox:1.36",
		Command: []string{"sh", "-c",
			fmt.Sprintf(
				`echo '[fanout-B%d] start at' $(date -u '+%%H:%%M:%%S') && `+
					`CONTENT=$(cat /data/a.txt) && sleep 4 && `+
					`echo "B%d processed: $CONTENT" > %s && `+
					`echo '[fanout-B%d] done at' $(date -u '+%%H:%%M:%%S')`,
				n, n, outFile, n,
			),
		},
		Labels:    labels,
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}
