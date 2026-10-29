package prs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"saturnus/internal/store"
)

type Provider interface {
	CurrentUser(context.Context) (string, error)
	Discover(context.Context, string, string) ([]int, error)
	Fetch(context.Context, string, int) (store.PullRequest, []store.PRCheck, error)
}

// GitHub uses gh for credentials and stable REST endpoints for data. A failed
// CI conclusion is an HTTP 200 response, not a subprocess failure.
type GitHub struct{ CLI string }

type boundedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	const max = 32 * 1024 * 1024
	n := len(p)
	left := max - b.Len()
	if left < n {
		b.overflow = true
		p = p[:left]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (g GitHub) api(ctx context.Context, endpoint string, paginate bool, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cli := g.CLI
	if cli == "" {
		cli = "gh"
	}
	args := []string{"api", "--method", "GET", endpoint}
	if paginate {
		args = append(args, "--paginate", "--slurp")
	}
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.WaitDelay = time.Second
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("GitHub API %s: %w", endpoint, err)
	}
	if output.overflow {
		return fmt.Errorf("GitHub response for %s exceeds 32 MiB", endpoint)
	}
	if err = json.Unmarshal(output.Bytes(), out); err != nil {
		return fmt.Errorf("invalid GitHub response for %s: %w", endpoint, err)
	}
	return nil
}

func (g GitHub) CurrentUser(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := g.api(ctx, "user", false, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", fmt.Errorf("GitHub user response has no login")
	}
	return u.Login, nil
}

type apiPR struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"html_url"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	Merged    bool      `json:"merged"`
	MergeSHA  string    `json:"merge_commit_sha"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (g GitHub) Discover(ctx context.Context, repo, author string) ([]int, error) {
	var pages [][]apiPR
	if err := g.api(ctx, "repos/"+repo+"/pulls?state=open&per_page=100", true, &pages); err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("invalid pull request pages")
	}
	numbers := []int{}
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("invalid pull request page")
		}
		for _, pr := range page {
			if pr.Number <= 0 || pr.User.Login == "" {
				return nil, fmt.Errorf("invalid discovered PR")
			}
			if strings.EqualFold(pr.User.Login, author) {
				numbers = append(numbers, pr.Number)
			}
		}
	}
	return numbers, nil
}

func (g GitHub) metadata(ctx context.Context, repo string, number int) (store.PullRequest, error) {
	var raw apiPR
	if err := g.api(ctx, fmt.Sprintf("repos/%s/pulls/%d", repo, number), false, &raw); err != nil {
		return store.PullRequest{}, err
	}
	if raw.Number != number || raw.Head.SHA == "" || (raw.State != "open" && raw.State != "closed") {
		return store.PullRequest{}, fmt.Errorf("invalid PR metadata for %s#%d", repo, number)
	}
	state := strings.ToUpper(raw.State)
	if raw.Merged {
		state = "MERGED"
	}
	mergeSHA := ""
	if state == "OPEN" {
		mergeSHA = raw.MergeSHA
	}
	return store.PullRequest{Repo: repo, Number: number, URL: raw.URL, Title: raw.Title, Author: raw.User.Login, State: state, Draft: raw.Draft,
		BaseBranch: raw.Base.Ref, HeadBranch: raw.Head.Ref, HeadRepo: raw.Head.Repo.FullName, HeadSHA: raw.Head.SHA, MergeSHA: mergeSHA, CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt}, nil
}

type apiCheck struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	HeadSHA     string    `json:"head_sha"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	URL         string    `json:"details_url"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	App         struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Summary string `json:"summary"`
	} `json:"output"`
	CheckSuite struct {
		ID int64 `json:"id"`
	} `json:"check_suite"`
}

type apiStatus struct {
	ID          int64     `json:"id"`
	Context     string    `json:"context"`
	State       string    `json:"state"`
	URL         string    `json:"target_url"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func checkBucket(status, conclusion string) string {
	if status != "completed" {
		switch status {
		case "queued", "in_progress", "waiting", "requested", "pending":
			return "pending"
		default:
			return "unknown"
		}
	}
	switch conclusion {
	case "success":
		return "pass"
	case "failure", "timed_out", "action_required", "startup_failure", "stale", "error":
		return "fail"
	case "cancelled":
		return "cancel"
	case "skipped", "neutral":
		return "skip"
	default:
		return "unknown"
	}
}

