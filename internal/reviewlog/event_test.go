package reviewlog

import (
	"strings"
	"testing"
)

func TestEventIsCorrelatedAndStaysOnOneLine(t *testing.T) {
	line := (Event{Name: "completed", Component: "worker", Kind: "reaction", ReviewID: "rvw-1", RunID: "run-1", ReactionID: "rxn-1", WorkerID: "wrk-1", Repo: "a/b", PRNumber: 7, Status: "failed", Error: "timed out\ninjected status=succeeded"}).String()
	for _, want := range []string{`event="completed"`, `component="worker"`, `review_id="rvw-1"`, `run_id="run-1"`, `reaction_id="rxn-1"`, `worker_id="wrk-1"`, `repo="a/b"`, `pr_number=7`, `status="failed"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing correlation field %s: %s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("log event split across lines: %q", line)
	}
}
