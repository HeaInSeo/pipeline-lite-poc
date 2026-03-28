// Package session provides DagSession, which tracks DAG node dependencies
// and submits only ready node attempts via NodeSubmitter.
//
// "Ready" = all parent nodes have status nodeSucceeded.
// Nodes that are not yet ready are held in DagSession state and are
// NOT forwarded to NodeSubmitter — meaning they are NOT created as K8s Jobs.
//
// This models hypothesis 2:
//
//	"dag-go actor/session interprets dependency and creates only ready node attempts."
//
// ASSUMPTION: In production, dag-go drives the actual execution loop.
// DagSession is a structural boundary layer that sits between the DAG spec
// and the K8s submission path. It can be used standalone in tests without
// a live K8s cluster.
//
// ASSUMPTION: NodeSubmitter concurrency limit (burst protection) is handled
// by wrapping NodeSubmitter with a NodeAttemptQueue (next diff).
package session

import (
	"context"
	"fmt"
	"sync"
)

// NodeSubmitter is the boundary toward Kueue/K8s.
// In production: wraps spawner.DriverK8s.Start().
// In tests: mock that records Submit calls.
//
// ASSUMPTION: submit is idempotent or callers ensure it is called once per node.
type NodeSubmitter interface {
	Submit(ctx context.Context, nodeID string) error
}

type nodeStatus int

const (
	nodeHeld      nodeStatus = iota // deps not yet satisfied → NOT submitted to Kueue
	nodeSubmitted                   // forwarded to NodeSubmitter (→ Kueue admit queue)
	nodeSucceeded                   // terminal success
	nodeFailed                      // terminal failure
)

type nodeEntry struct {
	id     string
	deps   []string // parent node IDs that must nodeSucceeded before this is ready
	status nodeStatus
}

// DagSession tracks a single DAG run's node dependency state.
//
//	sess := NewDagSession("run-001", mySubmitter)
//	sess.AddNode("A")
//	sess.AddNode("B", "A")   // B depends on A
//	sess.AddNode("C", "B")
//	sess.Start(ctx)          // only A is submitted (B, C are held)
//	sess.NodeDone(ctx, "A", true)  // A succeeded → B becomes ready → submitted
//	sess.NodeDone(ctx, "B", false) // B failed → C stays held (fast-fail)
type DagSession struct {
	mu        sync.Mutex
	runID     string
	nodes     map[string]*nodeEntry
	submitter NodeSubmitter
}

func NewDagSession(runID string, submitter NodeSubmitter) *DagSession {
	return &DagSession{
		runID:     runID,
		nodes:     make(map[string]*nodeEntry),
		submitter: submitter,
	}
}

// AddNode registers a node and its dependencies.
// Must be called before Start().
func (s *DagSession) AddNode(nodeID string, deps ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[nodeID] = &nodeEntry{id: nodeID, deps: deps, status: nodeHeld}
}

// Start submits all initially-ready nodes (nodes with zero unfinished deps).
// This is the first "gate": only nodes with no deps go to Kueue immediately.
func (s *DagSession) Start(ctx context.Context) error {
	s.mu.Lock()
	ready := s.collectReady()
	s.mu.Unlock()
	return s.submitBatch(ctx, ready)
}

// NodeDone marks nodeID as succeeded or failed and submits any newly-ready nodes.
// On failure: dependants stay held (fast-fail). No new submissions.
func (s *DagSession) NodeDone(ctx context.Context, nodeID string, success bool) error {
	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown node: %s", nodeID)
	}
	if success {
		n.status = nodeSucceeded
	} else {
		n.status = nodeFailed
	}
	var ready []*nodeEntry
	if success {
		ready = s.collectReady()
	}
	// if failure: ready is empty → dependants stay held (fast-fail)
	s.mu.Unlock()

	return s.submitBatch(ctx, ready)
}

// collectReady returns nodes that are held and have all deps succeeded.
// Caller must hold s.mu.
func (s *DagSession) collectReady() []*nodeEntry {
	var ready []*nodeEntry
	for _, n := range s.nodes {
		if n.status != nodeHeld {
			continue
		}
		if s.allDepsSucceeded(n) {
			ready = append(ready, n)
		}
	}
	return ready
}

// allDepsSucceeded reports whether all deps of n are nodeSucceeded.
// Caller must hold s.mu.
func (s *DagSession) allDepsSucceeded(n *nodeEntry) bool {
	for _, depID := range n.deps {
		dep, ok := s.nodes[depID]
		if !ok || dep.status != nodeSucceeded {
			return false
		}
	}
	return true
}

// submitBatch calls submitter.Submit for each node and marks it nodeSubmitted.
func (s *DagSession) submitBatch(ctx context.Context, nodes []*nodeEntry) error {
	for _, n := range nodes {
		if err := s.submitter.Submit(ctx, n.id); err != nil {
			return fmt.Errorf("submit %s: %w", n.id, err)
		}
		s.mu.Lock()
		if s.nodes[n.id].status == nodeHeld {
			s.nodes[n.id].status = nodeSubmitted
		}
		s.mu.Unlock()
	}
	return nil
}

// ─── Inspection helpers (used in tests) ──────────────────────────────────────

// HeldNodes returns node IDs in held (not-ready) state.
func (s *DagSession) HeldNodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, n := range s.nodes {
		if n.status == nodeHeld {
			out = append(out, n.id)
		}
	}
	return out
}

// SubmittedNodes returns node IDs that have been forwarded to NodeSubmitter.
func (s *DagSession) SubmittedNodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, n := range s.nodes {
		if n.status == nodeSubmitted || n.status == nodeSucceeded {
			out = append(out, n.id)
		}
	}
	return out
}

// NodeCount returns (held, submitted, succeeded, failed).
func (s *DagSession) NodeCount() (held, submitted, succeeded, failed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.nodes {
		switch n.status {
		case nodeHeld:
			held++
		case nodeSubmitted:
			submitted++
		case nodeSucceeded:
			succeeded++
		case nodeFailed:
			failed++
		}
	}
	return
}
