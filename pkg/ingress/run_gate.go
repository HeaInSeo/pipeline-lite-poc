// Package ingress provides the RunGate: the boundary between user submission
// and dag-go execution. Every DAG run must pass through RunGate.Admit() before
// dag.Start() is called.
//
// Purpose: prove hypothesis 1 —
//
//	"user submission is received by the Run queue at the service front end"
//
// RunGate connects RunStore to the actual dag-go execution path.
// Without it, cmd/* paths call dag.Start() directly, bypassing RunStore entirely.
//
// State transitions managed by RunGate:
//
//	queued → admitted-to-dag → running → finished
//	                                   → canceled (on error)
//
// ASSUMPTION: production wraps this gate with a rate-limiter or
// admission policy before calling Admit().
package ingress

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/seoyhaein/poc/pkg/store"
)

// RunGate is the ingress boundary for DAG runs.
// It wraps a RunStore and ensures every run is recorded before execution begins.
//
// If Store is nil, Admit() calls runFn directly without any store tracking
// (backward-compatible with existing cmd/* paths that have no store).
type RunGate struct {
	Store store.RunStore
}

// NewRunGate creates a RunGate backed by the given RunStore.
// Pass nil to create a no-op gate (backward-compatible).
func NewRunGate(s store.RunStore) *RunGate {
	return &RunGate{Store: s}
}

// Admit records the run in RunStore, then calls runFn.
// The run state advances through: queued → admitted-to-dag → running → finished/canceled.
//
// If store is nil: calls runFn directly (no-op gate).
//
// Callers must supply a runID that is stable and unique per DAG run.
// runFn receives the same ctx and should call dag.Start() + dag.Wait().
//
// INVARIANT: RunStore.Enqueue(queued) is called BEFORE runFn is invoked.
// This is the central structural assertion for Q1 (ingress boundary).
func (g *RunGate) Admit(ctx context.Context, runID string, runFn func(context.Context) error) error {
	if g.Store == nil {
		// No-op gate: backward-compatible with paths that have no RunStore.
		log.Printf("[ingress] WARN: RunGate has no store — run %s admitted without tracking", runID)
		return runFn(ctx)
	}

	// 1. Enqueue as queued — MUST happen before runFn.
	// This is the structural invariant: the run exists in the store before
	// any K8s API call is made.
	enqErr := g.Store.Enqueue(ctx, store.RunRecord{
		RunID: runID,
		State: store.StateQueued,
	})
	if enqErr != nil && !errors.Is(enqErr, store.ErrAlreadyExists) {
		return fmt.Errorf("[ingress] enqueue %s: %w", runID, enqErr)
	}
	log.Printf("[ingress] run %s → queued", runID)

	// 2. Transition to admitted-to-dag: dag-go is about to take control.
	if err := g.Store.UpdateState(ctx, runID, store.StateQueued, store.StateAdmittedToDag); err != nil {
		// If state mismatch (e.g., re-submission): log but continue.
		log.Printf("[ingress] warn: UpdateState admitted: %v", err)
	} else {
		log.Printf("[ingress] run %s → admitted-to-dag", runID)
	}

	// 3. Transition to running: dag.Start() is being called.
	if err := g.Store.UpdateState(ctx, runID, store.StateAdmittedToDag, store.StateRunning); err != nil {
		log.Printf("[ingress] warn: UpdateState running: %v", err)
	} else {
		log.Printf("[ingress] run %s → running", runID)
	}

	// 4. Execute: this is where dag.Start() + dag.Wait() live.
	runErr := runFn(ctx)

	// 5. Terminal transition.
	if runErr == nil {
		if err := g.Store.UpdateState(ctx, runID, store.StateRunning, store.StateFinished); err != nil {
			log.Printf("[ingress] warn: UpdateState finished: %v", err)
		} else {
			log.Printf("[ingress] run %s → finished", runID)
		}
	} else {
		if err := g.Store.UpdateState(ctx, runID, store.StateRunning, store.StateCanceled); err != nil {
			log.Printf("[ingress] warn: UpdateState canceled: %v", err)
		} else {
			log.Printf("[ingress] run %s → canceled: %v", runID, runErr)
		}
	}

	return runErr
}
