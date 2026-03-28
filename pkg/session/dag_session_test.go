package session_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seoyhaein/poc/pkg/session"
)

// ─── mock ─────────────────────────────────────────────────────────────────────

type mockSubmitter struct {
	mu            sync.Mutex
	submitted     []string
	delay         time.Duration
	callCount     atomic.Int64
	concurrent    atomic.Int64
	maxConcurrent atomic.Int64
}

func (m *mockSubmitter) Submit(_ context.Context, nodeID string) error {
	cur := m.concurrent.Add(1)
	defer m.concurrent.Add(-1)
	for {
		prev := m.maxConcurrent.Load()
		if cur <= prev {
			break
		}
		if m.maxConcurrent.CompareAndSwap(prev, cur) {
			break
		}
	}
	m.callCount.Add(1)
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.Lock()
	m.submitted = append(m.submitted, nodeID)
	m.mu.Unlock()
	return nil
}

func (m *mockSubmitter) Submitted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.submitted))
	copy(out, m.submitted)
	return out
}

// ─── ready-node boundary ──────────────────────────────────────────────────────

// TestOnlyReadyNodesSubmitted proves:
// In A→B→C, Start() submits only A (the node with no deps).
// B and C are held — they are NOT forwarded to NodeSubmitter (not K8s Jobs yet).
func TestOnlyReadyNodesSubmitted(t *testing.T) {
	sub := &mockSubmitter{}
	sess := session.NewDagSession("run-001", sub)
	sess.AddNode("A")
	sess.AddNode("B", "A")
	sess.AddNode("C", "B")

	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := sub.Submitted()
	if len(got) != 1 || got[0] != "A" {
		t.Fatalf("expected only [A] submitted after Start, got %v", got)
	}
	held := sess.HeldNodes()
	sort.Strings(held)
	if len(held) != 2 {
		t.Fatalf("expected 2 held nodes (B, C), got %v", held)
	}
	t.Logf("PASS: only A submitted; B, C held (%v)", held)
}

// TestNonReadyNodeNotSubmitted proves:
// B is NOT submitted before A completes (would not be a K8s Job).
// After A succeeds, B is submitted. After B succeeds, C is submitted.
func TestNonReadyNodeNotSubmitted(t *testing.T) {
	sub := &mockSubmitter{}
	sess := session.NewDagSession("run-002", sub)
	sess.AddNode("A")
	sess.AddNode("B", "A")
	sess.AddNode("C", "B")

	_ = sess.Start(context.Background())

	for _, id := range sub.Submitted() {
		if id == "B" {
			t.Fatal("BOUNDARY VIOLATION: B was submitted before A completed")
		}
	}
	t.Log("confirmed: B not submitted before A completes")

	_ = sess.NodeDone(context.Background(), "A", true)
	foundB := false
	for _, id := range sub.Submitted() {
		if id == "B" {
			foundB = true
		}
	}
	if !foundB {
		t.Fatal("B was not submitted after A succeeded")
	}

	for _, id := range sub.Submitted() {
		if id == "C" {
			t.Fatal("BOUNDARY VIOLATION: C was submitted before B completed")
		}
	}

	_ = sess.NodeDone(context.Background(), "B", true)
	foundC := false
	for _, id := range sub.Submitted() {
		if id == "C" {
			foundC = true
		}
	}
	if !foundC {
		t.Fatal("C was not submitted after B succeeded")
	}
	t.Log("PASS: A→B→C submitted in dependency order")
}

// TestFastFail_DependantNotSubmitted proves:
// When B fails, C stays held and is never forwarded to NodeSubmitter.
// C is never created as a K8s Job.
func TestFastFail_DependantNotSubmitted(t *testing.T) {
	sub := &mockSubmitter{}
	sess := session.NewDagSession("run-003", sub)
	sess.AddNode("A")
	sess.AddNode("B", "A")
	sess.AddNode("C", "B")

	_ = sess.Start(context.Background())
	_ = sess.NodeDone(context.Background(), "A", true)
	_ = sess.NodeDone(context.Background(), "B", false) // B FAILS

	for _, id := range sub.Submitted() {
		if id == "C" {
			t.Fatal("BOUNDARY VIOLATION: C was submitted despite B failing")
		}
	}
	held := sess.HeldNodes()
	foundC := false
	for _, id := range held {
		if id == "C" {
			foundC = true
		}
	}
	if !foundC {
		t.Fatal("expected C to remain held after B failed")
	}
	t.Logf("PASS: C remained held after B failed; held: %v", held)
}

// TestFanOut_AllReadyNodesSubmittedAtOnce proves:
// When A succeeds and B1/B2/B3 all depend only on A,
// all three become ready in one NodeDone call.
// C (depends on all three) stays held.
func TestFanOut_AllReadyNodesSubmittedAtOnce(t *testing.T) {
	sub := &mockSubmitter{}
	sess := session.NewDagSession("run-004", sub)
	sess.AddNode("A")
	sess.AddNode("B1", "A")
	sess.AddNode("B2", "A")
	sess.AddNode("B3", "A")
	sess.AddNode("C", "B1", "B2", "B3")

	_ = sess.Start(context.Background())
	if got := sub.Submitted(); len(got) != 1 || got[0] != "A" {
		t.Fatalf("expected only A submitted initially, got %v", got)
	}

	_ = sess.NodeDone(context.Background(), "A", true)
	submitted := sub.Submitted()
	if len(submitted) != 4 {
		t.Fatalf("expected 4 submitted after A done (A+B1+B2+B3), got %v", submitted)
	}
	for _, id := range submitted {
		if id == "C" {
			t.Fatal("BOUNDARY VIOLATION: C submitted before B1/B2/B3 completed")
		}
	}
	t.Logf("PASS: fan-out B1/B2/B3 submitted after A; C still held")
}

// TestBurstBoundary_AllReadyAtStart is an OBSERVATION test.
// It shows that when N independent nodes are all ready at Start(),
// DagSession.submitBatch calls Submit N times sequentially.
// Without a NodeAttemptQueue, this is N direct calls to downstream (K8s API).
// See ARCH_BOUNDARY.md §7: NodeAttemptQueue with sem=3 would cap this.
func TestBurstBoundary_AllReadyAtStart(t *testing.T) {
	const n = 10
	sub := &mockSubmitter{delay: 5 * time.Millisecond}
	sess := session.NewDagSession("run-burst", sub)
	for i := 0; i < n; i++ {
		sess.AddNode(fmt.Sprintf("node-%02d", i))
	}

	start := time.Now()
	_ = sess.Start(context.Background())
	elapsed := time.Since(start)

	calls := sub.callCount.Load()
	maxConc := sub.maxConcurrent.Load()
	if calls != n {
		t.Fatalf("expected %d Submit calls, got %d", n, calls)
	}
	t.Logf("OBSERVATION: %d nodes, %d calls, max_concurrent=%d, elapsed=%s",
		n, calls, maxConc, elapsed.Round(time.Millisecond))
	t.Logf("OBSERVATION: submitBatch is sequential → max_concurrent=1")
	t.Logf("OBSERVATION: caller can fire N DagSessions concurrently → N×M K8s API calls with no cap")
	t.Logf("OBSERVATION: NodeAttemptQueue(sem=3) proposed in ARCH_BOUNDARY.md §7")
}
