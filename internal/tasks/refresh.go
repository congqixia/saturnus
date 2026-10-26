package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"saturnus/internal/store"
)

// RefreshResult separates external failures from archive suggestions.
// An external failure is recorded as a partial/failed result, not a fatal error.
type RefreshResult struct {
	TaskID      string         `json:"task_id"`
	Task        store.Task     `json:"task"`
	PRs         []store.TaskPR `json:"prs"`
	Outcome     string         `json:"outcome"`
	Errors      []string       `json:"errors"`
	Suggestion  string         `json:"suggestion"`
	PRSucceeded int            `json:"prs_succeeded"`
	PRFailed    int            `json:"prs_failed"`
}

type prResult struct {
	info      PRInfo
	fetchedAt time.Time
	fetchErr  error
	syncErr   error
}

type refreshScope map[string]prResult

func prKey(repo string, num int) string { return fmt.Sprintf("%s#%d", strings.ToLower(repo), num) }

func (s *Service) RefreshContext(ctx context.Context, id string) (RefreshResult, error) {
	return s.refresh(ctx, id, false, make(refreshScope))
}

// Sweep shares PR lookups and review effects across every selected task.
func (s *Service) Sweep(ctx context.Context, activeOnly bool) ([]RefreshResult, error) {
	if !s.cfg.Enabled {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopServerCancel := context.AfterFunc(s.cfg.Context, cancel)
	defer stopServerCancel()
	tasks, err := s.store.ListTasksForRefresh(ctx, activeOnly)
	if err != nil {
		return nil, err
	}
	scope := make(refreshScope)
	out := make([]RefreshResult, 0, len(tasks))
	for _, task := range tasks {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		result, err := s.refresh(ctx, task.ID, activeOnly, scope)
		if err != nil {
			result.TaskID = task.ID
			result.Outcome = "failed"
			if errors.Is(err, ErrArchived) || errors.Is(err, store.ErrNotFound) {
				result.Outcome = "skipped"
			}
			result.Errors = append(result.Errors, err.Error())
		}
		out = append(out, result)
	}
	return out, nil
}

func (s *Service) fetchPR(ctx context.Context, repo string, num int, scope refreshScope) prResult {
	key := prKey(repo, num)
	if cached, ok := scope[key]; ok {
		return cached
	}
	result := prResult{}
	unlock, err := s.locks.Lock(ctx, "pr:"+key)
	if err != nil {
		result.fetchErr = err
		return result
	}
	defer unlock()
	result.info, result.fetchErr = FetchPRInfo(ctx, s.cfg.GHCli, repo, num)
	if result.fetchErr == nil {
		result.fetchedAt = time.Now().UTC()
		if s.cfg.SyncReview != nil {
			result.syncErr = s.cfg.SyncReview(ctx, repo, num, result.info)
		}
	}
	// A per-task deadline should not poison shared results for later tasks.
	if ctx.Err() == nil {
		scope[key] = result
	}
	return result
}

func (s *Service) refresh(parent context.Context, id string, activeOnly bool, scope refreshScope) (RefreshResult, error) {
	result := RefreshResult{TaskID: id, Outcome: "success", Errors: []string{}, PRs: []store.TaskPR{}}
	if !s.cfg.Enabled {
		return result, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(parent, s.cfg.RefreshTimeout)
	defer cancel()
	stopServerCancel := context.AfterFunc(s.cfg.Context, cancel)
	defer stopServerCancel()
	unlock, err := s.locks.Lock(ctx, "task:"+id)
	if err != nil {
		return result, err
	}
	defer unlock()
	task, err := s.store.GetTask(id)
	if err != nil {
		return result, err
	}
	result.Task = task
	if task.Status == store.TaskStatusArchived {
		return result, ErrArchived
	}
	if activeOnly && task.Status != store.TaskStatusOpen && task.Status != store.TaskStatusInProgress {
		result.Outcome = "skipped"
		return result, nil
	}
	task.RefreshAttemptedAt = time.Now().UTC()
	successes := 0
	addError := func(stage string, err error) { result.Errors = append(result.Errors, stage+": "+err.Error()) }
	if _, ok := s.cfg.Repos[task.IssueRepo]; !ok {
		addError("issue", fmt.Errorf("repo is not in the task whitelist: %s", task.IssueRepo))
	} else if task.IssueNumber > 0 {
		info, err := FetchIssueInfo(ctx, s.cfg.GHCli, task.IssueRepo, task.IssueNumber)
		if err != nil {
			addError("issue", err)
		} else {
			task.IssueTitle, task.IssueState = info.Title, info.State
			if info.URL != "" {
				task.IssueURL = info.URL
			}
			successes++
		}
		if ctx.Err() == nil {
			linked, err := FetchLinkedPRs(ctx, s.cfg.GHCli, task.IssueRepo, task.IssueNumber)
			if err != nil {
				addError("timeline", err)
			} else {
				successes++
				existing, err := s.store.TaskPRsForRefresh(ctx, id)
				if err != nil {
					return result, err
				}
				seen := map[string]bool{}
				for _, pr := range existing {
					seen[prKey(pr.Repo, pr.PRNumber)] = true
				}
				for _, pr := range linked {
					if seen[prKey(task.IssueRepo, pr.Number)] {
						continue
					}
					// Metadata is fetched in the common PR loop, once per unique PR.
					_, err := s.store.AddTaskPRContext(ctx, store.TaskPR{TaskID: id, Repo: task.IssueRepo, PRNumber: pr.Number, PRURL: pr.URL, ReviewStatus: "none"}, activeOnly)
					if err != nil {
						addError("attach PR", err)
					}
				}
			}
		}
	}
	readCtx, readCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	prs, err := s.store.TaskPRsForRefresh(readCtx, id)
	readCancel()
	if err != nil {
		return result, err
	}
	// Resume partially completed tasks fairly on the next scan: untouched or
	// least recently attempted PRs come first, including failed lookups.
	sort.SliceStable(prs, func(i, j int) bool { return prs[i].SyncAttemptedAt.Before(prs[j].SyncAttemptedAt) })
	for i := range prs {
		if ctx.Err() != nil {
			break
		}
		pr := &prs[i]
		pr.SyncAttemptedAt = time.Now().UTC()
		var lookup prResult
		if _, ok := s.cfg.Repos[pr.Repo]; !ok {
			lookup.fetchErr = fmt.Errorf("repo is not in the task whitelist: %s", pr.Repo)
		} else {
			lookup = s.fetchPR(ctx, pr.Repo, pr.PRNumber, scope)
		}
		pr.SyncError = ""
		if lookup.fetchErr != nil {
			pr.SyncError = "metadata: " + lookup.fetchErr.Error()
		} else {
			// A concurrent manual refresh of a different task may have observed this
			// shared PR after our per-scan cache. Never regress the newer snapshot.
			if !lookup.fetchedAt.Before(pr.SyncedAt) {
				pr.PRTitle, pr.PRState, pr.BaseBranch = lookup.info.Title, lookup.info.State, lookup.info.BaseBranch
				if lookup.info.URL != "" {
					pr.PRURL = lookup.info.URL
				}
				pr.SyncedAt = lookup.fetchedAt
			}
			successes++
		}
		if lookup.syncErr != nil {
			pr.SyncError = joinNotes(pr.SyncError, "review: "+lookup.syncErr.Error())
		}
		if err := s.snapshotReview(pr, pr.Repo, pr.PRNumber); err != nil {
			pr.SyncError = joinNotes(pr.SyncError, "snapshot: "+err.Error())
		}
		if pr.SyncError != "" {
			addError(prKey(pr.Repo, pr.PRNumber), errors.New(pr.SyncError))
			result.PRFailed++
		} else {
			result.PRSucceeded++
		}
		// Persist cancellation diagnostics, but never perform more remote work.
		checkpoint, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		updated, err := s.store.UpdateTaskPRContext(checkpoint, *pr, activeOnly)
		stop()
		if err != nil {
			return result, err
		}
		prs[i] = updated
	}
	if ctx.Err() != nil {
		addError("refresh", ctx.Err())
	}
	result.PRs = prs
	result.Suggestion = CloseSuggestion(task, prs)
	if len(result.Errors) > 0 {
		result.Outcome = "partial"
		if successes == 0 {
			result.Outcome = "failed"
		}
		task.RefreshedAt = time.Time{} // COALESCE keeps the previous successful refresh.
	} else {
		task.RefreshedAt = time.Now().UTC()
	}
	task.RefreshError = strings.Join(result.Errors, "; ")
	checkpoint, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer stop()
	result.Task, err = s.store.SaveTaskRefresh(checkpoint, task, activeOnly)
	return result, err
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// CloseSuggestion is derived from snapshots and never changes feature task status.
func CloseSuggestion(task store.Task, prs []store.TaskPR) string {
	if task.Status == store.TaskStatusArchived {
		return ""
	}
	reason := ""
	if task.IssueState == "CLOSED" {
		reason = "issue is CLOSED"
	} else if len(prs) > 0 {
		allMerged := true
		for _, pr := range prs {
			if pr.PRState != "MERGED" {
				allMerged = false
			}
		}
		if allMerged {
			reason = "all PRs merged"
		}
	}
	if reason == "" {
		return ""
	}
	return fmt.Sprintf("Consider /sat task close %s (%s).", task.ID, reason)
}

// Start launches one scheduler. The returned stop function cancels and joins it.
func (s *Service) Start(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() { defer close(done); s.run(ctx, waitInterval) }()
	return func() { cancel(); <-done }
}

func waitInterval(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) run(ctx context.Context, wait func(context.Context, time.Duration) bool) {
	if !s.cfg.Enabled || s.cfg.RefreshInterval <= 0 {
		return
	}
	for ctx.Err() == nil {
		started := time.Now()
		results, err := s.Sweep(ctx, true)
		counts := map[string]int{}
		uniquePRs := map[string]bool{}
		for _, result := range results {
			counts[result.Outcome]++
			for _, pr := range result.PRs {
				uniquePRs[prKey(pr.Repo, pr.PRNumber)] = true
			}
			if len(result.Errors) > 0 {
				log.Printf("task refresh task=%s outcome=%s errors=%s", result.TaskID, result.Outcome, strings.Join(result.Errors, "; "))
			}
		}
		log.Printf("task refresh sweep tasks=%d unique_prs=%d outcomes=%v duration=%s error=%v", len(results), len(uniquePRs), counts, time.Since(started), err)
		if !wait(ctx, s.cfg.RefreshInterval) {
			return
		}
	}
}
