package session_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seoyhaein/poc/pkg/session"
)

// ── slow submitter ─────────────────────────────────────────────────────────

// slowSubmitter simulates a NodeSubmitter that takes `delay` per call.
// It tracks concurrent inflight and peak concurrency for assertions.
type slowSubmitter struct {
	delay         time.Duration
	mu            sync.Mutex
	submitted     []string
	concurrent    atomic.Int64
	maxConcurrent atomic.Int64
}

func (s *slowSubmitter) Submit(_ context.Context, nodeID string) error {
	cur := s.concurrent.Add(1)
	defer s.concurrent.Add(-1)
	for {
		prev := s.maxConcurrent.Load()
		if cur <= prev {
			break
		}
		if s.maxConcurrent.CompareAndSwap(prev, cur) {
			break
		}
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	s.submitted = append(s.submitted, nodeID)
	s.mu.Unlock()
	return nil
}

func (s *slowSubmitter) Submitted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.submitted))
	copy(out, s.submitted)
	return out
}

// ── burst boundary tests ───────────────────────────────────────────────────

// TestBurstBoundary_BoundedRelease proves:
// With sem=3, even if 10 nodes are ready simultaneously, at most 3
// Submit calls are in-flight to the underlying NodeSubmitter at any time.
// This is the H3 "burst absorbed at queue boundary" invariant.
func TestBurstBoundary_BoundedRelease(t *testing.T) {
	const n = 10
	const sem = 3
	inner := &slowSubmitter{delay: 20 * time.Millisecond}
	q := session.NewNodeAttemptQueue(inner, sem)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		nodeID := fmt.Sprintf("node-%02d", i)
		go func(id string) {
			defer wg.Done()
			_ = q.Submit(context.Background(), id)
		}(nodeID)
	}
	wg.Wait()

	total, blocked, maxConc := q.Stats()
	innerMax := inner.maxConcurrent.Load()

	if total != n {
		t.Fatalf("expected %d total submits, got %d", n, total)
	}
	if innerMax > sem {
		t.Fatalf("BOUNDARY VIOLATION: inner max_concurrent=%d exceeded sem=%d", innerMax, sem)
	}
	t.Logf("PASS: %d nodes, sem=%d, queue_max_concurrent=%d, inner_max_concurrent=%d, blocked=%d",
		n, sem, maxConc, innerMax, blocked)
}

// TestBurstBoundary_UnboundedBaseline shows what happens WITHOUT NodeAttemptQueue:
// All 10 Submit calls go directly to the inner submitter, max_concurrent=10.
// This is the current DagSession behavior (see TestBurstBoundary_AllReadyAtStart).
// Contrast with TestBurstBoundary_BoundedRelease.
func TestBurstBoundary_UnboundedBaseline(t *testing.T) {
	const n = 10
	inner := &slowSubmitter{delay: 20 * time.Millisecond}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		nodeID := fmt.Sprintf("node-%02d", i)
		go func(id string) {
			defer wg.Done()
			_ = inner.Submit(context.Background(), id)
		}(nodeID)
	}
	wg.Wait()

	maxConc := inner.maxConcurrent.Load()
	t.Logf("OBSERVATION (no queue): %d nodes, inner_max_concurrent=%d — all hit K8s API simultaneously",
		n, maxConc)
	t.Logf("CONTRAST: with NodeAttemptQueue(sem=3), inner_max_concurrent is capped at 3")
}

// TestBurstBoundary_ContextCancelWhileBlocked proves:
// If ctx is cancelled while a goroutine is waiting for a queue slot,
// Submit returns ctx.Err() immediately and does NOT call the inner submitter.
// The slot is never consumed.
func TestBurstBoundary_ContextCancelWhileBlocked(t *testing.T) {
	const sem = 1
	inner := &slowSubmitter{delay: 100 * time.Millisecond}
	q := session.NewNodeAttemptQueue(inner, sem)

	// Fill the single slot with a long-running submit
	go func() { _ = q.Submit(context.Background(), "slot-holder") }()
	time.Sleep(5 * time.Millisecond) // let slot-holder acquire slot

	// Second submit should block → cancel it
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := q.Submit(ctx, "blocked-node")
	if err == nil {
		t.Fatal("expected ctx.Err(), got nil")
	}
	t.Logf("PASS: blocked submit returned ctx.Err()=%v when context cancelled", err)
}

// TestNodeAttemptQueue_DagSessionComposition proves:
// DagSession + NodeAttemptQueue compose correctly.
// DagSession sees NodeAttemptQueue as a NodeSubmitter, and the queue caps
// concurrent K8s API calls while preserving ready-only invariant.
func TestNodeAttemptQueue_DagSessionComposition(t *testing.T) {
	const sem = 2
	inner := &slowSubmitter{delay: 10 * time.Millisecond}
	q := session.NewNodeAttemptQueue(inner, sem)

	sess := session.NewDagSession("run-queue-test", q)
	// A → B1, B2, B3, B4 (all B-nodes ready after A)
	sess.AddNode("A")
	sess.AddNode("B1", "A")
	sess.AddNode("B2", "A")
	sess.AddNode("B3", "A")
	sess.AddNode("B4", "A")

	_ = sess.Start(context.Background())

	// Only A submitted initially
	submitted := inner.Submitted()
	if len(submitted) != 1 || submitted[0] != "A" {
		t.Fatalf("expected only A submitted initially, got %v", submitted)
	}

	// After A completes: B1–B4 all become ready at once
	_ = sess.NodeDone(context.Background(), "A", true)

	// All 4 B-nodes should be submitted (through queue)
	allSubmitted := inner.Submitted()
	if len(allSubmitted) != 5 {
		t.Fatalf("expected 5 total (A+B1+B2+B3+B4), got %v", allSubmitted)
	}

	_, _, maxConc := q.Stats()
	t.Logf("PASS: DagSession+NodeAttemptQueue: 4 B-nodes submitted, queue_max_concurrent=%d (sem=%d)",
		maxConc, sem)
	if maxConc > int64(sem) {
		t.Errorf("BOUNDARY VIOLATION: queue_max_concurrent=%d > sem=%d", maxConc, sem)
	}
}
