package session

import (
	"context"
	"sync/atomic"
)

// NodeAttemptQueue is a FIFO + semaphore-bounded wrapper around NodeSubmitter.
//
// It enforces a concurrency limit (sem) on simultaneous Submit calls, preventing
// a burst of newly-ready nodes from becoming an equal burst of K8s API calls.
//
// Design:
//   - FIFO: callers block in Submit() order (Go channel semantics).
//   - Block: Submit() blocks until a slot is available or ctx is cancelled.
//     It does NOT drop — dropped submissions would skip K8s Jobs silently.
//   - Semaphore: `sem` is a buffered channel of size `concurrency`.
//
// This is the "release control seed" — v1 only limits concurrent submission.
//
// ASSUMPTION: production adds a priority queue, fair-scheduling, and
// back-pressure signaling. v1 does not address these.
//
// ASSUMPTION: NodeAttemptQueue does not track submitted nodes.
// That remains DagSession's responsibility.
type NodeAttemptQueue struct {
	sem   chan struct{}
	inner NodeSubmitter

	// observation counters (for tests and observability)
	totalSubmits    atomic.Int64
	blockedSubmits  atomic.Int64
	maxConcurrent   atomic.Int64
	currentInflight atomic.Int64
}

// NewNodeAttemptQueue returns a NodeAttemptQueue that allows at most
// `concurrency` simultaneous Submit calls to the underlying NodeSubmitter.
// Callers beyond the limit block until a slot is freed.
func NewNodeAttemptQueue(inner NodeSubmitter, concurrency int) *NodeAttemptQueue {
	return &NodeAttemptQueue{
		sem:   make(chan struct{}, concurrency),
		inner: inner,
	}
}

// Submit acquires a slot (blocking if all slots are occupied), forwards to the
// inner NodeSubmitter, then releases the slot.
// Returns ctx.Err() if the context is cancelled while waiting for a slot.
func (q *NodeAttemptQueue) Submit(ctx context.Context, nodeID string) error {
	q.totalSubmits.Add(1)

	// Try to acquire slot without blocking first (fast path).
	select {
	case q.sem <- struct{}{}:
		// slot acquired immediately
	default:
		// All slots occupied: block until one frees or ctx cancels.
		q.blockedSubmits.Add(1)
		select {
		case q.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Track inflight count for max_concurrent observation.
	cur := q.currentInflight.Add(1)
	for {
		prev := q.maxConcurrent.Load()
		if cur <= prev {
			break
		}
		if q.maxConcurrent.CompareAndSwap(prev, cur) {
			break
		}
	}

	defer func() {
		q.currentInflight.Add(-1)
		<-q.sem
	}()

	return q.inner.Submit(ctx, nodeID)
}

// Stats returns observation counters for testing and monitoring.
// Returns (total, blocked, maxConcurrent).
func (q *NodeAttemptQueue) Stats() (total, blocked, maxConcurrent int64) {
	return q.totalSubmits.Load(), q.blockedSubmits.Load(), q.maxConcurrent.Load()
}

// Concurrency returns the semaphore size (max concurrent submits).
func (q *NodeAttemptQueue) Concurrency() int {
	return cap(q.sem)
}
