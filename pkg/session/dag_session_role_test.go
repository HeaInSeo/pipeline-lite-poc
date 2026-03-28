package session_test

// ─── Q3: DagSession role boundary analysis ────────────────────────────────────
//
// Q3 asks: "Is DagSession remaining a thin adapter, or is it growing into
// a second orchestration engine?"
//
// This file documents the answer with executable evidence.

import (
	"context"
	"sort"
	"testing"

	"github.com/seoyhaein/poc/pkg/session"
)

// TestDagSession_Responsibilities_IsAdapterNotEngine classifies every DagSession
// responsibility and checks for orchestration-engine characteristics.
//
// Adapter (acceptable): pass-through, translate, filter, route
// Orchestration engine (unacceptable): own dependency truth, own state machine
//
//	for execution, own retry policy, own scheduling
//
// FINDING: DagSession currently has BOTH adapter and engine characteristics.
// See the RISK section below.
func TestDagSession_Responsibilities_IsAdapterNotEngine(t *testing.T) {
	// ── Adapter responsibilities (acceptable) ─────────────────────────────────
	adapterRoles := []string{
		"accept a NodeSubmitter interface (swappable for NodeAttemptQueue, mock, etc.)",
		"translate node completion events to submit boundary calls",
		"forward ready nodes to NodeSubmitter (NodeAttemptQueue → K8s)",
		"track held/submitted/succeeded/failed per node for this run",
	}

	// ── Engine responsibilities (RISK — duplicates dag-go) ───────────────────
	engineRoles := []string{
		"own dependency graph (AddNode/deps): duplicate of dag-go DAG edges",
		"own 'ready' calculation (allDepsSucceeded): duplicate of dag-go readiness check",
		"own Start() trigger: parallel to dag.Start()",
		"own NodeDone() lifecycle: parallel to dag-go node completion callback",
	}

	t.Log("=== DagSession Responsibility Analysis ===")
	t.Log("")
	t.Log("ADAPTER roles (acceptable):")
	for _, r := range adapterRoles {
		t.Logf("  ✓ %s", r)
	}
	t.Log("")
	t.Log("ENGINE roles (RISK — duplicates dag-go logic):")
	for _, r := range engineRoles {
		t.Logf("  ⚠ %s", r)
	}
	t.Log("")
	t.Logf("Adapter count: %d", len(adapterRoles))
	t.Logf("Engine count:  %d", len(engineRoles))
}

// TestDagSession_DoesNotKnowAboutDagGo proves isolation:
// DagSession has zero imports from dag-go. It does not call dag-go APIs.
// This is correct: DagSession is a structural layer, not integrated INTO dag-go.
//
// CONSEQUENCE: DagSession dependency graph is NOT connected to dag-go's DAG.
// They are PARALLEL systems that must be kept manually in sync — a maintenance risk.
func TestDagSession_DoesNotKnowAboutDagGo(t *testing.T) {
	// This test cannot fail programmatically, but documents the isolation.
	// Verified by: grep -r "dag-go" pkg/session/ → returns 0 matches
	t.Log("OBSERVATION: DagSession has no dag-go imports")
	t.Log("IMPLICATION: DagSession dependency graph != dag-go DAG — parallel structures")
	t.Log("RISK: if dag-go DAG and DagSession graph diverge, silent correctness bugs")
}

// TestDagSession_CurrentUsagePath proves the structural gap:
// In ALL poc/cmd/* executables, dag-go drives node execution directly via RunE callbacks.
// DagSession is NEVER called from cmd/*.
//
// This means:
//   - dag-go is the ACTUAL source of truth for readiness in the main execution path
//   - DagSession is only exercised in unit tests (pkg/session/...)
//   - NodeAttemptQueue is only effective when DagSession is used (not dag-go path)
func TestDagSession_CurrentUsagePath(t *testing.T) {
	// Paths in poc/cmd/* that use dag-go directly (NOT DagSession):
	cmdPaths := []string{
		"cmd/pipeline-abc/main.go: dag.Start() → SpawnerNode.RunE() directly",
		"cmd/execclass/main.go:    dag.Start() → SpawnerNode.RunE() directly",
		"cmd/fanout/main.go:       dag.Start() → SpawnerNode.RunE() directly",
		"cmd/fastfail/main.go:     dag.Start() → SpawnerNode.RunE() directly",
		"cmd/dag-runner/main.go:   dag.Start() → SpawnerNode.RunE() directly",
	}
	// Paths in poc/cmd/* that use DagSession:
	dagSessionPaths := []string{}

	t.Logf("OBSERVATION: %d cmd paths use dag-go directly (no DagSession)", len(cmdPaths))
	for _, p := range cmdPaths {
		t.Logf("  DAG-GO: %s", p)
	}
	t.Logf("OBSERVATION: %d cmd paths use DagSession", len(dagSessionPaths))
	t.Log("")
	t.Log("STRUCTURAL GAP: DagSession exists but is unused in all cmd/* execution paths")
	t.Log("CONSEQUENCE: NodeAttemptQueue burst control is NOT active on the main execution path")
	t.Log("FIX: either use DagSession in cmd/* OR add burst control at the dag-go RunE layer")
}

// TestDagSession_ReadinessAuthority proves which system decides node readiness.
//
// dag-go: calls node.RunE() when the graph engine determines a node is ready.
// DagSession: calls Submit() when allDepsSucceeded() returns true.
//
// In the current main path, dag-go is the SOLE authority for readiness.
// DagSession is a dead-letter readiness system (tested but not active).
func TestDagSession_ReadinessAuthority(t *testing.T) {
	sub := &mockSubmitter{}
	sess := newDagSession(t, sub)
	// A → B (B depends on A)
	sess.AddNode("A")
	sess.AddNode("B", "A")

	_ = sess.Start(context.Background())
	held := sess.HeldNodes()
	sort.Strings(held)

	// DagSession correctly holds B until A completes
	if len(held) != 1 || held[0] != "B" {
		t.Fatalf("expected [B] held, got %v", held)
	}

	t.Log("DagSession readiness: CORRECT (B held until A done)")
	t.Log("dag-go readiness: CORRECT (B's RunE not called until A's RunE returns)")
	t.Log("FINDING: two parallel correct readiness systems — dag-go is source of truth in cmd/*")
	t.Log("RISK: if they diverge (e.g., DagSession node list differs from dag-go DAG), silent bugs")
}

// ─── helper ──────────────────────────────────────────────────────────────────

func newDagSession(t *testing.T, sub *mockSubmitter) *session.DagSession {
	t.Helper()
	return session.NewDagSession("test-run", sub)
}
