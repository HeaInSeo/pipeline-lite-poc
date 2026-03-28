package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/seoyhaein/poc/pkg/store"
)

// ─── state machine ────────────────────────────────────────────────────────────

// TestValidateTransition_Valid proves that all designed forward transitions
// are accepted by the state machine.
func TestValidateTransition_Valid(t *testing.T) {
	cases := [][2]store.RunState{
		{store.StateQueued, store.StateAdmittedToDag},
		{store.StateQueued, store.StateHeld},
		{store.StateQueued, store.StateCanceled},
		{store.StateHeld, store.StateResumed},
		{store.StateHeld, store.StateCanceled},
		{store.StateResumed, store.StateAdmittedToDag},
		{store.StateAdmittedToDag, store.StateRunning},
		{store.StateAdmittedToDag, store.StateCanceled},
		{store.StateRunning, store.StateFinished},
		{store.StateRunning, store.StateCanceled},
	}
	for _, c := range cases {
		if err := store.ValidateTransition(c[0], c[1]); err != nil {
			t.Errorf("expected valid %s→%s: %v", c[0], c[1], err)
		}
	}
}

// TestValidateTransition_Invalid proves that reverse transitions and
// terminal→any are rejected, preventing impossible state mutations.
func TestValidateTransition_Invalid(t *testing.T) {
	cases := [][2]store.RunState{
		{store.StateFinished, store.StateQueued},       // terminal → any
		{store.StateCanceled, store.StateRunning},      // terminal → any
		{store.StateRunning, store.StateQueued},        // backward
		{store.StateAdmittedToDag, store.StateHeld},    // skip-back
		{store.StateRunning, store.StateAdmittedToDag}, // backward
	}
	for _, c := range cases {
		if err := store.ValidateTransition(c[0], c[1]); err == nil {
			t.Errorf("expected invalid %s→%s to be rejected", c[0], c[1])
		}
	}
}

// ─── InMemoryRunStore ─────────────────────────────────────────────────────────

// TestMemoryStore_DoesNotSurviveReset proves hypothesis:
// "memory store loses queued runs when a new instance is created (simulating restart)."
// This is the key difference from JsonRunStore.
func TestMemoryStore_DoesNotSurviveReset(t *testing.T) {
	ctx := context.Background()

	s1 := store.NewInMemoryRunStore()
	if err := s1.Enqueue(ctx, store.RunRecord{RunID: "run-1", State: store.StateQueued}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := s1.Enqueue(ctx, store.RunRecord{RunID: "run-2", State: store.StateQueued}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulate restart: create a NEW instance. No shared state.
	s2 := store.NewInMemoryRunStore()
	recs, err := s2.ListByState(ctx, store.StateQueued)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("InMemoryRunStore: expected 0 records after reset, got %d", len(recs))
	}
	t.Logf("OBSERVATION: InMemoryRunStore lost %d queued runs on reset", 2)
}

// TestMemoryStore_StateTransition proves that UpdateState enforces
// the state machine policy (rejects invalid transitions).
func TestMemoryStore_StateTransition(t *testing.T) {
	ctx := context.Background()
	s := store.NewInMemoryRunStore()

	_ = s.Enqueue(ctx, store.RunRecord{RunID: "run-1", State: store.StateQueued})

	// valid: queued → admitted-to-dag
	if err := s.UpdateState(ctx, "run-1", store.StateQueued, store.StateAdmittedToDag); err != nil {
		t.Fatalf("valid transition rejected: %v", err)
	}

	// invalid: admitted-to-dag → queued (backward)
	err := s.UpdateState(ctx, "run-1", store.StateAdmittedToDag, store.StateQueued)
	if err == nil {
		t.Fatal("invalid backward transition was accepted")
	}
	t.Logf("PASS: invalid backward transition rejected: %v", err)

	// invalid: wrong 'from' state
	err = s.UpdateState(ctx, "run-1", store.StateQueued, store.StateRunning)
	if err == nil {
		t.Fatal("UpdateState with wrong 'from' state was accepted")
	}
	t.Logf("PASS: wrong 'from' state rejected: %v", err)
}

// ─── JsonRunStore (durable-lite) ──────────────────────────────────────────────

// TestJsonStore_RecoveryAfterRestart proves hypothesis:
// "durable store recovers queued runs after restart; memory store does not."
// The same JSON file is opened twice (simulating process restart).
func TestJsonStore_RecoveryAfterRestart(t *testing.T) {
	ctx := context.Background()

	f, err := os.CreateTemp(t.TempDir(), "runstore-*.json")
	if err != nil {
		t.Fatalf("tmpfile: %v", err)
	}
	path := f.Name()
	f.Close()

	// "First process": enqueue two runs, then exit.
	s1, err := store.NewJsonRunStore(path)
	if err != nil {
		t.Fatalf("NewJsonRunStore: %v", err)
	}
	if err := s1.Enqueue(ctx, store.RunRecord{RunID: "run-A", State: store.StateQueued}); err != nil {
		t.Fatalf("Enqueue A: %v", err)
	}
	if err := s1.Enqueue(ctx, store.RunRecord{RunID: "run-B", State: store.StateQueued}); err != nil {
		t.Fatalf("Enqueue B: %v", err)
	}
	_ = s1.UpdateState(ctx, "run-A", store.StateQueued, store.StateAdmittedToDag)
	// run-B stays queued

	// "Restart": open the same file with a new instance.
	s2, err := store.NewJsonRunStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}

	queued, _ := s2.ListByState(ctx, store.StateQueued)
	admitted, _ := s2.ListByState(ctx, store.StateAdmittedToDag)

	if len(queued) != 1 {
		t.Fatalf("expected 1 queued run after restart, got %d", len(queued))
	}
	if len(admitted) != 1 {
		t.Fatalf("expected 1 admitted run after restart, got %d", len(admitted))
	}
	t.Logf("OBSERVATION: JsonRunStore recovered %d queued + %d admitted runs after restart",
		len(queued), len(admitted))
}

// TestJsonStore_NoDuplicateEnqueue proves ErrAlreadyExists is returned
// when the same RunID is submitted twice.
func TestJsonStore_NoDuplicateEnqueue(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/store.json"

	s, _ := store.NewJsonRunStore(path)
	_ = s.Enqueue(ctx, store.RunRecord{RunID: "dup", State: store.StateQueued})

	err := s.Enqueue(ctx, store.RunRecord{RunID: "dup", State: store.StateQueued})
	if err == nil {
		t.Fatal("expected ErrAlreadyExists for duplicate RunID")
	}
	t.Logf("PASS: duplicate enqueue rejected: %v", err)
}