func (g GitHub) checks(ctx context.Context, repo, sha string, runs []apiWorkflowRun) (map[string]store.PRCheck, error) {
	var pages []struct {
		TotalCount *int       `json:"total_count"`
		Checks     []apiCheck `json:"check_runs"`
	}
	base := "repos/" + repo + "/commits/" + url.PathEscape(sha)
	if err := g.api(ctx, base+"/check-runs?filter=all&per_page=100", true, &pages); err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("invalid check run pages")
	}
	checks := map[string]store.PRCheck{}
	add := func(c store.PRCheck) {
		old, ok := checks[c.Key]
		if !ok || c.ExternalID > old.ExternalID || (c.ExternalID == old.ExternalID && c.UpdatedAt.After(old.UpdatedAt)) {
			checks[c.Key] = c
		}
	}
	bySuite := map[int64]apiWorkflowRun{}
	latestRun := map[string]int64{}
	for _, r := range runs {
		key := r.HeadSHA + ":" + r.key()
		if r.ID > latestRun[key] {
			latestRun[key] = r.ID
		}
		if old, ok := bySuite[r.CheckSuiteID]; !ok || r.Attempt > old.Attempt {
			bySuite[r.CheckSuiteID] = r
		}
	}
	total, received := 0, 0
	for _, page := range pages {
		if page.TotalCount == nil || *page.TotalCount < 0 || page.Checks == nil {
			return nil, fmt.Errorf("incomplete check run page")
		}
		if *page.TotalCount > total {
			total = *page.TotalCount
		}
		received += len(page.Checks)
		for _, c := range page.Checks {
			if c.ID <= 0 || c.Name == "" || (c.HeadSHA != "" && c.HeadSHA != sha) {
				return nil, fmt.Errorf("invalid check run or mismatched commit")
			}
			updated := c.CompletedAt
			if updated.IsZero() {
				updated = c.StartedAt
			}
			check := store.PRCheck{Key: "check_run:" + strconv.FormatInt(c.App.ID, 10) + ":" + c.Name, Source: "check_run", ExternalID: c.ID,
				App: c.App.Slug, Name: c.Name, HeadSHA: sha, Status: c.Status, Conclusion: c.Conclusion, Bucket: checkBucket(c.Status, c.Conclusion), URL: c.URL,
				Description: limitText(c.Output.Summary, 2000), StartedAt: c.StartedAt, CompletedAt: c.CompletedAt, UpdatedAt: updated}
			if c.App.Slug == "github-actions" && c.CheckSuite.ID > 0 {
				if r, ok := bySuite[c.CheckSuite.ID]; ok {
					// A newer workflow execution replaces all of its old jobs,
					// including matrix entries removed from the new execution.
					if r.ID != latestRun[r.HeadSHA+":"+r.key()] {
						continue
					}
					check.Workflow, check.WorkflowID, check.WorkflowRunID, check.RunAttempt, check.Event = r.Name, r.WorkflowID, r.ID, r.Attempt, r.Event
					check.Key = "check_run:" + strconv.FormatInt(c.App.ID, 10) + ":" + r.key() + ":" + c.Name
				} else {
					check.Key = "check_run:" + strconv.FormatInt(c.App.ID, 10) + ":suite:" + strconv.FormatInt(c.CheckSuite.ID, 10) + ":" + c.Name
				}
			}
			add(check)
		}
	}
	if received < total {
		return nil, fmt.Errorf("incomplete check run pagination: received %d of %d", received, total)
	}
	var statusPages [][]apiStatus
	if err := g.api(ctx, base+"/statuses?per_page=100", true, &statusPages); err != nil {
		return nil, err
	}
	if len(statusPages) == 0 {
		return nil, fmt.Errorf("invalid commit status pages")
	}
	for _, page := range statusPages {
		if page == nil {
			return nil, fmt.Errorf("invalid commit status page")
		}
		for _, c := range page {
			if c.ID <= 0 || c.Context == "" {
				return nil, fmt.Errorf("invalid commit status")
			}
			status, conclusion := "completed", c.State
			if c.State == "pending" {
				status = "pending"
				conclusion = ""
			}
			add(store.PRCheck{Key: "commit_status:" + c.Context, Source: "commit_status", ExternalID: c.ID, Name: c.Context, HeadSHA: sha,
				Status: status, Conclusion: conclusion, Bucket: checkBucket(status, conclusion), URL: c.URL, Description: c.Description, StartedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
		}
	}
	return checks, nil
}

func (g GitHub) Fetch(ctx context.Context, repo string, number int) (store.PullRequest, []store.PRCheck, error) {
	pr, err := g.metadata(ctx, repo, number)
	if err != nil {
		return pr, nil, err
	}
	runs, err := g.workflowRuns(ctx, repo, pr.HeadSHA)
	if err != nil {
		return pr, nil, err
	}
	if pr.MergeSHA != "" && pr.MergeSHA != pr.HeadSHA {
		mergeRuns, err := g.workflowRuns(ctx, repo, pr.MergeSHA)
		if err != nil {
			return pr, nil, err
		}
		runs = append(runs, mergeRuns...)
	}
	checks, err := g.checks(ctx, repo, pr.HeadSHA, runs)
	if err != nil {
		return pr, nil, err
	}
	if pr.MergeSHA != "" && pr.MergeSHA != pr.HeadSHA {
		mergeChecks, err := g.checks(ctx, repo, pr.MergeSHA, runs)
		if err != nil {
			return pr, nil, err
		}
		// Prefer the active test-merge result for the same logical check,
		// retaining head-only checks and the actual tested SHA on each item.
		for key, c := range mergeChecks {
			checks[key] = c
		}
	}
	preferred := pr.HeadSHA
	if pr.MergeSHA != "" {
		preferred = pr.MergeSHA
	}
	addUnrepresentedWorkflows(checks, runs, preferred, func(c store.PRCheck) { checks[c.Key] = c })
	latest, err := g.metadata(ctx, repo, number)
	if err != nil {
		return pr, nil, err
	}
	if latest.HeadSHA != pr.HeadSHA || latest.MergeSHA != pr.MergeSHA || latest.State != pr.State {
		return latest, nil, fmt.Errorf("PR head or merge target changed while collecting CI; refresh again")
	}
	out := make([]store.PRCheck, 0, len(checks))
	for _, c := range checks {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return latest, out, nil
}

func limitText(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
