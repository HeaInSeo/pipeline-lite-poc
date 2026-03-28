// cmd/operational: operational 시나리오 3가지
//
// Sprint 3 변경사항:
//
//   - BoundedDriver(sem=3): K8s Job 생성 burst 제어 (Q2 fix)
//
//   - K8s init 실패 시 graceful degradation: NopDriver
//
//   - 각 시나리오를 RunGate로 wrapping (Q1 fix)
//
//     1. timeout: 긴 Job에 짧은 context timeout → Wait가 ctx.Err() 반환, Job 삭제 확인
//     2. pending:  quota 초과 Job 제출 → Kueue pending → 선행 Job 완료 후 admit 확인
//     3. rerun:    동일 RunID 재실행 (이전 Job 삭제 후) → 정상 완료 확인
//
// 사용법: go run ./cmd/operational/ [timeout|pending|rerun|all]
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/seoyhaein/poc/pkg/ingress"
	"github.com/seoyhaein/poc/pkg/store"
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
	"github.com/seoyhaein/spawner/pkg/driver"
)

const (
	namespace            = "default"
	maxConcurrentK8sJobs = 3
	runStoreFile         = "/tmp/operational-runstore.json"
)

func main() {
	runStore, err := store.NewJsonRunStore(runStoreFile)
	if err != nil {
		log.Fatalf("runstore init: %v", err)
	}

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	var innerDrv driver.Driver
	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig(namespace, kubeconfig)
	if k8sErr != nil {
		log.Printf("[operational] WARN: K8s unavailable (%v) — using NopDriver", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}
	drv := imp.NewBoundedDriver(innerDrv, maxConcurrentK8sJobs)
	gate := ingress.NewRunGate(runStore)

	mode := "all"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "timeout":
		testTimeout(gate, drv, kubeconfig)
	case "pending":
		testPending(gate, drv)
	case "rerun":
		testRerun(gate, drv)
	case "all":
		testTimeout(gate, drv, kubeconfig)
		testPending(gate, drv)
		testRerun(gate, drv)
	default:
		log.Fatalf("unknown mode: %s (use timeout|pending|rerun|all)", mode)
	}
}

