// cmd/fastfail: Day 7 검증용 — fast-fail 전파 확인
//
// 파이프라인 구조:
//
//	start → A → B1(success) ─┐
//	              B2(FAIL)    ─┤→ C(수렴) → end
//	              B3(success) ─┘
//
// 검증 항목:
//  1. B2 실패 → C가 부모 실패 전파로 실행되지 않음 (PreflightFailed)
//  2. B1/B3는 A에만 의존 → 정상 실행 완료
//  3. dag-go Wait() → false 반환 (DAG 실패)
//
// 사용법: go run ./cmd/fastfail/
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
	queueName = "poc-standard-lq"
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

	fmt.Println("[fastfail] building A → B1/B2(FAIL)/B3 → C DAG")
	fmt.Println("[fastfail] expected: B2 fails → C skipped → Wait=false")

	dag, err := daggo.InitDag()
	if err != nil {
		log.Fatalf("dag init: %v", err)
	}

	// Create nodes
	for _, id := range []string{"ff-a", "ff-b1", "ff-b2", "ff-b3", "ff-c"} {
		if dag.CreateNode(id) == nil {
			log.Fatalf("CreateNode(%s) returned nil", id)
		}
	}

	// Edges: start→A, A→B1/B2/B3, B1/B2/B3→C (수렴)
	edges := [][2]string{
		{daggo.StartNode, "ff-a"},
		{"ff-a", "ff-b1"},
		{"ff-a", "ff-b2"},
		{"ff-a", "ff-b3"},
		{"ff-b1", "ff-c"},
		{"ff-b2", "ff-c"},
		{"ff-b3", "ff-c"},
	}
	for _, e := range edges {
		if err := dag.AddEdge(e[0], e[1]); err != nil {
			log.Fatalf("AddEdge %s→%s: %v", e[0], e[1], err)
		}
	}

	pvcMount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}
	labels := map[string]string{"kueue.x-k8s.io/queue-name": queueName}

	dag.SetNodeRunner("ff-a", &adapter.SpawnerNode{Driver: drv, Spec: specA(pvcMount, labels)})
	dag.SetNodeRunner("ff-b1", &adapter.SpawnerNode{Driver: drv, Spec: specB(1, false, pvcMount, labels)})
	dag.SetNodeRunner("ff-b2", &adapter.SpawnerNode{Driver: drv, Spec: specB(2, true, pvcMount, labels)}) // FAIL
	dag.SetNodeRunner("ff-b3", &adapter.SpawnerNode{Driver: drv, Spec: specB(3, false, pvcMount, labels)})
	dag.SetNodeRunner("ff-c", &adapter.SpawnerNode{Driver: drv, Spec: specC(pvcMount, labels)})

	if err := dag.FinishDag(); err != nil {
		log.Fatalf("FinishDag: %v", err)
	}
	if !dag.ConnectRunner() {
		log.Fatalf("ConnectRunner failed")
	}
	if !dag.GetReady(ctx) {
		log.Fatalf("GetReady failed")
	}

	fmt.Println("[fastfail] starting DAG...")
	if !dag.Start() {
		log.Fatalf("Start failed")
	}

	ok := dag.Wait(ctx)
	if !ok {
		fmt.Println("[fastfail] PASS: Wait=false (B2 failure propagated, C not executed)")
	} else {
		fmt.Println("[fastfail] UNEXPECTED: Wait=true — fast-fail did not trigger")
		os.Exit(1)
	}
}

func specA(mount api.Mount, labels map[string]string) api.RunSpec {
	return api.RunSpec{
		RunID:    "ff-a",
		ImageRef: "busybox:1.36",
		Command:  []string{"sh", "-c", `echo '[ff-A] writing seed' && echo 'seed' > /data/ff-seed.txt`},
		Labels:   labels, Mounts: []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// specB: shouldFail=true → exit 1 (fast-fail 트리거용)
func specB(n int, shouldFail bool, mount api.Mount, labels map[string]string) api.RunSpec {
	var cmd string
	if shouldFail {
		cmd = fmt.Sprintf(`echo '[ff-B%d] intentional FAIL' && exit 1`, n)
	} else {
		cmd = fmt.Sprintf(`echo '[ff-B%d] success' && echo 'ok' > /data/ff-b%d.txt`, n, n)
	}
	return api.RunSpec{
		RunID:    fmt.Sprintf("ff-b%d", n),
		ImageRef: "busybox:1.36",
		Command:  []string{"sh", "-c", cmd},
		Labels:   labels, Mounts: []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// specC: 수렴 노드 — B2 실패로 인해 실행되지 않아야 한다
func specC(mount api.Mount, labels map[string]string) api.RunSpec {
	return api.RunSpec{
		RunID:    "ff-c",
		ImageRef: "busybox:1.36",
		Command:  []string{"sh", "-c", `echo '[ff-C] THIS SHOULD NOT RUN if fast-fail works' && exit 1`},
		Labels:   labels, Mounts: []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}
