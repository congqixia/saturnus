package store

import (
	"context"
	"fmt"
	"time"
)

// SaveTaskRefresh updates remote snapshots without overwriting operator fields.
func (s *Store) SaveTaskRefresh(ctx context.Context, task Task, activeOnly bool) (Task, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET issue_title=?, issue_state=?, issue_url=?,
 refresh_attempted_at=?, refreshed_at=COALESCE(?,refreshed_at), refresh_error=?, updated_at=?
 WHERE id=? AND status!='archived' AND (?=0 OR status IN ('open','in_progress'))`,
		task.IssueTitle, task.IssueState, task.IssueURL, nullableTimeText(task.RefreshAttemptedAt), nullableTimeText(task.RefreshedAt), task.RefreshError, timeText(time.Now().UTC()), task.ID, activeOnly)
	if err != nil {
		return Task{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Task{}, err
	}
	if n == 0 {
		return Task{}, ErrNotFound
	}
	return s.GetTask(task.ID)
}

// PatchTask only updates operator-managed fields, preserving refresh snapshots.
func (s *Store) PatchTask(id string, title, status, description *string) (Task, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET title=COALESCE(?,title), status=COALESCE(?,status), description=COALESCE(?,description), updated_at=? WHERE id=?`, title, status, description, timeText(time.Now().UTC()), id)
	if err != nil {
		return Task{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Task{}, err
	}
	if n == 0 {
		return Task{}, ErrNotFound
	}
	if err := audit(ctx, tx, "system", "task.update", id, mustJSON(map[string]any{"title": title, "status": status, "description": description})); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.GetTask(id)
}

// SaveReviewSync uses optimistic concurrency and only owns synchronization fields.
// A retry/worker or another writer moving the record makes the caller defer.
func (s *Store) SaveReviewSync(ctx context.Context, before, after ReviewRequest) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE review_requests SET status=?, archived_at=?, pr_head_commit=?,
 title=?, base_branch=?, refreshed_at=?, task_synced_at=?, updated_at=?
 WHERE id=? AND status=? AND updated_at=?`,
		after.Status, nullableTimeText(after.ArchivedAt), after.PrHeadCommit, after.Title, after.BaseBranch,
		nullableTimeText(after.RefreshedAt), nullableTimeText(after.TaskSyncedAt), timeText(time.Now().UTC()),
		before.ID, before.Status, timeText(before.UpdatedAt))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) SetReviewTask(id, taskID, taskURL string) error {
	_, err := s.db.Exec(`UPDATE review_requests SET task_id=?, task_url=?, updated_at=? WHERE id=?`, taskID, taskURL, timeText(time.Now().UTC()), id)
	return err
}

func (s *Store) SetReviewRequester(id, openID, name string) error {
	_, err := s.db.Exec(`UPDATE review_requests SET requester_open_id=?, requester_name=?, updated_at=? WHERE id=? AND status!='archived'`, openID, name, timeText(time.Now().UTC()), id)
	return err
}

func (s *Store) SetReviewRequesterName(id, openID, name string) error {
	_, err := s.db.Exec(`UPDATE review_requests SET requester_name=?, updated_at=? WHERE id=? AND requester_open_id=?`, name, timeText(time.Now().UTC()), id, openID)
	return err
}

// ListTasksForRefresh reports database errors instead of treating them as an empty scan.
func (s *Store) ListTasksForRefresh(ctx context.Context, activeOnly bool) ([]Task, error) {
	query := `SELECT id, title, status, issue_repo, issue_number, issue_title, issue_state, issue_url,
 description, archived_at, created_at, updated_at, refresh_attempted_at, refreshed_at, refresh_error
 FROM tasks WHERE status!='archived'`
	if activeOnly {
		query += ` AND status IN ('open','in_progress')`
	}
	query += ` ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

func (s *Store) TaskPRsForRefresh(ctx context.Context, taskID string) ([]TaskPR, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, repo, pr_number, pr_url, pr_title, pr_state, base_branch,
 review_id, review_status, fs_task_id, fs_task_url, synced_at, created_at, updated_at, sync_attempted_at, sync_error
 FROM task_prs WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskPR{}
	for rows.Next() {
		pr, err := scanTaskPR(rows)
		if err != nil {
			return nil, fmt.Errorf("scan PR: %w", err)
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}
