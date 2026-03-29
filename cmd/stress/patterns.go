package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	daggo "github.com/seoyhaein/dag-go"
	"github.com/seoyhaein/poc/pkg/adapter"
	"github.com/seoyhaein/spawner/pkg/api"
	"github.com/seoyhaein/spawner/pkg/driver"
)

// ── InstrumentedRunner ────────────────────────────────────────────────────────

// InstrumentedRunner wraps a dag-go Runnable and records entry/exit timing.
// The recorded time covers: semaphore wait + K8s Job Create + K8s Job Wait.
type InstrumentedRunner struct {
	inner  daggo.Runnable
	nodeID string
	m      *RunMetrics
}

var _ daggo.Runnable = (*InstrumentedRunner)(nil)

func (r *InstrumentedRunner) RunE(ctx context.Context, i interface{}) error {
	r.m.Enter(r.nodeID, time.Now())
	err := r.inner.RunE(ctx, i)
	r.m.Done(r.nodeID, time.Now(), err == nil)
	return err
}

// ── RunMetrics ────────────────────────────────────────────────────────────────

// NodeTiming records per-node timing within RunE().
// Duration = sem_wait + K8s_Job_Create + K8s_Job_Wait.
type NodeTiming struct {
	NodeID  string
	EnterAt time.Time
	DoneAt  time.Time
	Success bool
}

// RunMetrics collects per-node timing for one run.
type RunMetrics struct {
	mu    sync.Mutex
	RunID string
	nodes map[string]*NodeTiming
	order []string // insertion order
}

func NewRunMetrics(runID string) *RunMetrics {
	return &RunMetrics{RunID: runID, nodes: make(map[string]*NodeTiming)}
}

func (m *RunMetrics) Enter(nodeID string, t time.Time) {
	m.mu.Lock()
	m.nodes[nodeID] = &NodeTiming{NodeID: nodeID, EnterAt: t}
	m.order = append(m.order, nodeID)
	m.mu.Unlock()
}

func (m *RunMetrics) Done(nodeID string, t time.Time, ok bool) {
	m.mu.Lock()
	if n, exists := m.nodes[nodeID]; exists {
		n.DoneAt = t
		n.Success = ok
	}
	m.mu.Unlock()
}

// Print outputs a per-node timing table and aggregate metrics.
func (m *RunMetrics) Print(wallTotal time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Printf("%-22s  %-12s  %-10s  %s\n", "node", "duration", "ok", "done_at")
	fmt.Printf("%s\n", strings.Repeat("-", 65))
	for _, id := range m.order {
		n := m.nodes[id]
		dur := n.DoneAt.Sub(n.EnterAt)
		ok := "✓"
		if !n.Success {
			ok = "✗"
		}
		fmt.Printf("%-22s  %-12v  %-10s  %s\n",
			id, dur.Round(time.Millisecond), ok, n.DoneAt.UTC().Format("15:04:05.000"))
	}
	fmt.Printf("%s\n", strings.Repeat("-", 65))

	// ── aggregate metrics ────────────────────────────────────────────────────
	var bNodes []*NodeTiming
	var aNode, cNode *NodeTiming
	for _, n := range m.nodes {
		switch {
		case n.NodeID == "a" || strings.HasSuffix(n.NodeID, "-a"):
			aNode = n
		case n.NodeID == "c" || strings.HasSuffix(n.NodeID, "-c") || n.NodeID == "d":
			cNode = n
		case strings.HasPrefix(n.NodeID, "b"):
			bNodes = append(bNodes, n)
		}
	}

	if len(bNodes) > 0 {
		var firstEnter, lastDone time.Time
		for _, b := range bNodes {
			if firstEnter.IsZero() || b.EnterAt.Before(firstEnter) {
				firstEnter = b.EnterAt
			}
			if b.DoneAt.After(lastDone) {
				lastDone = b.DoneAt
			}
		}
		submitBurst := time.Duration(0)
		var enters []time.Time
		for _, b := range bNodes {
			enters = append(enters, b.EnterAt)
		}
		sort.Slice(enters, func(i, j int) bool { return enters[i].Before(enters[j]) })
		if len(enters) > 1 {
			submitBurst = enters[len(enters)-1].Sub(enters[0])
		}

		fmt.Printf("submit_burst_duration (last_B.enter - first_B.enter): %v\n",
			submitBurst.Round(time.Millisecond))
		fmt.Printf("b_stage_duration (last_B.done - first_B.enter):        %v\n",
			lastDone.Sub(firstEnter).Round(time.Millisecond))
		if cNode != nil && !lastDone.IsZero() {
			collectorDelay := cNode.EnterAt.Sub(lastDone)
			fmt.Printf("collector_start_delay (C.enter - last_B.done):         %v\n",
				collectorDelay.Round(time.Millisecond))
		}
	}

	if aNode != nil && cNode != nil {
		critPath := cNode.DoneAt.Sub(aNode.EnterAt)
		fmt.Printf("critical_path (A.enter → C.done):                      %v\n",
			critPath.Round(time.Millisecond))
	}
	fmt.Printf("wall_total:                                             %v\n",
		wallTotal.Round(time.Second))
}

