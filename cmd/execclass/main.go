// cmd/execclass: Day 8 검증용 — 같은 DAG 내 standard/highmem executionClass 혼용
//
// 파이프라인 구조:
//
//	start → A(standard) → B1(standard) ─┐
//	                       B2(highmem)  ─┤→ C(standard) → end
//
// 검증 항목:
//  1. A, B1, C → poc-standard-lq (poc-standard-cq에서 처리)
//  2. B2       → poc-highmem-lq  (poc-highmem-cq에서 처리)
//  3. 같은 DAG 내에서 두 큐가 동시 활성화됨 (`kubectl get workloads` 로 확인)
//
// 사용법: go run ./cmd/execclass/
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

	fmt.Println("[execclass] building mixed executionClass DAG")
	fmt.Println("[execclass] A(std) → B1(std)/B2(highmem) → C(std)")

	dag, err := daggo.InitDag()
	if err != nil {
		log.Fatalf("dag init: %v", err)
	}

	for _, id := range []string{"ec-a", "ec-b1", "ec-b2", "ec-c"} {
		if dag.CreateNode(id) == nil {
			log.Fatalf("CreateNode(%s) returned nil", id)
		}
	}

	edges := [][2]string{
		{daggo.StartNode, "ec-a"},
		{"ec-a", "ec-b1"},
		{"ec-a", "ec-b2"},
		{"ec-b1", "ec-c"},
		{"ec-b2", "ec-c"},
	}
	for _, e := range edges {
		if err := dag.AddEdge(e[0], e[1]); err != nil {
			log.Fatalf("AddEdge %s→%s: %v", e[0], e[1], err)
		}
	}

	pvcMount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}

	// executionClass → queue-name label injection
	std := adapter.ExecutionClassStandard
	hm := adapter.ExecutionClassHighmem

	runners := map[string]*adapter.SpawnerNode{
		"ec-a": {Driver: drv, Spec: makeSpec("ec-a", std, pvcMount,
			`echo '[ec-A] standard node' && echo 'seed' > /data/ec-seed.txt`)},
		"ec-b1": {Driver: drv, Spec: makeSpec("ec-b1", std, pvcMount,
			`echo '[ec-B1] standard node' && cat /data/ec-seed.txt && echo 'b1-ok' > /data/ec-b1.txt`)},
		"ec-b2": {Driver: drv, Spec: makeSpecHighmem("ec-b2", hm, pvcMount,
			`echo '[ec-B2] highmem node' && cat /data/ec-seed.txt && echo 'b2-ok' > /data/ec-b2.txt`)},
		"ec-c": {Driver: drv, Spec: makeSpec("ec-c", std, pvcMount,
			`echo '[ec-C] standard node' && cat /data/ec-b1.txt && cat /data/ec-b2.txt && echo '[ec-C] both results verified'`)},
	}
	for id, r := range runners {
		if !dag.SetNodeRunner(id, r) {
			log.Fatalf("SetNodeRunner(%s) failed", id)
		}
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

	fmt.Println("[execclass] starting DAG...")
	if !dag.Start() {
		log.Fatalf("Start failed")
	}

	ok := dag.Wait(ctx)
	if ok {
		fmt.Println("[execclass] PASS: mixed executionClass DAG succeeded")
		fmt.Println("[execclass] standard nodes → poc-standard-lq, highmem node → poc-highmem-lq")
	} else {
		fmt.Println("[execclass] FAIL: pipeline did not succeed")
		os.Exit(1)
	}
}

// makeSpec: standard tier (100m CPU / 128Mi)
func makeSpec(runID string, ec adapter.ExecutionClass, mount api.Mount, cmd string) api.RunSpec {
	return api.RunSpec{
		RunID:     runID,
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", cmd},
		Labels:    ec.QueueLabel(),
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "128Mi"},
	}
}

// makeSpecHighmem: highmem tier (200m CPU / 512Mi) — 실제 highmem-cq 제출 확인용
func makeSpecHighmem(runID string, ec adapter.ExecutionClass, mount api.Mount, cmd string) api.RunSpec {
	return api.RunSpec{
		RunID:     runID,
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", cmd},
		Labels:    ec.QueueLabel(),
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "200m", Memory: "512Mi"},
	}
}
