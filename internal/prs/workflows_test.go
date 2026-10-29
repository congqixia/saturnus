package prs

import (
	"context"
	"testing"

	"saturnus/internal/store"
)

func TestWorkflowIdentityPreventsSameNamedJobFromHidingFailure(t *testing.T) {
	g := githubStub(t, map[string]string{
		"repos/a/b/pulls/1": prJSON("head", ""),
		"repos/a/b/actions/runs?head_sha=head&per_page=100":         `[{"total_count":2,"workflow_runs":[{"id":20,"workflow_id":10,"check_suite_id":100,"run_attempt":1,"name":"Linux","event":"pull_request","head_sha":"head","status":"completed","conclusion":"failure"},{"id":21,"workflow_id":11,"check_suite_id":200,"run_attempt":2,"name":"Mac","event":"pull_request","head_sha":"head","status":"completed","conclusion":"success"}]}]`,
		"repos/a/b/commits/head/check-runs?filter=all&per_page=100": `[{"total_count":2,"check_runs":[{"id":300,"name":"test","head_sha":"head","status":"completed","conclusion":"failure","app":{"id":1,"slug":"github-actions"},"check_suite":{"id":100}},{"id":400,"name":"test","head_sha":"head","status":"completed","conclusion":"success","app":{"id":1,"slug":"github-actions"},"check_suite":{"id":200}}]}]`,
		"repos/a/b/commits/head/statuses?per_page=100":              `[[]]`,
	})
	_, checks, err := g.Fetch(context.Background(), "a/b", 1)
	if err != nil || len(checks) != 2 {
		t.Fatalf("workflow jobs collapsed: %+v %v", checks, err)
	}
	state, counts := (Policy{}).Evaluate("a/b", checks)
	if state != "FAIL" || counts.Fail != 1 || counts.Pass != 1 || checks[0].Workflow == checks[1].Workflow {
		t.Fatalf("one workflow hid another: %s %+v %+v", state, counts, checks)
	}
}

func TestWorkflowWithoutJobsStillReportsStartupFailure(t *testing.T) {
	g := githubStub(t, map[string]string{
		"repos/a/b/pulls/1": prJSON("head", ""),
		"repos/a/b/actions/runs?head_sha=head&per_page=100":         `[{"total_count":1,"workflow_runs":[{"id":20,"workflow_id":10,"run_attempt":1,"name":"Workflow config","event":"push","head_sha":"head","status":"completed","conclusion":"failure"}]}]`,
		"repos/a/b/commits/head/check-runs?filter=all&per_page=100": `[{"total_count":0,"check_runs":[]}]`,
		"repos/a/b/commits/head/statuses?per_page=100":              `[[]]`,
	})
	_, checks, err := g.Fetch(context.Background(), "a/b", 1)
	if err != nil || len(checks) != 1 || checks[0].Source != "workflow_run" || checks[0].Bucket != "fail" {
		t.Fatalf("startup failure was missed: %+v %v", checks, err)
	}
	if state, _ := (Policy{Global: []string{"Workflow config"}}).Evaluate("a/b", checks); state != "BYPASS" {
		t.Fatal(state)
	}
}

func TestNewWorkflowRunReplacesRemovedFailedJob(t *testing.T) {
	g := githubStub(t, map[string]string{
		"repos/a/b/pulls/1": prJSON("head", ""),
		"repos/a/b/actions/runs?head_sha=head&per_page=100":         `[{"total_count":2,"workflow_runs":[{"id":20,"workflow_id":10,"check_suite_id":100,"name":"Tests","event":"push","head_sha":"head","status":"completed","conclusion":"failure"},{"id":21,"workflow_id":10,"check_suite_id":200,"name":"Tests","event":"push","head_sha":"head","status":"completed","conclusion":"success"}]}]`,
		"repos/a/b/commits/head/check-runs?filter=all&per_page=100": `[{"total_count":2,"check_runs":[{"id":300,"name":"removed matrix job","head_sha":"head","status":"completed","conclusion":"failure","app":{"id":1,"slug":"github-actions"},"check_suite":{"id":100}},{"id":400,"name":"current job","head_sha":"head","status":"completed","conclusion":"success","app":{"id":1,"slug":"github-actions"},"check_suite":{"id":200}}]}]`,
		"repos/a/b/commits/head/statuses?per_page=100":              `[[]]`,
	})
	_, checks, err := g.Fetch(context.Background(), "a/b", 1)
	if err != nil || len(checks) != 1 || checks[0].Name != "current job" {
		t.Fatalf("obsolete workflow job survived: %+v %v", checks, err)
	}
	if state, _ := (Policy{}).Evaluate("a/b", checks); state != "PASS" {
		t.Fatal(state)
	}
}

func TestOptionalFailedJobDoesNotReappearAsParentWorkflowFailure(t *testing.T) {
	g := githubStub(t, map[string]string{
		"repos/a/b/pulls/1": prJSON("head", "merge"),
		// GitHub may associate a workflow with the PR head while its job
		// checks reference the test-merge SHA. Correlate suites across both.
		"repos/a/b/actions/runs?head_sha=head&per_page=100":          `[{"total_count":1,"workflow_runs":[{"id":20,"workflow_id":10,"check_suite_id":100,"run_attempt":1,"name":"Optional workflow","event":"pull_request","head_sha":"head","status":"completed","conclusion":"failure"}]}]`,
		"repos/a/b/actions/runs?head_sha=merge&per_page=100":         noWorkflowRuns,
		"repos/a/b/commits/head/check-runs?filter=all&per_page=100":  `[{"total_count":0,"check_runs":[]}]`,
		"repos/a/b/commits/merge/check-runs?filter=all&per_page=100": `[{"total_count":1,"check_runs":[{"id":300,"name":"optional job","head_sha":"merge","status":"completed","conclusion":"failure","app":{"id":1,"slug":"github-actions"},"check_suite":{"id":100}}]}]`,
		"repos/a/b/commits/head/statuses?per_page=100":               `[[{"id":10,"context":"required build","state":"success"}]]`,
		"repos/a/b/commits/merge/statuses?per_page=100":              `[[]]`,
	})
	_, checks, err := g.Fetch(context.Background(), "a/b", 1)
	if err != nil || len(checks) != 2 {
		t.Fatalf("duplicated parent workflow: %+v %v", checks, err)
	}
	state, counts := (Policy{Global: []string{"optional job"}}).Evaluate("a/b", checks)
	if state != "PASS" || counts.Bypass != 1 {
		t.Fatalf("optional job still blocked: %s %+v %+v", state, counts, checks)
	}
	for _, c := range checks {
		if c.Name == "optional job" && (c.WorkflowRunID != 20 || c.Workflow != "Optional workflow" || c.HeadSHA != "merge") {
			t.Fatal(c)
		}
	}
}

func TestNewWorkflowAttemptCanReplaceOldFailure(t *testing.T) {
	checks := map[string]store.PRCheck{}
	runs := []apiWorkflowRun{
		{ID: 10, WorkflowID: 1, HeadSHA: "head", Event: "push", Status: "completed", Conclusion: "failure"},
		{ID: 10, WorkflowID: 1, HeadSHA: "head", Event: "push", Attempt: 2, Status: "completed", Conclusion: "success"},
	}
	addUnrepresentedWorkflows(checks, runs, "head", func(c store.PRCheck) { checks[c.Key] = c })
	if len(checks) != 1 {
		t.Fatal(checks)
	}
	for _, c := range checks {
		if c.Bucket != "pass" || c.RunAttempt != 2 {
			t.Fatal(c)
		}
	}
}
