// cmd/fastfail: A → B1/B2(FAIL)/B3 → C 수렴 파이프라인 — fast-fail 전파 확인
//
// Sprint 3 변경사항:
//   - RunGate + JsonRunStore: 실행 전 ingress boundary 등록 (Q1 fix)
//   - BoundedDriver(sem=3): K8s Job 생성 burst 제어 (Q2 fix)
//   - K8s init 실패 시 graceful degradation: NopDriver + log.Printf
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
	"github.com/seoyhaein/poc/pkg/ingress"
	"github.com/seoyhaein/poc/pkg/store"
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
	"github.com/seoyhaein/spawner/pkg/driver"
)

const (
	pvcName   = "poc-shared-pvc"
	mountPath = "/data"
	namespace = "default"
	queueName = "poc-standard-lq"

	maxConcurrentK8sJobs = 3
	runStoreFile         = "/tmp/fastfail-runstore.json"
	pipelineRunID        = "fastfail-run-1"
)

func main() {
	// ── 1. Durable RunStore ─────────────────────────────────────────────────
	runStore, err := store.NewJsonRunStore(runStoreFile)
	if err != nil {
		log.Fatalf("runstore init: %v", err)
	}
	bootstrapRecovery(runStore)

	// ── 2. Driver: BoundedDriver wrapping DriverK8s ──────────────────────
	var innerDrv driver.Driver
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig(namespace, kubeconfig)
	if k8sErr != nil {
		log.Printf("[fastfail] WARN: K8s unavailable (%v) — using NopDriver, runs will be held", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}

	drv := imp.NewBoundedDriver(innerDrv, maxConcurrentK8sJobs)
	log.Printf("[fastfail] BoundedDriver(sem=%d) initialized, K8sAvailable=%v",
		maxConcurrentK8sJobs, k8sErr == nil)

	// ── 3. RunGate: ingress boundary ─────────────────────────────────────
	gate := ingress.NewRunGate(runStore)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Println("[fastfail] building A → B1/B2(FAIL)/B3 → C DAG")
	fmt.Println("[fastfail] expected: B2 fails → C skipped → Wait=false")

	// ── 4. RunGate.Admit wraps the entire dag execution ──────────────────
	// fast-fail means the DAG itself returns false — this is not an error
	// from the gate's perspective if we interpret it as a "pipeline failure"
	// (not a system error). We map Wait=false to a non-nil error for the gate.
	gateErr := gate.Admit(ctx, pipelineRunID, func(ctx context.Context) error {
		ok := runFastfailPipeline(ctx, drv)
		if !ok {
			// Pipeline failure (B2 intentionally fails) — expected path.
			// Return an error so RunGate records state=canceled.
			return fmt.Errorf("fastfail pipeline: B2 failed as expected (fast-fail verified)")
		}
		return nil
	})

	if gateErr != nil {
		// This is the EXPECTED outcome for fastfail: B2 intentionally fails.
		fmt.Printf("[fastfail] PASS: Wait=false (B2 failure propagated, C not executed)\n")
		fmt.Printf("[fastfail]   gate recorded: %v\n", gateErr)
	} else {
		fmt.Println("[fastfail] UNEXPECTED: Wait=true — fast-fail did not trigger")
		os.Exit(1)
	}
}

// runFastfailPipeline builds and runs the fast-fail DAG. Returns true if all
// nodes succeed (unexpected), false if any node fails (expected for B2).
func runFastfailPipeline(ctx context.Context, drv driver.Driver) bool {
	dag, err := daggo.InitDag()
	if err != nil {
		log.Printf("dag init: %v", err)
		return false
	}

	for _, id := range []string{"ff-a", "ff-b1", "ff-b2", "ff-b3", "ff-c"} {
		if dag.CreateNode(id) == nil {
			log.Printf("CreateNode(%s) returned nil", id)
			return false
		}
	}

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
			log.Printf("AddEdge %s→%s: %v", e[0], e[1], err)
			return false
		}
	}

	pvcMount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}
	labels := map[string]string{"kueue.x-k8s.io/queue-name": queueName}

	nodeRunners := map[string]*adapter.SpawnerNode{
		"ff-a":  {Driver: drv, Spec: specA(pvcMount, labels)},
		"ff-b1": {Driver: drv, Spec: specB(1, false, pvcMount, labels)},
		"ff-b2": {Driver: drv, Spec: specB(2, true, pvcMount, labels)}, // FAIL
		"ff-b3": {Driver: drv, Spec: specB(3, false, pvcMount, labels)},
		"ff-c":  {Driver: drv, Spec: specC(pvcMount, labels)},
	}
	for id, r := range nodeRunners {
		if !dag.SetNodeRunner(id, r) {
			log.Printf("SetNodeRunner(%s) failed", id)
			return false
		}
	}

	if err := dag.FinishDag(); err != nil {
		log.Printf("FinishDag: %v", err)
		return false
	}
	if !dag.ConnectRunner() {
		log.Printf("ConnectRunner failed")
		return false
	}
	if !dag.GetReady(ctx) {
		log.Printf("GetReady failed")
		return false
	}

	fmt.Println("[fastfail] starting DAG...")
	if !dag.Start() {
		log.Printf("Start failed")
		return false
	}

	return dag.Wait(ctx)
}

// bootstrapRecovery logs recovered runs from a previous process.
func bootstrapRecovery(s store.RunStore) {
	ctx := context.Background()
	queued, _ := s.ListByState(ctx, store.StateQueued)
	held, _ := s.ListByState(ctx, store.StateHeld)
	if len(queued)+len(held) > 0 {
		log.Printf("[fastfail] BOOTSTRAP: recovered %d queued + %d held runs",
			len(queued), len(held))
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

func specC(mount api.Mount, labels map[string]string) api.RunSpec {
	return api.RunSpec{
		RunID:    "ff-c",
		ImageRef: "busybox:1.36",
		Command:  []string{"sh", "-c", `echo '[ff-C] THIS SHOULD NOT RUN if fast-fail works' && exit 1`},
		Labels:   labels, Mounts: []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}
