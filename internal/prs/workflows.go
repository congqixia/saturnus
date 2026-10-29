package prs

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"saturnus/internal/store"
)

type apiWorkflowRun struct {
	ID           int64     `json:"id"`
	WorkflowID   int64     `json:"workflow_id"`
	CheckSuiteID int64     `json:"check_suite_id"`
	Attempt      int       `json:"run_attempt"`
	Name         string    `json:"name"`
	Event        string    `json:"event"`
	HeadSHA      string    `json:"head_sha"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	URL          string    `json:"html_url"`
	StartedAt    time.Time `json:"run_started_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (r apiWorkflowRun) key() string { return strconv.FormatInt(r.WorkflowID, 10) + ":" + r.Event }

func (g GitHub) workflowRuns(ctx context.Context, repo, sha string) ([]apiWorkflowRun, error) {
	var pages []struct {
		TotalCount *int             `json:"total_count"`
		Runs       []apiWorkflowRun `json:"workflow_runs"`
	}
	if err := g.api(ctx, "repos/"+repo+"/actions/runs?head_sha="+url.QueryEscape(sha)+"&per_page=100", true, &pages); err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("invalid workflow run pages")
	}
	out := []apiWorkflowRun{}
	total := 0
	for _, page := range pages {
		if page.TotalCount == nil || *page.TotalCount < 0 || page.Runs == nil {
			return nil, fmt.Errorf("incomplete workflow run page")
		}
		if *page.TotalCount > total {
			total = *page.TotalCount
		}
		for _, r := range page.Runs {
			if r.ID <= 0 || r.WorkflowID <= 0 || r.HeadSHA != sha || r.Event == "" {
				return nil, fmt.Errorf("invalid workflow run or mismatched commit")
			}
			if r.Name == "" {
				r.Name = "workflow " + strconv.FormatInt(r.WorkflowID, 10)
			}
			out = append(out, r)
		}
	}
	if len(out) < total {
		return nil, fmt.Errorf("incomplete workflow pagination: received %d of %d", len(out), total)
	}
	return out, nil
}

// Workflows with no jobs (e.g. startup failure) must remain visible. A parent
// conclusion already represented by jobs is not added a second time: doing so
// would reintroduce a failure from a deliberately bypassed optional job.
func addUnrepresentedWorkflows(checks map[string]store.PRCheck, runs []apiWorkflowRun, preferredSHA string, add func(store.PRCheck)) {
	represented := map[int64]map[string]bool{}
	for _, c := range checks {
		if c.WorkflowRunID > 0 {
			if represented[c.WorkflowRunID] == nil {
				represented[c.WorkflowRunID] = map[string]bool{}
			}
			represented[c.WorkflowRunID][c.Bucket] = true
		}
	}
	latest := map[string]apiWorkflowRun{}
	for _, r := range runs {
		old, ok := latest[r.key()]
		prefer := r.HeadSHA == preferredSHA
		oldPrefer := old.HeadSHA == preferredSHA
		if !ok || (prefer && !oldPrefer) || (prefer == oldPrefer && (r.ID > old.ID || (r.ID == old.ID && r.Attempt > old.Attempt))) {
			latest[r.key()] = r
		}
	}
	for _, r := range latest {
		bucket := checkBucket(r.Status, r.Conclusion)
		jobs := represented[r.ID]
		if len(jobs) > 0 && (bucket == "pass" || bucket == "skip" || jobs[bucket]) {
			continue
		}
		completed := time.Time{}
		if r.Status == "completed" {
			completed = r.UpdatedAt
		}
		add(store.PRCheck{Key: "workflow_run:" + r.key(), Source: "workflow_run", ExternalID: r.ID, App: "github-actions", Name: r.Name,
			Workflow: r.Name, WorkflowID: r.WorkflowID, WorkflowRunID: r.ID, RunAttempt: r.Attempt, Event: r.Event, HeadSHA: r.HeadSHA, Status: r.Status, Conclusion: r.Conclusion,
			Bucket: bucket, URL: r.URL, Description: "Workflow " + r.Name + " (" + r.Event + ")", StartedAt: r.StartedAt, CompletedAt: completed, UpdatedAt: r.UpdatedAt})
	}
}