// ── Pattern dispatch ──────────────────────────────────────────────────────────

func runPattern(ctx context.Context, pattern, runID, pBase string, drv driver.Driver, m *RunMetrics) error {
	switch pattern {
	case "wide-fanout-8":
		return wideFanout(ctx, 8, func(i int) int { return 0 }, runID, pBase, drv, m)
	case "long-tail-8":
		return wideFanout(ctx, 8, func(i int) int {
			if i >= 7 {
				return 12 // B7, B8: straggler (12s extra → total ~18s vs ~6s)
			}
			return 0
		}, runID, pBase, drv, m)
	case "mixed-duration-8":
		sleeps := []int{0, 1, 2, 3, 4, 5, 7, 9} // total = ~6, 7, 8, 9, 10, 11, 13, 15s
		return wideFanout(ctx, 8, func(i int) int {
			return sleeps[i-1]
		}, runID, pBase, drv, m)
	case "two-stage-8x4":
		return twoStage8x4(ctx, runID, pBase, drv, m)
	default:
		return fmt.Errorf("unknown pattern: %q", pattern)
	}
}

// ── wideFanout: A → (B1..BN) → C ─────────────────────────────────────────────

// wideFanout builds and runs an A → B1..BN → C dag.
// sleepSec(i) returns extra sleep seconds for worker i (1-indexed).
func wideFanout(ctx context.Context, workers int, sleepSec func(i int) int,
	runID, pBase string, drv driver.Driver, m *RunMetrics) error {

	dag, err := initDag()
	if err != nil {
		return fmt.Errorf("InitDag: %w", err)
	}

	mount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}

	// A prepares all directories upfront
	var mkdirs []string
	mkdirs = append(mkdirs, pBase+"/a-output")
	mkdirs = append(mkdirs, pBase+"/c-output")
	for i := 1; i <= workers; i++ {
		mkdirs = append(mkdirs, fmt.Sprintf("%s/b-output/shard-%d", pBase, i-1))
	}
	nodeA := dag.CreateNode("a")
	if nodeA == nil {
		return fmt.Errorf("CreateNode(a) returned nil")
	}

	bNodes := make([]interface{ SetRunner(daggo.Runnable) bool }, workers)
	for i := 1; i <= workers; i++ {
		id := fmt.Sprintf("b%d", i)
		n := dag.CreateNode(id)
		if n == nil {
			return fmt.Errorf("CreateNode(%s) returned nil", id)
		}
		bNodes[i-1] = n
	}

	nodeC := dag.CreateNode("c")
	if nodeC == nil {
		return fmt.Errorf("CreateNode(c) returned nil")
	}

	// Edges
	if err := dag.AddEdge(daggo.StartNode, "a"); err != nil {
		return fmt.Errorf("AddEdge start→a: %w", err)
	}
	for i := 1; i <= workers; i++ {
		if err := dag.AddEdge("a", fmt.Sprintf("b%d", i)); err != nil {
			return fmt.Errorf("AddEdge a→b%d: %w", i, err)
		}
		if err := dag.AddEdge(fmt.Sprintf("b%d", i), "c"); err != nil {
			return fmt.Errorf("AddEdge b%d→c: %w", i, err)
		}
	}

	// Runners
	if !nodeA.SetRunner(instrument("a", m, &adapter.SpawnerNode{
		Driver: drv,
		Spec:   stressSpecSetup("a", pBase+"/a-output", mkdirs, 0, runID, mount),
	})) {
		return fmt.Errorf("SetRunner(a) failed")
	}

	for i := 1; i <= workers; i++ {
		id := fmt.Sprintf("b%d", i)
		outDir := fmt.Sprintf("%s/b-output/shard-%d", pBase, i-1)
		sl := sleepSec(i)
		if !bNodes[i-1].SetRunner(instrument(id, m, &adapter.SpawnerNode{
			Driver: drv,
			Spec:   stressSpec(id, outDir, sl, runID, mount),
		})) {
			return fmt.Errorf("SetRunner(%s) failed", id)
		}
	}

	if !nodeC.SetRunner(instrument("c", m, &adapter.SpawnerNode{
		Driver: drv,
		Spec:   stressSpecCollect("c", pBase, workers, runID, mount),
	})) {
		return fmt.Errorf("SetRunner(c) failed")
	}

	return execDag(ctx, dag)
}

