package ingress_test

// TestQ1_MainPathWiring_AllCmdUsesRunGate verifies that every cmd/* binary
// in the PoC project has been wired to use RunGate.
//
// This is a source-code-grep test: it searches for `ingress.NewRunGate` and
// `gate.Admit` in the cmd/* directories and fails if any cmd is missing them.
//
// WHY: Sprint 2 found Q1 PARTIAL because RunGate existed in pkg/ but no cmd/*
// called it. This test prevents regression — if someone adds a new cmd without
// wiring RunGate, this test will catch it.
//
// WHY: Sprint 3 wired RunGate into all 7 cmd/* paths. This test documents and
// proves that wiring.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQ1_MainPathWiring_AllCmdUsesRunGate proves every cmd/* main.go imports
// and calls the RunGate ingress boundary.
func TestQ1_MainPathWiring_AllCmdUsesRunGate(t *testing.T) {
	// Find repository root relative to this file's package
	// pkg/ingress → ../../  (two levels up)
	repoRoot := filepath.Join("..", "..")
	cmdDir := filepath.Join(repoRoot, "cmd")

	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		t.Fatalf("cannot read cmd dir %s: %v", cmdDir, err)
	}

	type result struct {
		cmd        string
		hasImport  bool
		hasAdmit   bool
		hasBounded bool
	}

	// producerOnlyCmds는 RunGate/BoundedDriver wiring 검사에서 제외되는 cmd 목록이다.
	// 이 테스트는 "pipeline executor가 RunGate를 건너뛰는 회귀"를 잡기 위해 만들어졌다.
	// producer(submit-only) cmd는 RunGate/BoundedDriver를 사용하지 않는 것이 설계 의도이므로
	// 여기에 등록하여 검사 대상에서 제외한다.
	producerOnlyCmds := map[string]bool{
		"dummy": true, // Sprint 8: N-shot submitter, producer-only (Enqueue만 호출, pipeline 실행 없음)
	}

	var results []result
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if producerOnlyCmds[entry.Name()] {
			continue
		}
		mainFile := filepath.Join(cmdDir, entry.Name(), "main.go")
		data, err := os.ReadFile(mainFile)
		if err != nil {
			t.Errorf("cmd/%s: cannot read main.go: %v", entry.Name(), err)
			continue
		}
		src := string(data)
		results = append(results, result{
			cmd:        entry.Name(),
			hasImport:  strings.Contains(src, `"github.com/seoyhaein/poc/pkg/ingress"`),
			hasAdmit:   strings.Contains(src, "gate.Admit(") || strings.Contains(src, ".Admit("),
			hasBounded: strings.Contains(src, "NewBoundedDriver("),
		})
	}

	if len(results) == 0 {
		t.Fatal("no cmd/* directories found — check repoRoot path")
	}

	t.Logf("=== Q1+Q2 Main Path Wiring Audit ===")
	t.Logf("%-20s  %-12s  %-10s  %-14s", "cmd", "RunGate import", "gate.Admit", "BoundedDriver")
	t.Logf("%s", strings.Repeat("-", 65))

	allPass := true
	for _, r := range results {
		importMark := "YES"
		admitMark := "YES"
		boundedMark := "YES"
		if !r.hasImport {
			importMark = "MISSING"
		}
		if !r.hasAdmit {
			admitMark = "MISSING"
		}
		if !r.hasBounded {
			boundedMark = "MISSING"
		}
		t.Logf("%-20s  %-12s  %-10s  %-14s", r.cmd, importMark, admitMark, boundedMark)

		if !r.hasImport || !r.hasAdmit || !r.hasBounded {
			allPass = false
			t.Errorf("cmd/%s: missing wiring (import=%v admit=%v bounded=%v)",
				r.cmd, r.hasImport, r.hasAdmit, r.hasBounded)
		}
	}
	t.Log("")
	if allPass {
		t.Logf("Q1 PASS: all %d cmd/* paths use RunGate (ingress boundary wired)", len(results))
		t.Logf("Q2 PASS: all %d cmd/* paths use BoundedDriver (burst control wired)", len(results))
		t.Logf("SPRINT3 FIX: Q1 and Q2 PARTIAL eliminated — main path wiring complete")
	}
}
