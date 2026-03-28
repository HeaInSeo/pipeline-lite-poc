// Package store provides RunStore: a boundary between the service front-end
// and dag-go/spawner. Queued runs live here until admitted to a DagSession.
//
// Hypothesis 1: "user submission is received by the Run queue / session store
// at the service front end."
package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RunState is the lifecycle state of a submitted DAG run.
type RunState string

const (
	// StateQueued: received from user, waiting for admission to dag-go.
	StateQueued RunState = "queued"
	// StateHeld: explicitly paused (rate-limit, policy, manual hold).
	StateHeld RunState = "held"
	// StateResumed: hold lifted, waiting for re-admission.
	StateResumed RunState = "resumed"
	// StateAdmittedToDag: handed to a DagSession; dag-go is now driving.
	StateAdmittedToDag RunState = "admitted-to-dag"
	// StateRunning: at least one node attempt is active in Kueue/K8s.
	StateRunning RunState = "running"
	// StateFinished: all nodes completed (terminal).
	StateFinished RunState = "finished"
	// StateCanceled: run was canceled (terminal).
	StateCanceled RunState = "canceled"
)

// validTransitions defines allowed state machine edges.
// Reverse transitions and terminal→any are rejected.
var validTransitions = map[RunState][]RunState{
	StateQueued:        {StateHeld, StateAdmittedToDag, StateCanceled},
	StateHeld:          {StateResumed, StateCanceled},
	StateResumed:       {StateAdmittedToDag, StateCanceled},
	StateAdmittedToDag: {StateRunning, StateCanceled},
	StateRunning:       {StateFinished, StateCanceled},
	StateFinished:      {}, // terminal
	StateCanceled:      {}, // terminal
}

var (
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrNotFound          = errors.New("run not found")
	ErrAlreadyExists     = errors.New("run already exists")
)

// RunRecord is the persisted representation of a submitted DAG run.
type RunRecord struct {
	RunID     string
	State     RunState
	Payload   []byte // serialized DAG spec or metadata (opaque)
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RunStore is the contract for the front-end run store.
// Two implementations are provided:
//   - InMemoryRunStore: fast, loses all data on restart
//   - JsonRunStore (durable-lite): file-backed, survives restart
//
// ASSUMPTION: production will use PostgreSQL/Redis. The interface is identical.
type RunStore interface {
	Enqueue(ctx context.Context, rec RunRecord) error
	Get(ctx context.Context, runID string) (RunRecord, bool, error)
	UpdateState(ctx context.Context, runID string, from, to RunState) error
	ListByState(ctx context.Context, state RunState) ([]RunRecord, error)
	Delete(ctx context.Context, runID string) error
}

// ValidateTransition returns ErrInvalidTransition if the from→to edge is not
// in the state machine. Callers should use this before calling UpdateState.
func ValidateTransition(from, to RunState) error {
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
}

// IsTerminal reports whether state is a terminal state (no further transitions).
func IsTerminal(s RunState) bool {
	return s == StateFinished || s == StateCanceled
}

// TerminalStates returns all terminal RunState values.
func TerminalStates() []RunState {
	return []RunState{StateFinished, StateCanceled}
}

var _ = time.Now // ensure time import is used
