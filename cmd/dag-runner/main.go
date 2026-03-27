// cmd/dag-runner: Day 4 검증용 — dag-go로 single node A를 실행하고 성공/실패를 확인한다.
// 사용법: go run ./cmd/dag-runner/ [success|fail]
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

func main() {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	drv, err := imp.NewK8sFromKubeconfig("default", kubeconfig)
	if err != nil {
		log.Fatalf("driver init: %v", err)
	}

	mode := "success"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "success":
		runDAG(drv, "dag-node-a-ok", successSpec())
	case "fail":
		runDAG(drv, "dag-node-a-fail", failSpec())
	default:
		log.Fatalf("unknown mode: %s (use 'success' or 'fail')", mode)
	}
}

func successSpec() api.RunSpec {
	return api.RunSpec{
		RunID:    "dag-node-a-ok",
		ImageRef: "busybox:1.36",
		Command:  []string{"sh", "-c", "echo '[dag] node-A started' && sleep 3 && echo '[dag] node-A done'"},
		Labels: map[string]string{
			"kueue.x-k8s.io/queue-name": "poc-standard-lq",
		},
		Resources: api.Resources{CPU: "100m", Memory: "128Mi"},
	}
}

func failSpec() api.RunSpec {
	return api.RunSpec{
		RunID:    "dag-node-a-fail",
		ImageRef: "busybox:1.36",
		Command:  []string{"sh", "-c", "echo '[dag] node-A started' && sleep 2 && exit 1"},
		Labels: map[string]string{
			"kueue.x-k8s.io/queue-name": "poc-standard-lq",
		},
		Resources: api.Resources{CPU: "100m", Memory: "128Mi"},
	}
}

func runDAG(drv *imp.DriverK8s, nodeID string, spec api.RunSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Printf("[dag-runner] building DAG with single node: %s\n", nodeID)

	dag, err := daggo.InitDag()
	if err != nil {
		log.Fatalf("dag init: %v", err)
	}

	node := dag.CreateNode(nodeID)
	if node == nil {
		log.Fatalf("CreateNode returned nil for %s", nodeID)
	}

	// Connect start_node → user node so the DAG graph is valid.
	if err := dag.AddEdge(daggo.StartNode, nodeID); err != nil {
		log.Fatalf("AddEdge start→%s: %v", nodeID, err)
	}

	runner := &adapter.SpawnerNode{Spec: spec, Driver: drv}
	if !node.SetRunner(runner) {
		log.Fatalf("SetRunner failed for node %s", nodeID)
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

	fmt.Printf("[dag-runner] starting DAG...\n")
	if !dag.Start() {
		log.Fatalf("Start failed")
	}

	ok := dag.Wait(ctx)
	if ok {
		fmt.Printf("[dag-runner] PASS: DAG succeeded (node %s)\n", nodeID)
	} else {
		fmt.Printf("[dag-runner] FAIL: DAG did not succeed (node %s)\n", nodeID)
		os.Exit(1)
	}
}
