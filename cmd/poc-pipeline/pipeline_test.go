package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/seoyhaein/spawner/pkg/api"
)

func TestGenerateRunID_Format(t *testing.T) {
	id := generateRunID()
	if len(id) != 15 {
		t.Errorf("generateRunID() len = %d, want 15 (got %q)", len(id), id)
	}
	if id[8] != '-' {
		t.Errorf("generateRunID() expected '-' at index 8, got %q (id=%s)", id[8], id)
	}
	if _, err := time.Parse("20060102-150405", id); err != nil {
		t.Errorf("generateRunID() = %q does not parse as YYYYMMDD-HHMMSS: %v", id, err)
	}
}

func TestPathBase(t *testing.T) {
	got := pathBase("testrun")
	want := "/data/poc-pipeline/testrun"
	if got != want {
		t.Errorf("pathBase(testrun) = %q, want %q", got, want)
	}
}

func TestNodeRunID_Format(t *testing.T) {
	cases := []struct {
		runID, node string
		want        string
	}{
		{"20260328-123456", "a", "poc-20260328-123456-a"},
		{"20260328-123456", "b1", "poc-20260328-123456-b1"},
		{"20260328-123456", "b2", "poc-20260328-123456-b2"},
		{"20260328-123456", "b3", "poc-20260328-123456-b3"},
		{"20260328-123456", "c", "poc-20260328-123456-c"},
	}
	for _, c := range cases {
		got := nodeRunID(c.runID, c.node)
		if got != c.want {
			t.Errorf("nodeRunID(%q,%q) = %q, want %q", c.runID, c.node, got, c.want)
		}
		if len(got) > 63 {
			t.Errorf("nodeRunID(%q,%q): len=%d exceeds K8s Job name limit of 63", c.runID, c.node, len(got))
		}
	}
}

func TestNodeRunID_Truncates(t *testing.T) {
	long := strings.Repeat("x", 70)
	got := nodeRunID(long, "a")
	if len(got) > 63 {
		t.Errorf("nodeRunID with 70-char runID: len=%d > 63 (got %q)", len(got), got)
	}
}

func TestSpecA_Paths(t *testing.T) {
	mount := api.Mount{Source: "poc-shared-pvc", Target: "/data", ReadOnly: false}
	pBase := "/data/poc-pipeline/testrun"
	spec := specA("testrun", pBase, mount)

	cmd := strings.Join(spec.Command, " ")
	checks := []string{
		"a-output",
		"b-output/shard-0",
		"b-output/shard-1",
		"b-output/shard-2",
		"c-output",
		"seed.txt",
		"runId=testrun",
		"mkdir -p",
	}
	for _, want := range checks {
		if !strings.Contains(cmd, want) {
			t.Errorf("specA command missing %q", want)
		}
	}
	wantRunID := "poc-testrun-a"
	if spec.RunID != wantRunID {
		t.Errorf("specA RunID = %q, want %q", spec.RunID, wantRunID)
	}
	if spec.ImageRef != "busybox:1.36" {
		t.Errorf("specA ImageRef = %q, want busybox:1.36", spec.ImageRef)
	}
}

func TestSpecWorker_Shards(t *testing.T) {
	mount := api.Mount{Source: "poc-shared-pvc", Target: "/data", ReadOnly: false}
	pBase := "/data/poc-pipeline/testrun"

	cases := []struct{ n, shard int }{{1, 0}, {2, 1}, {3, 2}}
	for _, c := range cases {
		spec := specWorker(c.n, "testrun", pBase, mount)
		cmd := strings.Join(spec.Command, " ")

		wantRunID := fmt.Sprintf("poc-testrun-b%d", c.n)
		if spec.RunID != wantRunID {
			t.Errorf("specWorker(%d) RunID = %q, want %q", c.n, spec.RunID, wantRunID)
		}
		wantShard := fmt.Sprintf("shard-%d", c.shard)
		if !strings.Contains(cmd, wantShard) {
			t.Errorf("specWorker(%d) command missing %q", c.n, wantShard)
		}
		if !strings.Contains(cmd, "result.txt") {
			t.Errorf("specWorker(%d) command missing result.txt", c.n)
		}
		if !strings.Contains(cmd, "seed.txt") {
			t.Errorf("specWorker(%d) command missing seed.txt", c.n)
		}
		if !strings.Contains(cmd, "set -e") {
			t.Errorf("specWorker(%d) command missing set -e", c.n)
		}
	}
}

func TestSpecCollect_Paths(t *testing.T) {
	mount := api.Mount{Source: "poc-shared-pvc", Target: "/data", ReadOnly: false}
	pBase := "/data/poc-pipeline/testrun"
	spec := specCollect("testrun", pBase, mount)
	cmd := strings.Join(spec.Command, " ")

	checks := []string{
		"shard-0/result.txt",
		"shard-1/result.txt",
		"shard-2/result.txt",
		"report.txt",
		"exit 1",
		"runId=testrun",
		"=== poc-pipeline report ===",
	}
	for _, want := range checks {
		if !strings.Contains(cmd, want) {
			t.Errorf("specCollect command missing %q", want)
		}
	}
	if spec.RunID != "poc-testrun-c" {
		t.Errorf("specCollect RunID = %q, want %q", spec.RunID, "poc-testrun-c")
	}
}
