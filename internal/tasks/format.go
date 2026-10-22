package tasks

import (
	"fmt"
	"strings"
	"time"

	"saturnus/internal/store"
)

// FormatList renders the task list for /sat and fallback text replies.
func (s *Service) FormatList(tasks []store.Task) string {
	if len(tasks) == 0 {
		return "No tasks."
	}
	var b strings.Builder
	for i, task := range tasks {
		if i >= 10 {
			break
		}
		prCount := len(s.store.ListTaskPRs(task.ID))
		fmt.Fprintf(&b, "%s %s %s (%d PRs) %s\n", task.ID, task.Status, task.Title, prCount, task.CreatedAt.Format(time.RFC3339))
	}
	return strings.TrimSpace(b.String())
}

// FormatOne renders a task's full detail: issue, docs and per-PR progress.
func (s *Service) FormatOne(task store.Task, docs []store.TaskDoc, prs []store.TaskPR) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s status=%s\n%s", task.ID, task.Status, task.Title)
	if task.IssueURL != "" {
		fmt.Fprintf(&b, "\nissue=%s", task.IssueURL)
	}
	if task.IssueState != "" {
		fmt.Fprintf(&b, "\nissue_state=%s", task.IssueState)
	}
	if !task.ArchivedAt.IsZero() {
		fmt.Fprintf(&b, "\narchived_at=%s", task.ArchivedAt.Format(time.RFC3339))
	}
	if len(docs) > 0 {
		fmt.Fprintf(&b, "\ndocs:")
		for _, doc := range docs {
			fmt.Fprintf(&b, "\n  %s %s", doc.ID, FormatRepoPath(doc.Repo, doc.Path))
		}
	}
	if len(prs) > 0 {
		fmt.Fprintf(&b, "\nprs:")
		for _, pr := range prs {
			state := pr.PRState
			if state == "" {
				state = "unknown"
			}
			fmt.Fprintf(&b, "\n  %s %s#%d %s review=%s pr_state=%s", pr.ID, pr.Repo, pr.PRNumber, pr.PRTitle, pr.ReviewStatus, state)
			if pr.ReviewID != "" {
				fmt.Fprintf(&b, " review_id=%s", pr.ReviewID)
			}
			if pr.FsTaskID != "" {
				fmt.Fprintf(&b, " fs_task=%s", pr.FsTaskID)
			}
			if pr.FsTaskURL != "" {
				fmt.Fprintf(&b, " fs_task_url=%s", pr.FsTaskURL)
			}
		}
	}
	if task.Description != "" {
		fmt.Fprintf(&b, "\n\ndescription=%s", task.Description)
	}
	return strings.TrimSpace(b.String())
}

// FormatRefreshResult renders the outcome of refreshing one task.
func (s *Service) FormatRefreshResult(task store.Task, prs []store.TaskPR, suggestion string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Refreshed %s (%s) issue_state=%s prs=%d", task.ID, task.Title, task.IssueState, len(prs))
	for _, pr := range prs {
		fmt.Fprintf(&b, "\n  %s#%d %s review=%s", pr.Repo, pr.PRNumber, pr.PRState, pr.ReviewStatus)
	}
	if suggestion != "" {
		fmt.Fprintf(&b, "\n%s", suggestion)
	}
	return b.String()
}

// FormatRefreshAll renders the outcome of refreshing every non-archived task.
func (s *Service) FormatRefreshAll(refreshed []store.Task, notes map[string]string) string {
	var b strings.Builder
	if len(refreshed) == 0 {
		b.WriteString("No active tasks to refresh.")
	}
	for _, task := range refreshed {
		fmt.Fprintf(&b, "Refreshed %s (%s) issue_state=%s\n", task.ID, task.Title, task.IssueState)
	}
	for id, note := range notes {
		if note != "" {
			fmt.Fprintf(&b, "%s: %s\n", id, note)
		}
	}
	return strings.TrimSpace(b.String())
}