// ── twoStage8x4: A → (B1..B8) → M → (C1..C4) → D ───────────────────────────

func twoStage8x4(ctx context.Context, runID, pBase string, drv driver.Driver, m *RunMetrics) error {
	dag, err := initDag()
	if err != nil {
		return fmt.Errorf("InitDag: %w", err)
	}

	mount := api.Mount{Source: pvcName, Target: mountPath, ReadOnly: false}

	// Pre-create all dirs in A node
	var mkdirs []string
	mkdirs = append(mkdirs, pBase+"/a-output", pBase+"/m-output", pBase+"/d-output")
	for i := 0; i < 8; i++ {
		mkdirs = append(mkdirs, fmt.Sprintf("%s/b-output/shard-%d", pBase, i))
	}
	for i := 0; i < 4; i++ {
		mkdirs = append(mkdirs, fmt.Sprintf("%s/c-output/shard-%d", pBase, i))
	}

	nodeA := dag.CreateNode("a")
	if nodeA == nil {
		return fmt.Errorf("CreateNode(a) nil")
	}
	bNodes := make([]interface{ SetRunner(daggo.Runnable) bool }, 8)
	for i := 1; i <= 8; i++ {
		n := dag.CreateNode(fmt.Sprintf("b%d", i))
		if n == nil {
			return fmt.Errorf("CreateNode(b%d) nil", i)
		}
		bNodes[i-1] = n
	}
	nodeM := dag.CreateNode("m")
	if nodeM == nil {
		return fmt.Errorf("CreateNode(m) nil")
	}
	cNodes := make([]interface{ SetRunner(daggo.Runnable) bool }, 4)
	for i := 1; i <= 4; i++ {
		n := dag.CreateNode(fmt.Sprintf("c%d", i))
		if n == nil {
			return fmt.Errorf("CreateNode(c%d) nil", i)
		}
		cNodes[i-1] = n
	}
	nodeD := dag.CreateNode("d")
	if nodeD == nil {
		return fmt.Errorf("CreateNode(d) nil")
	}

	// Edges: start→A, A→B1..B8, B1..B8→M, M→C1..C4, C1..C4→D
	if err := dag.AddEdge(daggo.StartNode, "a"); err != nil {
		return err
	}
	for i := 1; i <= 8; i++ {
		if err := dag.AddEdge("a", fmt.Sprintf("b%d", i)); err != nil {
			return err
		}
		if err := dag.AddEdge(fmt.Sprintf("b%d", i), "m"); err != nil {
			return err
		}
	}
	for i := 1; i <= 4; i++ {
		if err := dag.AddEdge("m", fmt.Sprintf("c%d", i)); err != nil {
			return err
		}
		if err := dag.AddEdge(fmt.Sprintf("c%d", i), "d"); err != nil {
			return err
		}
	}

	// Runners
	if !nodeA.SetRunner(instrument("a", m, &adapter.SpawnerNode{
		Driver: drv,
		Spec:   stressSpecSetup("a", pBase+"/a-output", mkdirs, 0, runID, mount),
	})) {
		return fmt.Errorf("SetRunner(a) failed")
	}
	for i := 1; i <= 8; i++ {
		id := fmt.Sprintf("b%d", i)
		outDir := fmt.Sprintf("%s/b-output/shard-%d", pBase, i-1)
		if !bNodes[i-1].SetRunner(instrument(id, m, &adapter.SpawnerNode{
			Driver: drv,
			Spec:   stressSpec(id, outDir, 0, runID, mount),
		})) {
			return fmt.Errorf("SetRunner(%s) failed", id)
		}
	}
	if !nodeM.SetRunner(instrument("m", m, &adapter.SpawnerNode{
		Driver: drv,
		Spec:   stressSpecMerge("m", pBase, 8, runID, mount),
	})) {
		return fmt.Errorf("SetRunner(m) failed")
	}
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("c%d", i)
		outDir := fmt.Sprintf("%s/c-output/shard-%d", pBase, i-1)
		if !cNodes[i-1].SetRunner(instrument(id, m, &adapter.SpawnerNode{
			Driver: drv,
			Spec:   stressSpec(id, outDir, 0, runID, mount),
		})) {
			return fmt.Errorf("SetRunner(%s) failed", id)
		}
	}
	if !nodeD.SetRunner(instrument("d", m, &adapter.SpawnerNode{
		Driver: drv,
		Spec:   stressSpecFinalCollect("d", pBase, 4, runID, mount),
	})) {
		return fmt.Errorf("SetRunner(d) failed")
	}

	return execDag(ctx, dag)
}

