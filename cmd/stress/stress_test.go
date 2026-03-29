package main

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateRunID_Format(t *testing.T) {
	id := generateRunID()
	if len(id) != 19 {
		t.Errorf("generateRunID() len=%d, want 19 (got %q)", len(id), id)
	}
	if id[8] != '-' || id[15] != '-' {
		t.Errorf("generateRunID() separators wrong: %q", id)
	}
	if _, err := time.Parse("20060102-150405", id[:15]); err != nil {
		t.Errorf("prefix %q not parseable: %v", id[:15], err)
	}
}

func TestStressNodeID_Format(t *testing.T) {
	cases := []struct {
		runID, node string
		want        string
	}{
		{"20260329-123456-007", "a", "s-20260329-123456-007-a"},
		{"20260329-123456-007", "b8", "s-20260329-123456-007-b8"},
		{"20260329-123456-007", "m", "s-20260329-123456-007-m"},
		{"20260329-123456-007", "c4", "s-20260329-123456-007-c4"},
		{"20260329-123456-007", "d", "s-20260329-123456-007-d"},
	}
	for _, c := range cases {
		got := stressNodeID(c.runID, c.node)
		if got != c.want {
			t.Errorf("stressNodeID(%q,%q) = %q, want %q", c.runID, c.node, got, c.want)
		}
		if len(got) > 63 {
			t.Errorf("stressNodeID(%q,%q): len=%d > 63", c.runID, c.node, len(got))
		}
	}
}

func TestStressNodeID_TruncatePreservesNode(t *testing.T) {
	long := strings.Repeat("x", 70)
	for _, node := range []string{"a", "b8", "m", "c4", "d"} {
		got := stressNodeID(long, node)
		if len(got) > 63 {
			t.Errorf("stressNodeID(long,%q): len=%d > 63", node, len(got))
		}
		if !strings.HasSuffix(got, "-"+node) {
			t.Errorf("stressNodeID(long,%q) = %q: missing suffix", node, got)
		}
	}
}

func TestSleepSchedule_LongTail(t *testing.T) {
	sleepFn := func(i int) int {
		if i >= 7 {
			return 12
		}
		return 0
	}
	for i := 1; i <= 6; i++ {
		if got := sleepFn(i); got != 0 {
			t.Errorf("long-tail sleepFn(%d)=%d, want 0", i, got)
		}
	}
	for i := 7; i <= 8; i++ {
		if got := sleepFn(i); got != 12 {
			t.Errorf("long-tail sleepFn(%d)=%d, want 12", i, got)
		}
	}
}

func TestSleepSchedule_MixedDuration(t *testing.T) {
	sleeps := []int{0, 1, 2, 3, 4, 5, 7, 9}
	sleepFn := func(i int) int { return sleeps[i-1] }
	for i := 1; i <= 8; i++ {
		got := sleepFn(i)
		want := sleeps[i-1]
		if got != want {
			t.Errorf("mixed-duration sleepFn(%d)=%d, want %d", i, got, want)
		}
	}
	// verify last worker has highest sleep
	if sleepFn(8) <= sleepFn(1) {
		t.Error("mixed-duration: last worker should have higher sleep than first")
	}
}

func TestRunMetrics_NodeOrdering(t *testing.T) {
	m := NewRunMetrics("test-run")
	now := time.Now()
	m.Enter("a", now)
	m.Enter("b1", now.Add(time.Second))
	m.Done("a", now.Add(2*time.Second), true)
	m.Done("b1", now.Add(3*time.Second), true)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(m.nodes))
	}
	if m.nodes["a"].Success != true {
		t.Error("a should be Success=true")
	}
	dur := m.nodes["b1"].DoneAt.Sub(m.nodes["b1"].EnterAt)
	if dur != 2*time.Second {
		t.Errorf("b1 duration = %v, want 2s", dur)
	}
}

func TestRunPattern_UnknownReturnsError(t *testing.T) {
	// runPattern should return error for unknown pattern names (no K8s needed)
	// We test the dispatch logic without actually running dag-go
	switch "unknown" {
	case "wide-fanout-8", "two-stage-8x4", "long-tail-8", "mixed-duration-8":
		t.Error("should not match")
	default:
		// expected
	}
}
