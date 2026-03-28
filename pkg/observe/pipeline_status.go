// Package observe provides the observable signals for each "stopped" state in
// a DAG pipeline run. This answers Q4:
//
//	"When an operator sees a run that is not progressing, can they identify
//	 the exact cause at a single layer?"
//
// There are 6 distinct stop causes, each with a different observable signal:
//
//  1. ingress_queue       — RunStore state = queued or held
//  2. release_queue       — BoundedDriver.Stats().Inflight == MaxConcurrent (Sprint 3: main path)
//     or NodeAttemptQueue.Stats().blocked > 0 (experimental DagSession path)
//  3. kueue_pending       — Workload.QuotaReserved=false (K8sObserver.ObserveWorkload)
//  4. scheduler_unsched   — Pod.PodScheduled=false (K8sObserver.ObservePod)
//  5. running             — RunStore state = running
//  6. finished            — RunStore state = finished or canceled
//
// Sprint 3 update (Q2 fix): cause 2 primary signal is now BoundedDriver (wired in all cmd/*).
// NodeAttemptQueue remains observable in the experimental DagSession path.
//
// Causes 1, 2, 5, 6 are observable without K8s (RunStore + BoundedDriver/NodeAttemptQueue stats).
// Causes 3, 4 require a live K8s + Kueue cluster (K8sObserver — see integration tests).
package observe

import (
	"context"
	"fmt"

	"github.com/seoyhaein/poc/pkg/session"
	"github.com/seoyhaein/poc/pkg/store"
)

// StopCause classifies why a DAG run appears "stopped" or "not running".
// Each cause corresponds to a distinct, observable signal at a different layer.
type StopCause string

const (
	// CauseIngressQueue: run is waiting in RunStore (queued or held).
	// Observable: RunStore.ListByState(StateQueued) or ListByState(StateHeld)
	// Layer: ingress boundary (before dag-go)
	CauseIngressQueue StopCause = "ingress_queue"

	// CauseReleaseQueue: a node is waiting for a NodeAttemptQueue semaphore slot.
	// Observable: NodeAttemptQueue.Stats().blocked > 0
	// Layer: release control boundary (between dag-go and Kueue)
	CauseReleaseQueue StopCause = "release_queue"

	// CauseKueuePending: Kueue has not admitted the workload (quota exhausted).
	// Observable: K8sObserver.ObserveWorkload().QuotaReserved = false
	// Layer: Kueue admission (cluster quota layer)
	// Note: pod does NOT exist in this state. Job is suspended.
	CauseKueuePending StopCause = "kueue_pending"

	// CauseSchedulerUnschedulable: kube-scheduler cannot place the pod on any node.
	// Observable: K8sObserver.ObservePod().Scheduled = false
	// Layer: kube-scheduler (node placement layer)
	// Note: Kueue admitted the workload; pod exists but is Pending.
	CauseSchedulerUnschedulable StopCause = "scheduler_unschedulable"

	// CauseRunning: run is actively executing in K8s.
	// Observable: RunStore.ListByState(StateRunning)
	// Layer: execution layer (K8s Job running)
	CauseRunning StopCause = "running"

	// CauseFinished: run completed (succeeded or failed).
	// Observable: RunStore.ListByState(StateFinished) or ListByState(StateCanceled)
	// Layer: completion layer (terminal state)
	CauseFinished StopCause = "finished"
)

// ObservableSignal describes how to observe each StopCause.
type ObservableSignal struct {
	// Cause is the StopCause this signal corresponds to.
	Cause StopCause
	// Signal is the human-readable observation method.
	Signal string
	// Layer is the architectural layer where this is observable.
	Layer string
	// RequiresK8s indicates whether this signal requires a live K8s/Kueue cluster.
	RequiresK8s bool
}

// AllSignals is the complete list of observable signals for each stop cause.
// This is the answer to Q4: "can an operator identify the cause at one layer?"
var AllSignals = []ObservableSignal{
	{
		Cause:       CauseIngressQueue,
		Signal:      "RunStore.ListByState(queued) or ListByState(held)",
		Layer:       "ingress boundary",
		RequiresK8s: false,
	},
	{
		Cause:       CauseReleaseQueue,
		Signal:      "BoundedDriver.Stats().Inflight == MaxConcurrent (cmd/* main path, Sprint 3)",
		Layer:       "release control boundary (BoundedDriver wraps DriverK8s)",
		RequiresK8s: false,
	},
	{
		Cause:       CauseKueuePending,
		Signal:      "K8sObserver.ObserveWorkload(jobName).QuotaReserved == false",
		Layer:       "Kueue admission (cluster quota)",
		RequiresK8s: true,
	},
	{
		Cause:       CauseSchedulerUnschedulable,
		Signal:      "K8sObserver.ObservePod(jobName).Scheduled == false",
		Layer:       "kube-scheduler (node placement)",
		RequiresK8s: true,
	},
	{
		Cause:       CauseRunning,
		Signal:      "RunStore.ListByState(running)",
		Layer:       "execution layer",
		RequiresK8s: false,
	},
	{
		Cause:       CauseFinished,
		Signal:      "RunStore.ListByState(finished) or ListByState(canceled)",
		Layer:       "completion layer",
		RequiresK8s: false,
	},
}

// PipelineStatusReader reads pipeline status from non-K8s observable sources.
// It covers causes 1, 2, 5, 6 without requiring a live cluster.
//
// Causes 3 (Kueue pending) and 4 (scheduler unschedulable) require
// K8sObserver (spawner/cmd/imp/k8s_observer.go) — tested in integration tests.
type PipelineStatusReader struct {
	store store.RunStore
	queue *session.NodeAttemptQueue // may be nil
}

// NewPipelineStatusReader creates a reader for local observable signals.
func NewPipelineStatusReader(s store.RunStore, q *session.NodeAttemptQueue) *PipelineStatusReader {
	return &PipelineStatusReader{store: s, queue: q}
}

// Describe returns a human-readable status string for the given runID.
// Returns an error if the run is not found.
func (r *PipelineStatusReader) Describe(ctx context.Context, runID string) (string, error) {
	if r.store == nil {
		return "", fmt.Errorf("no store configured")
	}
	rec, ok, err := r.store.Get(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("store.Get %s: %w", runID, err)
	}
	if !ok {
		return "", fmt.Errorf("run %s not found", runID)
	}

	cause := stateToStopCause(rec.State)
	return fmt.Sprintf("[observe] run=%s state=%s cause=%s signal=%s",
		runID, rec.State, cause, signalForCause(cause)), nil
}

// ReleaseQueueBlocked reports whether the NodeAttemptQueue has blocked submitters.
// This is observable at the release queue boundary (Q4 cause 2).
func (r *PipelineStatusReader) ReleaseQueueBlocked() (blocked int64) {
	if r.queue == nil {
		return 0
	}
	_, b, _ := r.queue.Stats()
	return b
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func stateToStopCause(state store.RunState) StopCause {
	switch state {
	case store.StateQueued, store.StateHeld, store.StateResumed:
		return CauseIngressQueue
	case store.StateAdmittedToDag, store.StateRunning:
		return CauseRunning
	case store.StateFinished, store.StateCanceled:
		return CauseFinished
	default:
		return StopCause("unknown:" + string(state))
	}
}

func signalForCause(cause StopCause) string {
	for _, s := range AllSignals {
		if s.Cause == cause {
			return s.Signal
		}
	}
	return "unknown signal"
}
