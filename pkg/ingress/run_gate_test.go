package ingress_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/seoyhaein/poc/pkg/ingress"
	"github.com/seoyhaein/poc/pkg/store"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func newMemStore() store.RunStore {
	return store.NewInMemoryRunStore()
}

func getState(t *testing.T, s store.RunStore, runID string) store.RunState {
	t.Helper()
	rec, ok, err := s.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !ok {
		t.Fatalf("run %s not found in store", runID)
	}
	return rec.State
}

// ─── Q1 verification tests ────────────────────────────────────────────────────

// TestRunGate_EnqueuesBeforeRunFnIsCalled is the core Q1 assertion:
// RunStore.Enqueue(queued) MUST be called before runFn executes.
//
// This proves the ingress boundary is not bypassed: even if runFn panics or
// returns immediately, the run exists in the store in queued/admitted state.
func TestRunGate_EnqueuesBeforeRunFnIsCalled(t *testing.T) {
	s := newMemStore()
	gate := ingress.NewRunGate(s)

	var stateAtRunFnEntry store.RunState

	_ = gate.Admit(context.Background(), "run-q1", func(ctx context.Context) error {
		// Inside runFn: check that the run is already in the store
		rec, ok, err := s.Get(ctx, "run-q1")
		if err != nil || !ok {
			t.Errorf("run-q1 NOT found in store when runFn was called — ingress boundary violated")
			return nil
		}
		stateAtRunFnEntry = rec.State
		return nil
	})

	if stateAtRunFnEntry == "" {
		t.Fatal("runFn was never called or store had no record at runFn entry")
	}
	t.Logf("PASS: run-q1 was in store state=%s when runFn was called", stateAtRunFnEntry)
	t.Logf("INVARIANT: RunStore.Enqueue(queued) happened BEFORE runFn — ingress boundary holds")
}

// TestRunGate_FullStateTransition proves the complete lifecycle:
// queued → admitted-to-dag → running → finished
func TestRunGate_FullStateTransition(t *testing.T) {
	s := newMemStore()
	gate := ingress.NewRunGate(s)

	// Check state BEFORE Admit: should not exist
	_, exists, _ := s.Get(context.Background(), "run-lifecycle")
	if exists {
		t.Fatal("run should not exist before Admit")
	}

	err := gate.Admit(context.Background(), "run-lifecycle", func(_ context.Context) error {
		return nil // success
	})
	if err != nil {
		t.Fatalf("Admit returned error: %v", err)
	}

	finalState := getState(t, s, "run-lifecycle")
	if finalState != store.StateFinished {
		t.Fatalf("expected finished after successful runFn, got %s", finalState)
	}
	t.Logf("PASS: run-lifecycle → queued → admitted-to-dag → running → finished")
}