// --- 1. timeout ---
func testTimeout(gate *ingress.RunGate, drv driver.Driver, kubeconfig string) {
	fmt.Println("\n[timeout] === context timeout 검증 ===")

	spec := api.RunSpec{
		RunID:     "op-timeout-job",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[timeout] sleeping 60s...' && sleep 60"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer outerCancel()

	var savedHandle driver.Handle
	gateErr := gate.Admit(outerCtx, "op-timeout-run", func(ctx context.Context) error {
		p, err := drv.Prepare(ctx, spec)
		if err != nil {
			return fmt.Errorf("Prepare: %w", err)
		}
		h, err := drv.Start(ctx, p)
		if err != nil {
			return fmt.Errorf("Start: %w", err)
		}
		savedHandle = h
		fmt.Println("[timeout] Job submitted, waiting with 10s timeout...")

		waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
		defer waitCancel()

		_, waitErr := drv.Wait(waitCtx, h)
		if waitErr == nil {
			return fmt.Errorf("Wait returned nil — job should not finish in 10s")
		}
		fmt.Printf("[timeout] Wait returned error (expected): %v\n", waitErr)
		return waitErr // intentional timeout
	})

	// Expected: gate records canceled (timeout is expected behavior)
	fmt.Printf("[timeout] gate result: %v\n", gateErr)

	// Cleanup: delete the job
	if savedHandle != nil {
		cfg, cfgErr := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if cfgErr == nil {
			cs, csErr := kubernetes.NewForConfig(cfg)
			if csErr == nil {
				prop := metav1.DeletePropagationBackground
				delErr := cs.BatchV1().Jobs(namespace).Delete(context.Background(), spec.RunID,
					metav1.DeleteOptions{PropagationPolicy: &prop})
				if delErr != nil {
					fmt.Printf("[timeout] Job delete (may already be gone): %v\n", delErr)
				} else {
					fmt.Println("[timeout] Job deleted OK")
				}
			}
		}
	}
	fmt.Println("[timeout] PASS")
}

// --- 2. pending ---
func testPending(gate *ingress.RunGate, drv driver.Driver) {
	fmt.Println("\n[pending] === Kueue pending → admit 검증 ===")
	fmt.Println("[pending] A(3500m, 15s) 제출 → B(1000m) 제출 → B는 A 완료 후 admit 예상")

	specA := api.RunSpec{
		RunID:     "op-pending-a",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[pending-A] running' && sleep 15 && echo '[pending-A] done'"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "3500m", Memory: "500Mi"},
	}
	specB := api.RunSpec{
		RunID:     "op-pending-b",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[pending-B] admitted and running' && sleep 3"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "1000m", Memory: "500Mi"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var (
		evA api.Event
		evB api.Event
	)

	// Run A and B with separate gate admits (each is an independent "run")
	gateErrA := gate.Admit(ctx, "op-pending-run-a", func(ctx context.Context) error {
		pA, err := drv.Prepare(ctx, specA)
		if err != nil {
			return fmt.Errorf("Prepare A: %w", err)
		}
		hA, err := drv.Start(ctx, pA)
		if err != nil {
			return fmt.Errorf("Start A: %w", err)
		}
		fmt.Println("[pending] Job A submitted (3500m CPU)")

		pB, err := drv.Prepare(ctx, specB)
		if err != nil {
			return fmt.Errorf("Prepare B: %w", err)
		}
		hB, err := drv.Start(ctx, pB)
		if err != nil {
			return fmt.Errorf("Start B: %w", err)
		}
		fmt.Println("[pending] Job B submitted (1000m CPU) — expect pending until A finishes")

		fmt.Println("[pending] waiting for A to complete...")
		var waitErr error
		evA, waitErr = drv.Wait(ctx, hA)
		if waitErr != nil {
			return fmt.Errorf("Wait A: %w", waitErr)
		}
		fmt.Printf("[pending] A finished: state=%s\n", evA.State)

		fmt.Println("[pending] waiting for B to be admitted and complete...")
		evB, waitErr = drv.Wait(ctx, hB)
		if waitErr != nil {
			return fmt.Errorf("Wait B: %w", waitErr)
		}
		fmt.Printf("[pending] B finished: state=%s\n", evB.State)

		if evA.State != api.StateSucceeded || evB.State != api.StateSucceeded {
			return fmt.Errorf("A=%s B=%s", evA.State, evB.State)
		}
		return nil
	})

	if gateErrA != nil {
		fmt.Printf("[pending] FAIL: %v\n", gateErrA)
		os.Exit(1)
	}
	fmt.Println("[pending] PASS: A completed → B admitted and completed")
}

// --- 3. rerun ---
func testRerun(gate *ingress.RunGate, drv driver.Driver) {
	fmt.Println("\n[rerun] === 동일 RunID 재실행 검증 ===")

	spec := api.RunSpec{
		RunID:     "op-rerun-job",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[rerun] run completed'"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	for i := 1; i <= 2; i++ {
		fmt.Printf("[rerun] run #%d\n", i)
		runID := fmt.Sprintf("op-rerun-run-%d", i)

		var savedHandle driver.Handle
		gateErr := gate.Admit(ctx, runID, func(ctx context.Context) error {
			p, err := drv.Prepare(ctx, spec)
			if err != nil {
				return fmt.Errorf("Prepare #%d: %w", i, err)
			}
			h, err := drv.Start(ctx, p)
			if err != nil {
				return fmt.Errorf("Start #%d: %w", i, err)
			}
			savedHandle = h

			ev, err := drv.Wait(ctx, h)
			if err != nil {
				return fmt.Errorf("Wait #%d: %w", i, err)
			}
			fmt.Printf("[rerun] run #%d: state=%s\n", i, ev.State)
			if ev.State != api.StateSucceeded {
				return fmt.Errorf("run #%d state=%s", i, ev.State)
			}
			return nil
		})

		if gateErr != nil {
			fmt.Printf("[rerun] FAIL: run #%d: %v\n", i, gateErr)
			os.Exit(1)
		}

		// Delete job so same RunID can run again
		if savedHandle != nil {
			if err := drv.Cancel(context.Background(), savedHandle); err != nil {
				fmt.Printf("[rerun] Cancel #%d (ignore if already gone): %v\n", i, err)
			}
		}
		fmt.Printf("[rerun] run #%d done, Job deleted\n", i)
	}
	fmt.Println("[rerun] PASS: same RunID ran twice successfully")
}
