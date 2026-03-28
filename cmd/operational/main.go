// cmd/operational: Day 9 검증용 — operational 시나리오 3가지
//
//  1. timeout: 긴 Job에 짧은 context timeout → Wait가 ctx.Err() 반환, Job 삭제 확인
//  2. pending:  quota 초과 Job 제출 → Kueue pending → 선행 Job 완료 후 admit 확인
//  3. rerun:    동일 RunID 재실행 (이전 Job 삭제 후) → 정상 완료 확인
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

	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/api"
)

const namespace = "default"

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

	mode := "all"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "timeout":
		testTimeout(drv, kubeconfig)
	case "pending":
		testPending(drv)
	case "rerun":
		testRerun(drv)
	case "all":
		testTimeout(drv, kubeconfig)
		testPending(drv)
		testRerun(drv)
	default:
		log.Fatalf("unknown mode: %s (use timeout|pending|rerun|all)", mode)
	}
}

// --- 1. timeout ---
// 60초 sleep Job에 10초 timeout context → ctx cancel → Wait returns ctx.Err() → Job 삭제
func testTimeout(drv *imp.DriverK8s, kubeconfig string) {
	fmt.Println("\n[timeout] === context timeout 검증 ===")

	spec := api.RunSpec{
		RunID:     "op-timeout-job",
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", "echo '[timeout] sleeping 60s...' && sleep 60"},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": "poc-standard-lq"},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := drv.Prepare(ctx, spec)
	if err != nil {
		log.Fatalf("[timeout] Prepare: %v", err)
	}
	h, err := drv.Start(ctx, p)
	if err != nil {
		log.Fatalf("[timeout] Start: %v", err)
	}
	fmt.Println("[timeout] Job submitted, waiting with 10s timeout...")

	_, waitErr := drv.Wait(ctx, h)
	if waitErr == nil {
		fmt.Println("[timeout] UNEXPECTED: Wait returned nil (job should not finish in 10s)")
		os.Exit(1)
	}
	fmt.Printf("[timeout] Wait returned error (expected): %v\n", waitErr)

	// Job 정리 (context가 이미 취소됐으므로 background context 사용)
	cfg, cfgErr := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if cfgErr != nil {
		log.Fatalf("[timeout] kubeconfig: %v", cfgErr)
	}
	cs, csErr := kubernetes.NewForConfig(cfg)
	if csErr != nil {
		log.Fatalf("[timeout] clientset: %v", csErr)
	}
	prop := metav1.DeletePropagationBackground
	delErr := cs.BatchV1().Jobs(namespace).Delete(context.Background(), spec.RunID, metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
	if delErr != nil {
		fmt.Printf("[timeout] Job delete (may already be gone): %v\n", delErr)
	} else {
		fmt.Println("[timeout] Job deleted OK")
	}
	fmt.Println("[timeout] PASS")
}

// --- 2. pending ---
// standard-cq quota: 4 CPU / 4Gi
// Job A: 3500m CPU (admitted) → Job B: 1000m CPU (pending, 합산 4500m > 4000m quota)
// → A 완료 후 B admitted
func testPending(drv *imp.DriverK8s) {
	fmt.Println("\n[pending] === Kueue pending → admit 검증 ===")
	fmt.Println("[pending] A(3500m, 15s) 제출 → B(1000m) 제출 → B는 A 완료 후 admit 예상")

	bgCtx := context.Background()

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

	pA, err := drv.Prepare(bgCtx, specA)
	if err != nil {
		log.Fatalf("[pending] Prepare A: %v", err)
	}
	hA, err := drv.Start(bgCtx, pA)
	if err != nil {
		log.Fatalf("[pending] Start A: %v", err)
	}
	fmt.Println("[pending] Job A submitted (3500m CPU)")

	pB, err := drv.Prepare(bgCtx, specB)
	if err != nil {
		log.Fatalf("[pending] Prepare B: %v", err)
	}
	hB, err := drv.Start(bgCtx, pB)
	if err != nil {
		log.Fatalf("[pending] Start B: %v", err)
	}
	fmt.Println("[pending] Job B submitted (1000m CPU) — expect pending until A finishes")

	ctxWait, cancelWait := context.WithTimeout(bgCtx, 3*time.Minute)
	defer cancelWait()

	fmt.Println("[pending] waiting for A to complete...")
	evA, err := drv.Wait(ctxWait, hA)
	if err != nil {
		log.Fatalf("[pending] Wait A: %v", err)
	}
	fmt.Printf("[pending] A finished: state=%s\n", evA.State)

	fmt.Println("[pending] waiting for B to be admitted and complete...")
	evB, err := drv.Wait(ctxWait, hB)
	if err != nil {
		log.Fatalf("[pending] Wait B: %v", err)
	}
	fmt.Printf("[pending] B finished: state=%s\n", evB.State)

	if evA.State == api.StateSucceeded && evB.State == api.StateSucceeded {
		fmt.Println("[pending] PASS: A completed → B admitted and completed")
	} else {
		fmt.Printf("[pending] FAIL: A=%s B=%s\n", evA.State, evB.State)
		os.Exit(1)
	}
}

// --- 3. rerun ---
// 동일 RunID를 두 번 제출 (첫 번째 완료 후 Job 삭제 → 두 번째 동일 RunID로 재실행)
func testRerun(drv *imp.DriverK8s) {
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

		p, err := drv.Prepare(ctx, spec)
		if err != nil {
			log.Fatalf("[rerun] Prepare #%d: %v", i, err)
		}
		h, err := drv.Start(ctx, p)
		if err != nil {
			log.Fatalf("[rerun] Start #%d: %v", i, err)
		}

		ev, err := drv.Wait(ctx, h)
		if err != nil {
			log.Fatalf("[rerun] Wait #%d: %v", i, err)
		}
		fmt.Printf("[rerun] run #%d: state=%s\n", i, ev.State)
		if ev.State != api.StateSucceeded {
			fmt.Printf("[rerun] FAIL: run #%d state=%s\n", i, ev.State)
			os.Exit(1)
		}

		// 다음 재실행을 위해 Job 삭제
		if err := drv.Cancel(context.Background(), h); err != nil {
			fmt.Printf("[rerun] Cancel #%d (ignore if already gone): %v\n", i, err)
		}
		fmt.Printf("[rerun] run #%d done, Job deleted\n", i)
	}
	fmt.Println("[rerun] PASS: same RunID ran twice successfully")
}
