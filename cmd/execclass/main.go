// cmd/execclass: 같은 DAG 내 standard/highmem executionClass 혼용
//
// Sprint 3 변경사항:
//   - RunGate + JsonRunStore: ingress boundary 등록 (Q1 fix)
//   - BoundedDriver(sem=3): K8s Job 생성 burst 제어 (Q2 fix)
//   - K8s init 실패 시 graceful degradation: NopDriver
//
// 파이프라인 구조:
//
//	start → A(standard) → B1(standard) ─┐
//	                       B2(highmem)  ─┤→ C(standard) → end
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
	runStoreFile         = "/tmp/execclass-runstore.json"
	pipelineRunID        = "execclass-run-1"
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
		log.Printf("[execclass] WARN: K8s unavailable (%v) — using NopDriver", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}
	drv := imp.NewBoundedDriver(innerDrv, maxConcurrentK8sJobs)
	log.Printf("[execclass] BoundedDriver(sem=%d) initialized, K8sAvailable=%v",
		maxConcurrentK8sJobs, k8sErr == nil)

	gate := ingress.NewRunGate(runStore)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Println("[execclass] building mixed executionClass DAG")
	fmt.Println("[execclass] A(std) → B1(std)/B2(highmem) → C(std)")

	gateErr := gate.Admit(ctx, pipelineRunID, func(ctx context.Context) error {
		if !runExecClassPipeline(ctx, drv) {
			return fmt.Errorf("execclass pipeline failed")
		}
		fmt.Println("[execclass] PASS: mixed executionClass DAG succeeded")
		fmt.Println("[execclass] standard nodes → poc-standard-lq, highmem node → poc-highmem-lq")
		return nil
	})
	if gateErr != nil {
		fmt.Printf("[execclass] FAIL: %v\n", gateErr)
		os.Exit(1)
	}
}

func runExecClassPipeline(ctx context.Context, drv driver.Driver) bool {
	dag, err := daggo.InitDag()
	if err != nil {
		log.Printf("dag init: %v", err)
		return false
	}

	for _, id := range []string{"ec-a", "ec-b1", "ec-b2", "ec-c"} {
		if dag.CreateNode(id) == nil {
			log.Printf("CreateNode(%s) returned nil", id)
			return false
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
			log.Printf("AddEdge %s→%s: %v", e[0], e[1], err)
			return false
		}
	}

	pvcMount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}
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
			log.Printf("SetNodeRunner(%s) failed", id)
			return false
		}
	}

	if err := dag.FinishDag(); err != nil {
		log.Printf("FinishDag: %v", err)
		return false
	}
	if !dag.ConnectRunner() || !dag.GetReady(ctx) {
		return false
	}

	fmt.Println("[execclass] starting DAG...")
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
		log.Printf("[execclass] BOOTSTRAP: recovered %d queued + %d held runs", len(queued), len(held))
	}
}

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
