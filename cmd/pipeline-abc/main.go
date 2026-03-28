// cmd/pipeline-abc: Day 5 검증용 — A→B→C 선형 파이프라인 + shared PVC 파일 handoff
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
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
)

const (
	pvcName   = "poc-shared-pvc"
	mountPath = "/data"
	queueName = "poc-standard-lq"
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

	fmt.Println("[pipeline-abc] building A→B→C DAG with shared PVC handoff")

	dag, err := daggo.InitDag()
	if err != nil {
		log.Fatalf("dag init: %v", err)
	}

	// Create nodes
	nodeA := dag.CreateNode("node-a")
	nodeB := dag.CreateNode("node-b")
	nodeC := dag.CreateNode("node-c")
	if nodeA == nil || nodeB == nil || nodeC == nil {
		log.Fatalf("CreateNode returned nil")
	}

	// Wire edges: start → A → B → C → (end auto-wired by FinishDag)
	for _, edge := range [][2]string{
		{daggo.StartNode, "node-a"},
		{"node-a", "node-b"},
		{"node-b", "node-c"},
	} {
		if err := dag.AddEdge(edge[0], edge[1]); err != nil {
			log.Fatalf("AddEdge %s→%s: %v", edge[0], edge[1], err)
		}
	}

	// Attach runners
	pvcMount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}

	if !nodeA.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specA(pvcMount)}) {
		log.Fatalf("SetRunner failed for node-a")
	}
	if !nodeB.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specB(pvcMount)}) {
		log.Fatalf("SetRunner failed for node-b")
	}
	if !nodeC.SetRunner(&adapter.SpawnerNode{Driver: drv, Spec: specC(pvcMount)}) {
		log.Fatalf("SetRunner failed for node-c")
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

	fmt.Println("[pipeline-abc] starting DAG...")
	if !dag.Start() {
		log.Fatalf("Start failed")
	}

	ok := dag.Wait(ctx)
	if ok {
		fmt.Println("[pipeline-abc] PASS: A→B→C pipeline succeeded with shared PVC handoff")
	} else {
		fmt.Println("[pipeline-abc] FAIL: pipeline did not succeed")
		os.Exit(1)
	}
}

// specA: write "hello from node-A" to /data/a.txt
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

// specB: read /data/a.txt, write "B processed: <content>" to /data/b.txt
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

// specC: read /data/b.txt and verify it contains expected content
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
