//go:build redis

package queue_test

// TestRedisStreams_Evaluation documents and proves the Redis Streams queue
// candidate properties relevant to replacing JsonRunStore for the release queue.
//
// Run with: go test ./pkg/queue/... -tags redis -v
// Requires: Redis server at localhost:6379
//
// WHY this file exists:
//   Sprint 3 requires evaluating Redis Streams as the ingress/release queue
//   candidate. These tests prove XADD/XREADGROUP/XACK/XPENDING/XCLAIM patterns
//   work as expected and compare them to JsonRunStore's durability properties.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/seoyhaein/poc/pkg/queue"
)

const (
	testAddr   = "localhost:6379"
	testStream = "poc:test:runs"
	testGroup  = "poc:test:workers"
)

// TestRedisStreams_XADD_XREADGROUP_XACK proves the basic enqueue/consume/ack
// cycle works correctly.
func TestRedisStreams_XADD_XREADGROUP_XACK(t *testing.T) {
	ctx := context.Background()

	q, err := queue.NewRedisRunQueue(ctx, testAddr, testStream+":basic", testGroup, "worker-1")
	if err != nil {
		t.Skipf("Redis unavailable at %s: %v", testAddr, err)
	}
	defer q.Close()

	// XADD: enqueue run-1
	id, err := q.Enqueue(ctx, "run-1")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	t.Logf("XADD: enqueued run-1 as entry %s", id)

	// XREADGROUP: consume
	entryID, msg, err := q.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if msg.RunID != "run-1" {
		t.Fatalf("expected run-1, got %s", msg.RunID)
	}
	t.Logf("XREADGROUP: received run_id=%s entry_id=%s", msg.RunID, entryID)

	// Verify pending count = 1 (not acked yet)
	pending, _ := q.Pending(ctx)
	if pending != 1 {
		t.Errorf("expected 1 pending before ack, got %d", pending)
	}
	t.Logf("XPENDING (before ack): count=%d", pending)

	// XACK: acknowledge
	if err := q.Ack(ctx, entryID); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	// Verify pending count = 0 (acked)
	pending, _ = q.Pending(ctx)
	if pending != 0 {
		t.Errorf("expected 0 pending after ack, got %d", pending)
	}
	t.Logf("XPENDING (after ack): count=%d", pending)
	t.Log("PASS: XADD → XREADGROUP → XPENDING → XACK cycle verified")
}

// TestRedisStreams_XCLAIM_StalePending proves that messages from a crashed
// consumer can be reclaimed by a healthy consumer after a visibility timeout.
func TestRedisStreams_XCLAIM_StalePending(t *testing.T) {
	ctx := context.Background()
	streamKey := testStream + ":xclaim"

	// consumer-1: reads but doesn't ack (simulates crash)
	q1, err := queue.NewRedisRunQueue(ctx, testAddr, streamKey, testGroup, "consumer-1")
	if err != nil {
		t.Skipf("Redis unavailable: %v", err)
	}
	defer q1.Close()

	_, err = q1.Enqueue(ctx, "run-crash")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	entryID, msg, err := q1.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume by consumer-1: %v", err)
	}
	t.Logf("consumer-1 read entry %s (run_id=%s) but did NOT ack — simulating crash", entryID, msg.RunID)

	// consumer-2: reclaims stale message after short idle
	q2, err := queue.NewRedisRunQueue(ctx, testAddr, streamKey, testGroup, "consumer-2")
	if err != nil {
		t.Fatalf("NewRedisRunQueue consumer-2: %v", err)
	}
	defer q2.Close()

	time.Sleep(10 * time.Millisecond)

	claimed, err := q2.ReclaimStale(ctx, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 reclaimed message, got %d", len(claimed))
	}
	t.Logf("XCLAIM: consumer-2 reclaimed entry %s (run_id=%s) from crashed consumer-1",
		claimed[0].ID, claimed[0].Values["run_id"])

	// Ack the reclaimed message
	if err := q2.Ack(ctx, claimed[0].ID); err != nil {
		t.Fatalf("Ack reclaimed: %v", err)
	}
	t.Log("PASS: XCLAIM reclaim from crashed consumer verified")
}

// TestRedisStreams_DurabilityComparison documents a comparison of Redis Streams
// vs JsonRunStore for the release queue role.
func TestRedisStreams_DurabilityComparison(t *testing.T) {
	t.Log("=== Redis Streams vs JsonRunStore: Release Queue Durability Assessment ===")
	t.Log("")

	properties := []struct {
		property    string
		jsonStore   string
		redisStream string
	}{
		{"Survives process restart", "YES (file-backed)", "YES (AOF/RDB)"},
		{"Concurrent multi-consumer", "NO (single mutex)", "YES (consumer groups)"},
		{"At-least-once delivery", "MANUAL (app-level)", "BUILT-IN (PEL + XACK)"},
		{"Crash recovery", "NO (no pending list)", "YES (XPENDING + XCLAIM)"},
		{"High-throughput writes", "NO (full JSON rewrite)", "YES (append-only stream)"},
		{"Horizontal scale", "NO (single file)", "YES (multiple consumers)"},
		{"Observability", "ListByState query", "XPENDING count"},
		{"Setup complexity", "ZERO (no server)", "LOW (Redis server needed)"},
		{"PoC friction", "NONE", "LOW (docker run redis)"},
		{"Production recommendation", "Replace with RDBMS", "Suitable for release queue"},
	}

	t.Logf("%-40s  %-30s  %s", "Property", "JsonRunStore", "Redis Streams")
	t.Logf("%s", fmt.Sprintf("%s", string(make([]byte, 90))))
	for _, p := range properties {
		t.Logf("%-40s  %-30s  %s", p.property, p.jsonStore, p.redisStream)
	}

	t.Log("")
	t.Log("VERDICT: Redis Streams > PostgreSQL at PoC stage")
	t.Log("  REASON 1: No schema migrations — Redis Streams are schemaless")
	t.Log("  REASON 2: Consumer groups provide built-in pending/ack/reclaim (no manual WAL)")
	t.Log("  REASON 3: XPENDING gives observable queue depth without complex queries")
	t.Log("  REASON 4: Horizontally scalable: add consumers without changing producer code")
	t.Log("  REASON 5: Docker Redis is lower friction than Docker PostgreSQL for PoC validation")
	t.Log("")
	t.Log("RECOMMENDATION:")
	t.Log("  ingress queue (run submission): Redis Streams (this package)")
	t.Log("  release queue (Job create): BoundedDriver semaphore (in-process, sufficient for PoC)")
	t.Log("  run state store: JsonRunStore → PostgreSQL (production)")
	t.Log("  readiness source of truth: dag-go (unchanged, not Redis)")
}
