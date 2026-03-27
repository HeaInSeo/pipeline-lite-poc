// cmd/runner: Day 3 검증용 - spawner K8s driver로 Job 1개 생성/완료/실패 확인
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
)

func main() {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	ns := "default"

	drv, err := imp.NewK8sFromKubeconfig(ns, kubeconfig)
	if err != nil {
		log.Fatalf("driver init: %v", err)
	}

	mode := "success"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "success":
		runJob(drv, ns, "poc-test-success", successSpec())
	case "fail":
		runJob(drv, ns, "poc-test-fail", failSpec())
	default:
		log.Fatalf("unknown mode: %s (use 'success' or 'fail')", mode)
	}
}

func successSpec() api.RunSpec {
	return api.RunSpec{
		RunID:    "poc-test-success",
		ImageRef: "busybox:1.36",
		Command:  []string{"sh", "-c", "echo '[runner] success job started' && sleep 3 && echo '[runner] done'"},
		Labels: map[string]string{
			"kueue.x-k8s.io/queue-name": "poc-standard-lq",
		},
		Resources: api.Resources{
			CPU:    "100m",
			Memory: "128Mi",
		},
	}
}

func failSpec() api.RunSpec {
	return api.RunSpec{
		RunID:    "poc-test-fail",
		ImageRef: "busybox:1.36",
		Command:  []string{"sh", "-c", "echo '[runner] fail job started' && sleep 2 && exit 1"},
		Labels: map[string]string{
			"kueue.x-k8s.io/queue-name": "poc-standard-lq",
		},
		Resources: api.Resources{
			CPU:    "100m",
			Memory: "128Mi",
		},
	}
}

func runJob(drv *imp.DriverK8s, ns, label string, spec api.RunSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	fmt.Printf("[runner] --- %s ---\n", label)
	fmt.Printf("[runner] Preparing job: %s\n", spec.RunID)

	prepared, err := drv.Prepare(ctx, spec)
	if err != nil {
		log.Fatalf("prepare: %v", err)
	}

	fmt.Printf("[runner] Starting job...\n")
	handle, err := drv.Start(ctx, prepared)
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	fmt.Printf("[runner] Job submitted. Waiting...\n")

	event, err := drv.Wait(ctx, handle)
	if err != nil {
		log.Fatalf("wait: %v", err)
	}

	fmt.Printf("[runner] Result: state=%s run_id=%s\n", event.State, event.RunID)
	if event.Message != "" {
		fmt.Printf("[runner] Message: %s\n", event.Message)
	}

	if event.State == api.StateSucceeded {
		fmt.Printf("[runner] PASS: job succeeded as expected\n")
	} else if event.State == api.StateFailed && label == "poc-test-fail" {
		fmt.Printf("[runner] PASS: job failed as expected\n")
	} else {
		fmt.Printf("[runner] UNEXPECTED result: %s\n", event.State)
		os.Exit(1)
	}
}