// ── DAG execution ─────────────────────────────────────────────────────────────

// withCallerDeadlineOnly returns a DagOption that sets DefaultTimeout=0,
// meaning preFlight relies solely on the caller's ctx deadline rather than
// adding any extra per-node deadline.
//
// Why: dag-go's default DefaultTimeout=30s begins counting from goroutine
// start, not from dependency satisfaction. For long-running patterns
// (long-tail-8, two-stage-8x4) the downstream collector node's preFlight
// budget is exhausted waiting for straggler workers, causing dag.Wait()=false
// even though all K8s Jobs completed successfully.  Setting DefaultTimeout=0
// delegates all deadline control to the caller ctx (30-minute overall
// timeout), which is the correct policy for multi-minute pipelines.
func withCallerDeadlineOnly() daggo.DagOption {
	return daggo.WithDefaultTimeout(0)
}

// initDag returns a new Dag with per-node preflight timeout disabled.
// All patterns must use this instead of daggo.InitDag() directly.
func initDag() (*daggo.Dag, error) {
	return daggo.InitDagWithOptions(withCallerDeadlineOnly())
}

func execDag(ctx context.Context, dag *daggo.Dag) error {
	if err := dag.FinishDag(); err != nil {
		return fmt.Errorf("FinishDag: %w", err)
	}
	if !dag.ConnectRunner() {
		return fmt.Errorf("ConnectRunner failed")
	}
	if !dag.GetReady(ctx) {
		return fmt.Errorf("GetReady failed")
	}
	log.Printf("[stress] dag starting...")
	if !dag.Start() {
		return fmt.Errorf("dag Start failed")
	}
	if !dag.Wait(ctx) {
		return fmt.Errorf("dag Wait: pipeline did not complete successfully")
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// instrument wraps r with InstrumentedRunner recording timing in m.
func instrument(nodeID string, m *RunMetrics, r daggo.Runnable) daggo.Runnable {
	return &InstrumentedRunner{inner: r, nodeID: nodeID, m: m}
}

// stressNodeID returns the K8s Job name for a stress run node.
// Format: "s-{runId}-{node}" (prefix "s-" saves chars vs "poc-").
func stressNodeID(runID, node string) string {
	id := "s-" + runID + "-" + node
	if len(id) <= 63 {
		return id
	}
	suffix := "-" + node
	prefix := "s-"
	maxLen := 63 - len(prefix) - len(suffix)
	if maxLen < 1 {
		maxLen = 1
	}
	return prefix + runID[:maxLen] + suffix
}

// stressSpecSetup: node that creates all directories + writes a-output/done.txt.
// extraMkdirs: additional directories to create (all B, C, M, D subdirs).
func stressSpecSetup(nodeID, outDir string, extraMkdirs []string, sleepSec int, runID string, mount api.Mount) api.RunSpec {
	var lines []string
	lines = append(lines, "set -e")
	for _, d := range extraMkdirs {
		lines = append(lines, "mkdir -p "+d)
	}
	lines = append(lines, "mkdir -p "+outDir)
	lines = append(lines,
		`echo "node=`+nodeID+` started=$(date -u +%Y-%m-%dT%H:%M:%SZ)" > `+outDir+`/done.txt`,
	)
	if sleepSec > 0 {
		lines = append(lines, fmt.Sprintf("sleep %d", sleepSec))
	}
	lines = append(lines,
		`echo "node=`+nodeID+` completed=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> `+outDir+`/done.txt`,
		`echo "[`+nodeID+`] done"`,
	)
	return api.RunSpec{
		RunID:     stressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// stressSpec: a worker node that sleeps and writes to outDir/done.txt.
func stressSpec(nodeID, outDir string, sleepSec int, runID string, mount api.Mount) api.RunSpec {
	lines := []string{
		"set -e",
		`echo "node=` + nodeID + ` started=$(date -u +%Y-%m-%dT%H:%M:%SZ)" > ` + outDir + `/done.txt`,
	}
	if sleepSec > 0 {
		lines = append(lines, fmt.Sprintf("sleep %d", sleepSec))
	}
	lines = append(lines,
		`echo "node=`+nodeID+` completed=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> `+outDir+`/done.txt`,
		`echo "[`+nodeID+`] done"`,
	)
	return api.RunSpec{
		RunID:     stressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// stressSpecCollect: final collect node for wideFanout (reads B shards → report.txt).
func stressSpecCollect(nodeID, pBase string, workers int, runID string, mount api.Mount) api.RunSpec {
	outDir := pBase + "/c-output"
	var lines []string
	lines = append(lines, "set -e")
	for i := 0; i < workers; i++ {
		lines = append(lines, fmt.Sprintf(
			"test -f %s/b-output/shard-%d/done.txt || { echo '[%s] missing shard-%d'; exit 1; }",
			pBase, i, nodeID, i))
	}
	lines = append(lines,
		"REPORT="+outDir+"/report.txt",
		`echo '=== stress collect ===' > "$REPORT"`,
		fmt.Sprintf(`echo 'runId=%s' >> "$REPORT"`, runID),
		fmt.Sprintf(`echo 'workers=%d' >> "$REPORT"`, workers),
		`echo "collected_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$REPORT"`,
	)
	for i := 0; i < workers; i++ {
		lines = append(lines,
			fmt.Sprintf(`cat %s/b-output/shard-%d/done.txt >> "$REPORT"`, pBase, i))
	}
	lines = append(lines, `echo '[collect] report.txt written'`)
	return api.RunSpec{
		RunID:     stressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// stressSpecMerge: M node in two-stage — reads B shards, writes merged.txt.
func stressSpecMerge(nodeID, pBase string, bWorkers int, runID string, mount api.Mount) api.RunSpec {
	outDir := pBase + "/m-output"
	var lines []string
	lines = append(lines, "set -e")
	for i := 0; i < bWorkers; i++ {
		lines = append(lines, fmt.Sprintf(
			"test -f %s/b-output/shard-%d/done.txt || { echo 'missing shard-%d'; exit 1; }",
			pBase, i, i))
	}
	lines = append(lines, "MERGED="+outDir+"/merged.txt",
		`echo '=== merge ===' > "$MERGED"`,
		fmt.Sprintf(`echo 'runId=%s' >> "$MERGED"`, runID),
		`echo "merged_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$MERGED"`,
	)
	for i := 0; i < bWorkers; i++ {
		lines = append(lines,
			fmt.Sprintf(`cat %s/b-output/shard-%d/done.txt >> "$MERGED"`, pBase, i))
	}
	lines = append(lines, `echo '[m] merged.txt written'`)
	return api.RunSpec{
		RunID:     stressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}

// stressSpecFinalCollect: D node in two-stage — reads C shards → final report.
func stressSpecFinalCollect(nodeID, pBase string, cWorkers int, runID string, mount api.Mount) api.RunSpec {
	outDir := pBase + "/d-output"
	var lines []string
	lines = append(lines, "set -e")
	for i := 0; i < cWorkers; i++ {
		lines = append(lines, fmt.Sprintf(
			"test -f %s/c-output/shard-%d/done.txt || { echo 'missing c-shard-%d'; exit 1; }",
			pBase, i, i))
	}
	lines = append(lines,
		"REPORT="+outDir+"/report.txt",
		`echo '=== two-stage final ===' > "$REPORT"`,
		fmt.Sprintf(`echo 'runId=%s' >> "$REPORT"`, runID),
		`echo "finalized_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$REPORT"`,
	)
	for i := 0; i < cWorkers; i++ {
		lines = append(lines,
			fmt.Sprintf(`cat %s/c-output/shard-%d/done.txt >> "$REPORT"`, pBase, i))
	}
	lines = append(lines, `echo '[d] report.txt written'`)
	return api.RunSpec{
		RunID:     stressNodeID(runID, nodeID),
		ImageRef:  "busybox:1.36",
		Command:   []string{"sh", "-c", strings.Join(lines, "\n")},
		Labels:    map[string]string{"kueue.x-k8s.io/queue-name": queueName},
		Mounts:    []api.Mount{mount},
		Resources: api.Resources{CPU: "100m", Memory: "64Mi"},
	}
}
