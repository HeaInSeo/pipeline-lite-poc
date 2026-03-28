// cmd/dag-runner: dag-go로 single node A를 실행하고 성공/실패를 확인한다.
//
// Sprint 3 변경사항:
//   - RunGate + JsonRunStore: ingress boundary 등록 (Q1 fix)
//   - BoundedDriver(sem=3): K8s Job 생성 burst 제어 (Q2 fix)
//   - K8s init 실패 시 graceful degradation: NopDriver
//
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
	"github.com/seoyhaein/poc/pkg/ingress"
	"github.com/seoyhaein/poc/pkg/store"
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
	"github.com/seoyhaein/spawner/pkg/driver"
)

const (
	maxConcurrentK8sJobs = 3
	runStoreFile         = "/tmp/dag-runner-runstore.json"
)

func main() {
	runStore, err := store.NewJsonRunStore(runStoreFile)
	if err != nil {
		log.Fatalf("runstore init: %v", err)
	}

	var innerDrv driver.Driver
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig("default", kubeconfig)
	if k8sErr != nil {
		log.Printf("[dag-runner] WARN: K8s unavailable (%v) — using NopDriver", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}
	drv := imp.NewBoundedDriver(innerDrv, maxConcurrentK8sJobs)

	gate := ingress.NewRunGate(runStore)

	mode := "success"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "success":
		runDAG(gate, drv, "dag-node-a-ok", successSpec())
	case "fail":
		runDAG(gate, drv, "dag-node-a-fail", failSpec())
	default:
		log.Fatalf("unknown mode: %s (use 'success' or 'fail')", mode)
	}
}

func successSpec() api.RunSpec {
	return api.RunSpec{
		RunID:     "dag-node-a-ok",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[dag] node-A started' && sleep 3 && echo '[dag] node-A done'"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "100m", Memory: "128Mi"},
	}
}

func failSpec() api.RunSpec {
	return api.RunSpec{
		RunID:     "dag-node-a-fail",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[dag] node-A started' && sleep 2 && exit 1"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "100m", Memory: "128Mi"},
	}
}

func runDAG(gate *ingress.RunGate, drv driver.Driver, nodeID string, spec api.RunSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	runID := "dag-runner-" + nodeID
	fmt.Printf("[dag-runner] building DAG with single node: %s\n", nodeID)

	gateErr := gate.Admit(ctx, runID, func(ctx context.Context) error {
		dag, err := daggo.InitDag()
		if err != nil {
			return fmt.Errorf("dag init: %w", err)
		}

		node := dag.CreateNode(nodeID)
		if node == nil {
			return fmt.Errorf("CreateNode returned nil for %s", nodeID)
		}
		if err := dag.AddEdge(daggo.StartNode, nodeID); err != nil {
			return fmt.Errorf("AddEdge start→%s: %w", nodeID, err)
		}

		runner := &adapter.SpawnerNode{Spec: spec, Driver: drv}
		if !node.SetRunner(runner) {
			return fmt.Errorf("SetRunner failed for node %s", nodeID)
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

		fmt.Printf("[dag-runner] starting DAG...\n")
		if !dag.Start() {
			return fmt.Errorf("Start failed")
		}

		if !dag.Wait(ctx) {
			return fmt.Errorf("DAG did not succeed (node %s)", nodeID)
		}
		return nil
	})

	if gateErr != nil {
		fmt.Printf("[dag-runner] FAIL: DAG did not succeed (node %s): %v\n", nodeID, gateErr)
		os.Exit(1)
	}
	fmt.Printf("[dag-runner] PASS: DAG succeeded (node %s)\n", nodeID)
}