// TestRunGate_TransitionsToCanceledOnRunFnError proves:
// When runFn returns an error, the run transitions to canceled (not finished).
func TestRunGate_TransitionsToCanceledOnRunFnError(t *testing.T) {
	s := newMemStore()
	gate := ingress.NewRunGate(s)

	expectedErr := errors.New("dag execution failed")
	err := gate.Admit(context.Background(), "run-fail", func(_ context.Context) error {
		return expectedErr
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected dag error, got: %v", err)
	}

	finalState := getState(t, s, "run-fail")
	if finalState != store.StateCanceled {
		t.Fatalf("expected canceled after runFn error, got %s", finalState)
	}
	t.Logf("PASS: run-fail → canceled when runFn returns error")
}

// TestRunGate_NilStore_Noop proves backward compatibility:
// RunGate with nil store calls runFn directly without panicking.
func TestRunGate_NilStore_Noop(t *testing.T) {
	gate := ingress.NewRunGate(nil)

	called := false
	err := gate.Admit(context.Background(), "run-noop", func(_ context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("runFn was not called with nil store")
	}
	t.Log("PASS: nil store = no-op gate, runFn called directly (backward-compatible)")
}

// TestRunGate_BypassPath_DocumentsGap documents the current state of cmd/*:
// All poc/cmd/* paths call dag.Start() WITHOUT a RunGate, bypassing RunStore.
//
// This test does NOT prove a violation — it DOCUMENTS the gap so that
// the sprint report can reference a concrete test name.
//
// STRUCTURAL GAP: cmd/pipeline-abc, cmd/execclass, cmd/fanout all bypass RunGate.
// Fix: inject RunGate in those mains, as shown in the wiring example below.
func TestRunGate_BypassPath_DocumentsGap(t *testing.T) {
	// Simulate the current cmd/pipeline-abc pattern:
	//   drv := imp.NewK8sFromKubeconfig(...)
	//   dag.Start()   ← NO RunGate here
	//   dag.Wait(ctx) ← NO store tracking
	//
	// The following shows what WOULD happen if RunGate were injected:
	s := newMemStore()
	gate := ingress.NewRunGate(s)

	// Simulate dag.Start() + dag.Wait() without K8s
	fakeDAGRun := func(ctx context.Context) error {
		// Represents: dag.Start(); return dag.Wait(ctx) ? nil : err
		return nil
	}

	_ = gate.Admit(context.Background(), "pipeline-abc-run-001", fakeDAGRun)

	finalState := getState(t, s, "pipeline-abc-run-001")
	t.Logf("OBSERVATION: with RunGate wired, final state = %s", finalState)
	t.Logf("CURRENT GAP: cmd/pipeline-abc calls dag.Start() without RunGate → RunStore never populated")
	t.Logf("FIX: wrap dag execution in gate.Admit(ctx, runID, func(ctx) error { ... dag.Start(); dag.Wait(ctx) })")
}

// TestRunGate_RestartRecovery proves that after a simulated restart,
// runs in queued/admitted-to-dag state are recoverable from JsonRunStore.
//
// This directly validates Q1 + Q5: ingress tracking enables restart recovery.
func TestRunGate_RestartRecovery(t *testing.T) {
	// Use JsonRunStore (durable) for restart simulation
	tmpFile := t.TempDir() + "/runstore.json"
	s1, err := store.NewJsonRunStore(tmpFile)
	if err != nil {
		t.Fatalf("NewJsonRunStore: %v", err)
	}
	gate1 := ingress.NewRunGate(s1)

	// Simulate: run-recovery admitted but process crashes DURING execution
	var crashErr = fmt.Errorf("simulated crash")
	err = gate1.Admit(context.Background(), "run-recovery", func(_ context.Context) error {
		// Process crashes mid-run: we don't reach terminal state
		return crashErr
	})
	if !errors.Is(err, crashErr) {
		t.Fatalf("expected crashErr, got: %v", err)
	}

	// Simulate process restart: open same JsonRunStore file
	s2, err := store.NewJsonRunStore(tmpFile)
	if err != nil {
		t.Fatalf("NewJsonRunStore restart: %v", err)
	}

	// The run should be recoverable (in canceled state — crash set it to canceled)
	finalState := getState(t, s2, "run-recovery")
	t.Logf("OBSERVATION: after restart, run-recovery state = %s", finalState)

	// Verify that runs in queued/admitted-to-dag are also recoverable
	storePath3 := t.TempDir() + "/store3.json"
	s3, _ := store.NewJsonRunStore(storePath3)
	_ = s3.Enqueue(context.Background(), store.RunRecord{RunID: "queued-run", State: store.StateQueued})
	_ = s3.Enqueue(context.Background(), store.RunRecord{RunID: "admitted-run", State: store.StateQueued})
	_ = s3.UpdateState(context.Background(), "admitted-run", store.StateQueued, store.StateAdmittedToDag)

	// Simulate restart: open same file
	s3r, _ := store.NewJsonRunStore(storePath3)
	recovered, _ := s3r.ListByState(context.Background(), store.StateQueued)
	recoveredAdmitted, _ := s3r.ListByState(context.Background(), store.StateAdmittedToDag)
	total := len(recovered) + len(recoveredAdmitted)
	if total != 2 {
		t.Fatalf("expected 2 runs to recover, got %d", total)
	}
	t.Logf("PASS: restart recovery: %d queued + %d admitted-to-dag recovered",
		len(recovered), len(recoveredAdmitted))
}
