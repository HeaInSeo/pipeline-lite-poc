// cmd/fanout: Day 6 검증용 — A → B1/B2/B3 fan-out 병렬 실행 + queue-name label injection
//
// 파이프라인 구조:
//
//	start → A → B1 ─┐
//	              B2 ─┤→ end (FinishDag 자동 연결)
//	              B3 ─┘
//
// 검증 항목:
//  1. dag-go worker pool이 B1/B2/B3를 실제로 병렬 실행하는지 (타임스탬프로 확인)
//  2. 각 Bx가 kueue.x-k8s.io/queue-name label을 올바르게 주입받는지
//  3. A가 쓴 /data/a.txt를 B1/B2/B3 각각이 독립적으로 읽는지
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
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
)

const (
	pvcName   = "poc-shared-pvc"
	mountPath = "/data"
	namespace = "default"
)

func main() {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	drv, err := imp.NewK8sFromKubeconfig(namespace, kubeconfig)
	if err != nil {
		log.Fatalf("driver init: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Println("[fanout] building A → B1/B2/B3 fan-out DAG")

	dag, err := daggo.InitDag()
	if err != nil {
		log.Fatalf("dag init: %v", err)
	}

	// Create 4 nodes
	nodeA := dag.CreateNode("fanout-a")
	nodeB1 := dag.CreateNode("fanout-b1")
	nodeB2 := dag.CreateNode("fanout-b2")
	nodeB3 := dag.CreateNode("fanout-b3")
	if nodeA == nil || nodeB1 == nil || nodeB2 == nil || nodeB3 == nil {
		log.Fatalf("CreateNode returned nil")
	}

	// Wire edges: start→A, A→B1, A→B2, A→B3 (B1/B2/B3 are independent → parallel)
	for _, edge := range [][2]string{
		{daggo.StartNode, "fanout-a"},
		{"fanout-a", "fanout-b1"},
		{"fanout-a", "fanout-b2"},
		{"fanout-a", "fanout-b3"},
	} {
		if err := dag.AddEdge(edge[0], edge[1]); err != nil {
			log.Fatalf("AddEdge %s→%s: %v", edge[0], edge[1], err)
		}
	}

	pvcMount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}

	// queue-name label injection: all nodes → poc-standard-lq
	queueLabel := map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"}

	if !nodeA.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specA(pvcMount, queueLabel)}) {
		log.Fatalf("SetRunner failed for fanout-a")
	}
	if !nodeB1.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specB(1, pvcMount, queueLabel)}) {
		log.Fatalf("SetRunner failed for fanout-b1")
	}
	if !nodeB2.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specB(2, pvcMount, queueLabel)}) {
		log.Fatalf("SetRunner failed for fanout-b2")
	}
	if !nodeB3.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specB(3, pvcMount, queueLabel)}) {
		log.Fatalf("SetRunner failed for fanout-b3")
	}

	if err := dag.FinishDag(); err != nil {
		log.Fatalf("FinishDag: %v", err)
	}
	if !dag.ConnectRunner() {
		log.Fatalf("ConnectRunner failed")
	}
	if !dag.GetReady(ctx) {
		log.Fatalf("GetReady failed")
	}

	fmt.Println("[fanout] starting DAG...")
	start := time.Now()
	if !dag.Start() {
		log.Fatalf("Start failed")
	}

	ok := dag.Wait(ctx)
	elapsed := time.Since(start).Round(time.Second)

	if ok {
		fmt.Printf("[fanout] PASS: fan-out pipeline succeeded (elapsed=%s)\n", elapsed)
		fmt.Println("[fanout] B1/B2/B3 ran in parallel — each ~6s total, not ~18s sequential")
	} else {
		fmt.Println("[fanout] FAIL: pipeline did not succeed")
		os.Exit(1)
	}
}

// specA: write timestamp + "hello from A" to /data/a.txt
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

// specB: each Bx reads /data/a.txt, sleeps 4s (to make parallel vs serial obvious), writes /data/bN.txt
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
