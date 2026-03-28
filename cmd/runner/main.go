// cmd/runner: spawner K8s driver로 Job 1개 생성/완료/실패 확인
//
// Sprint 3 변경사항:
//   - RunGate + JsonRunStore: ingress boundary 등록 (Q1 fix)
//   - BoundedDriver(sem=1): 단일 Job이므로 sem=1으로도 충분 (Q2 fix)
//   - K8s init 실패 시 graceful degradation: NopDriver
//
// 사용법: go run ./cmd/runner/ [success|fail]
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/seoyhaein/poc/pkg/ingress"
	"github.com/seoyhaein/poc/pkg/store"
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
	"github.com/seoyhaein/spawner/pkg/driver"
)

const (
	maxConcurrentK8sJobs = 3
	runStoreFile         = "/tmp/runner-runstore.json"
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
	ns := "default"
	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig(ns, kubeconfig)
	if k8sErr != nil {
		log.Printf("[runner] WARN: K8s unavailable (%v) — using NopDriver", k8sErr)
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
		runJob(gate, drv, "poc-test-success", successSpec())
	case "fail":
		runJob(gate, drv, "poc-test-fail", failSpec())
	default:
		log.Fatalf("unknown mode: %s (use 'success' or 'fail')", mode)
	}
}

func successSpec() api.RunSpec {
	return api.RunSpec{
		RunID:     "poc-test-success",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[runner] success job started' && sleep 3 && echo '[runner] done'"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "100m", Memory: "128Mi"},
	}
}

func failSpec() api.RunSpec {
	return api.RunSpec{
		RunID:     "poc-test-fail",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[runner] fail job started' && sleep 2 && exit 1"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "100m", Memory: "128Mi"},
	}
}

func runJob(gate *ingress.RunGate, drv driver.Driver, label string, spec api.RunSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	runID := "runner-" + label
	fmt.Printf("[runner] --- %s ---\n", label)

	gateErr := gate.Admit(ctx, runID, func(ctx context.Context) error {
		fmt.Printf("[runner] Preparing job: %s\n", spec.RunID)
		prepared, err := drv.Prepare(ctx, spec)
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}

		fmt.Printf("[runner] Starting job...\n")
		handle, err := drv.Start(ctx, prepared)
		if err != nil {
			return fmt.Errorf("start: %w", err)
		}
		fmt.Printf("[runner] Job submitted. Waiting...\n")

		event, err := drv.Wait(ctx, handle)
		if err != nil {
			return fmt.Errorf("wait: %w", err)
		}

		fmt.Printf("[runner] Result: state=%s run_id=%s\n", event.State, event.RunID)
		if event.Message != "" {
			fmt.Printf("[runner] Message: %s\n", event.Message)
		}

		if event.State == api.StateSucceeded {
			fmt.Printf("[runner] PASS: job succeeded as expected\n")
			return nil
		} else if event.State == api.StateFailed && label == "poc-test-fail" {
			fmt.Printf("[runner] PASS: job failed as expected\n")
			// Return error so RunGate records canceled — this is intentional failure.
			return fmt.Errorf("job failed as expected (intentional)")
		}
		return fmt.Errorf("UNEXPECTED result: %s", event.State)
	})

	if gateErr != nil && label != "poc-test-fail" {
		fmt.Printf("[runner] UNEXPECTED: %v\n", gateErr)
		os.Exit(1)
	}
}
