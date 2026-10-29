package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PullRequest is independent of task membership and Saturnus review execution.
// CheckedHeadSHA identifies the last complete CI snapshot; SyncError makes an
// unsuccessful observation explicit without discarding that snapshot.
type PullRequest struct {
	ID              string        `json:"id"`
	Repo            string        `json:"repo"`
	Number          int           `json:"number"`
	URL             string        `json:"url"`
	Title           string        `json:"title"`
	Author          string        `json:"author"`
	State           string        `json:"state"`
	Draft           bool          `json:"draft"`
	BaseBranch      string        `json:"base_branch"`
	HeadBranch      string        `json:"head_branch"`
	HeadRepo        string        `json:"head_repo"`
	HeadSHA         string        `json:"head_sha"`
	MergeSHA        string        `json:"merge_sha"`
	CheckedHeadSHA  string        `json:"checked_head_sha"`
	CheckedMergeSHA string        `json:"checked_merge_sha"`
	CIState         string        `json:"ci_state"`
	Monitoring      bool          `json:"monitoring"`
	Counts          PRCheckCounts `json:"counts"`
	SyncAttemptedAt time.Time     `json:"sync_attempted_at"`
	SyncedAt        time.Time     `json:"synced_at"`
	SyncError       string        `json:"sync_error"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

type PRCheckCounts struct {
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Pending int `json:"pending"`
	Cancel  int `json:"cancel"`
	Skip    int `json:"skip"`
	Unknown int `json:"unknown"`
	Bypass  int `json:"bypass"`
}

// PRCheck retains the upstream state even when a local policy bypasses it.
// Key identifies the logical check (source/app/name), not an old run attempt.
type PRCheck struct {
	Key           string    `json:"key"`
	Source        string    `json:"source"`
	ExternalID    int64     `json:"external_id"`
	App           string    `json:"app"`
	Workflow      string    `json:"workflow,omitempty"`
	WorkflowID    int64     `json:"workflow_id,omitempty"`
	WorkflowRunID int64     `json:"workflow_run_id,omitempty"`
	RunAttempt    int       `json:"run_attempt,omitempty"`
	Event         string    `json:"event,omitempty"`
	Name          string    `json:"name"`
	HeadSHA       string    `json:"head_sha"`
	Status        string    `json:"status"`
	Conclusion    string    `json:"conclusion"`
	Bucket        string    `json:"bucket"`
	URL           string    `json:"url"`
	Description   string    `json:"description"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Bypassed      bool      `json:"bypassed"`
	BypassRule    string    `json:"bypass_rule,omitempty"`
}

// Indexed identity/lifecycle columns support discovery; typed snapshots keep
// provider metadata extensible without adding a column for every CI vendor.
var pullRequestSchema = []string{
	`CREATE TABLE IF NOT EXISTS pull_requests (
 id TEXT PRIMARY KEY, repo TEXT NOT NULL COLLATE NOCASE, number INTEGER NOT NULL,
 state TEXT NOT NULL, updated_at TEXT NOT NULL, data TEXT NOT NULL, UNIQUE(repo,number))`,
	`CREATE INDEX IF NOT EXISTS idx_pull_requests_state ON pull_requests(state,updated_at)`,
	`CREATE TABLE IF NOT EXISTS pr_checks (
 pr_id TEXT NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
 check_key TEXT NOT NULL, data TEXT NOT NULL, PRIMARY KEY(pr_id,check_key))`,
}

func decodePullRequest(raw []byte, err error) (PullRequest, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequest{}, ErrNotFound
	}
	if err != nil {
		return PullRequest{}, err
	}
	var pr PullRequest
	err = json.Unmarshal(raw, &pr)
	return pr, err
}

func (s *Store) GetPullRequest(ctx context.Context, id string) (PullRequest, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM pull_requests WHERE id=?`, id).Scan(&raw)
	return decodePullRequest(raw, err)
}

func (s *Store) GetPullRequestByNumber(ctx context.Context, repo string, number int) (PullRequest, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM pull_requests WHERE repo=? AND number=?`, repo, number).Scan(&raw)
	return decodePullRequest(raw, err)
}

// TrackPullRequest is idempotent and never replaces an existing CI snapshot.
func (s *Store) TrackPullRequest(ctx context.Context, repo string, number int) (PullRequest, error) {
	if number <= 0 {
		return PullRequest{}, fmt.Errorf("invalid PR number")
	}
	now := time.Now().UTC()
	pr := PullRequest{ID: "pr_" + newID(), Repo: strings.ToLower(repo), Number: number, Monitoring: true,
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), State: "UNKNOWN", CIState: "UNKNOWN", CreatedAt: now, UpdatedAt: now}
	raw, err := json.Marshal(pr)
	if err != nil {
		return pr, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO pull_requests(id,repo,number,state,updated_at,data) VALUES(?,?,?,?,?,?)
 ON CONFLICT(repo,number) DO NOTHING`, pr.ID, pr.Repo, number, pr.State, timeText(now), string(raw))
	if err != nil {
		return PullRequest{}, err
	}
	return s.GetPullRequestByNumber(ctx, repo, number)
}

func (s *Store) ListPullRequests(ctx context.Context, all bool) ([]PullRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM pull_requests WHERE ? OR state IN ('OPEN','UNKNOWN') ORDER BY updated_at DESC,id`, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PullRequest{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		pr, err := decodePullRequest(raw, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

// SavePullRequestSnapshot publishes metadata/checks together. On sync failure
// replaceChecks=false preserves the last successful check details for diagnosis.
func (s *Store) SavePullRequestSnapshot(ctx context.Context, pr PullRequest, checks []PRCheck, replaceChecks bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	raw, err := json.Marshal(pr)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE pull_requests SET state=?,updated_at=?,data=? WHERE id=? AND repo=? AND number=?`, pr.State, timeText(pr.UpdatedAt), string(raw), pr.ID, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if replaceChecks {
		if _, err = tx.ExecContext(ctx, `DELETE FROM pr_checks WHERE pr_id=?`, pr.ID); err != nil {
			return err
		}
		for _, check := range checks {
			raw, err = json.Marshal(check)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO pr_checks(pr_id,check_key,data) VALUES(?,?,?)`, pr.ID, check.Key, string(raw)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) PullRequestChecks(ctx context.Context, id string) ([]PRCheck, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM pr_checks WHERE pr_id=? ORDER BY check_key`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PRCheck{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var c PRCheck
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type PullRequestRef struct {
	Repo   string
	Number int
}

// PullRequestSeeds joins existing features by their shared GitHub identity.
// Task/review rows remain compatible; each PR gets one independent monitor row.
func (s *Store) PullRequestSeeds(ctx context.Context) ([]PullRequestRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo,pr_number FROM review_requests WHERE status!='archived' AND pr_number>0
 UNION SELECT p.repo,p.pr_number FROM task_prs p JOIN tasks t ON t.id=p.task_id WHERE t.status!='archived' AND p.pr_number>0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PullRequestRef{}
	for rows.Next() {
		var r PullRequestRef
		if err := rows.Scan(&r.Repo, &r.Number); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) PullRequestTaskIDs(ctx context.Context, repo string, number int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT task_id FROM task_prs WHERE repo=? COLLATE NOCASE AND pr_number=? ORDER BY task_id`, repo, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) PullRequestReviewID(ctx context.Context, repo string, number int) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM review_requests WHERE repo=? COLLATE NOCASE AND pr_number=? ORDER BY created_at DESC LIMIT 1`, repo, number).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
