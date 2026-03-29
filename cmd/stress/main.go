// cmd/stress: synthetic pipeline stress harness
//
// 목적: 실제 유전체 툴 없이, 구조적 리스크를 드러내는 synthetic 패턴을
// kind 클러스터에서 실행하여 BoundedDriver/submit burst/collect 병목을 실측한다.
//
// 사용법:
//
//	go run ./cmd/stress/ --pattern wide-fanout-8 --sem 4
//	go run ./cmd/stress/ --pattern long-tail-8   --sem 4
//	go run ./cmd/stress/ --pattern mixed-duration-8
//	go run ./cmd/stress/ --pattern two-stage-8x4  --sem 4
//	go run ./cmd/stress/ --pattern multi-run-burst --runs 5 --sem 4
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/seoyhaein/poc/pkg/ingress"
	"github.com/seoyhaein/poc/pkg/store"
	"github.com/seoyhaein/spawner/cmd/imp"
	"github.com/seoyhaein/spawner/pkg/driver"
)

const (
	pvcName      = "poc-shared-pvc"
	mountPath    = "/data"
	queueName    = "poc-standard-lq"
	namespace    = "default"
	defaultSem   = 4
	runStoreFile = "/tmp/stress-runstore.json"
	dataRoot     = "/data/stress"
)

func main() {
	var (
		pattern       string
		sem           int
		runs          int
		runIDOverride string
	)
	flag.StringVar(&pattern, "pattern", "wide-fanout-8",
		"wide-fanout-8 | two-stage-8x4 | long-tail-8 | mixed-duration-8 | multi-run-burst")
	flag.IntVar(&sem, "sem", defaultSem, "BoundedDriver max concurrent K8s Job creates")
	flag.IntVar(&runs, "runs", 5, "concurrent runs (multi-run-burst only)")
	flag.StringVar(&runIDOverride, "run-id", "", "custom run ID prefix (default: auto-generated)")
	flag.Parse()

	baseRunID := runIDOverride
	if baseRunID == "" {
		baseRunID = generateRunID()
	}

	// ── RunStore ────────────────────────────────────────────────────────────
	rs, err := store.NewJsonRunStore(runStoreFile)
	if err != nil {
		log.Fatalf("[stress] runstore: %v", err)
	}

	// ── Driver ──────────────────────────────────────────────────────────────
	var innerDrv driver.Driver
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	k8sDrv, k8sErr := imp.NewK8sFromKubeconfig(namespace, kubeconfig)
	if k8sErr != nil {
		log.Printf("[stress] WARN: K8s unavailable (%v) — NopDriver, runs will fail", k8sErr)
		innerDrv = &imp.NopDriver{}
	} else {
		innerDrv = k8sDrv
	}
	drv := imp.NewBoundedDriver(innerDrv, sem)
	log.Printf("[stress] pattern=%s sem=%d K8sAvailable=%v", pattern, sem, k8sErr == nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// ── BoundedDriver stats goroutine ────────────────────────────────────────
	statsCtx, statsCancel := context.WithCancel(ctx)
	defer statsCancel()
	startBoundedStats(statsCtx, drv)

	gate := ingress.NewRunGate(rs)

	// ── Dispatch ─────────────────────────────────────────────────────────────
	if pattern == "multi-run-burst" {
		runMultiBurst(ctx, drv, gate, baseRunID, runs, sem)
		return
	}

	runID := baseRunID
	pBase := dataRoot + "/" + runID
	m := NewRunMetrics(runID)
	storeID := "stress-" + pattern + "-" + runID

	wallStart := time.Now()
	if err := gate.Admit(ctx, storeID, func(ctx context.Context) error {
		return runPattern(ctx, pattern, runID, pBase, drv, m)
	}); err != nil {
		log.Printf("[stress] FAIL (K8s path): %v", err)
		statsCancel()
		os.Exit(1)
	}
	wallTotal := time.Since(wallStart)
	statsCancel()

	fmt.Printf("\n=== %s results (sem=%d) ===\n", pattern, sem)
	m.Print(wallTotal)
}

// runMultiBurst launches nRuns instances of wide-fanout-8 concurrently,
// sharing the same BoundedDriver. Checks artifact/state isolation.
func runMultiBurst(ctx context.Context, drv *imp.BoundedDriver, gate *ingress.RunGate, baseRunID string, nRuns int, sem int) {
	type result struct {
		runID string
		total time.Duration
		err   error
	}
	results := make([]result, nRuns)
	var wg sync.WaitGroup

	wallStart := time.Now()
	for i := 0; i < nRuns; i++ {
		i := i
		runID := fmt.Sprintf("%s-r%d", baseRunID, i+1)
		pBase := dataRoot + "/" + runID
		m := NewRunMetrics(runID)
		storeID := "stress-burst-" + runID

		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.Now()
			err := gate.Admit(ctx, storeID, func(ctx context.Context) error {
				return runPattern(ctx, "wide-fanout-8", runID, pBase, drv, m)
			})
			results[i] = result{runID: runID, total: time.Since(t), err: err}
		}()
	}
	wg.Wait()
	wallTotal := time.Since(wallStart)

	fmt.Printf("\n=== multi-run-burst (%d runs, sem=%d) ===\n", nRuns, sem)
	successes := 0
	for i, r := range results {
		status := "PASS"
		if r.err != nil {
			status = fmt.Sprintf("FAIL(%v)", r.err)
		} else {
			successes++
		}
		fmt.Printf("  run[%d] %s  total=%-8v  %s\n", i+1, r.runID[len(r.runID)-10:], r.total.Round(time.Second), status)
	}
	fmt.Printf("  succeeded: %d/%d  wall_total: %v\n", successes, nRuns, wallTotal.Round(time.Second))
	fmt.Printf("  isolation: each run used /data/stress/{runId}/... (unique paths)\n")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func generateRunID() string {
	t := time.Now().UTC()
	return fmt.Sprintf("%s-%03d", t.Format("20060102-150405"), t.Nanosecond()/1e6)
}

func startBoundedStats(ctx context.Context, drv *imp.BoundedDriver) {
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		var peak int64
		for {
			select {
			case <-ctx.Done():
				log.Printf("[bounded-stats] FINAL peak_inflight=%d max=%d",
					peak, drv.Stats().MaxConcurrent)
				return
			case <-ticker.C:
				s := drv.Stats()
				if s.Inflight > peak {
					peak = s.Inflight
					log.Printf("[bounded-stats] NEW PEAK inflight=%d available=%d max=%d",
						s.Inflight, s.Available, s.MaxConcurrent)
				}
			}
		}
	}()
}
